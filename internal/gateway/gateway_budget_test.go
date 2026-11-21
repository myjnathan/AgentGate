package gateway_test

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// setBudget replaces (or adds) a department budget in the test env's
// store. The gateway reads budgets from the store on every call, so
// no restart is needed.
func setBudget(t *testing.T, e *testEnv, dept string, capUSD, threshold float64) {
	t.Helper()
	e.st.WithLock(func(st *store.State) {
		for _, b := range st.Budgets {
			if b.Department == dept {
				b.MonthlyCapUSD = capUSD
				b.AlertThreshold = threshold
				return
			}
		}
		st.Budgets["bud_test_"+dept] = &store.DepartmentBudget{
			ID: "bud_test_" + dept, Department: dept,
			MonthlyCapUSD: capUSD, AlertThreshold: threshold}
	})
	if err := e.st.Save(); err != nil {
		t.Fatal(err)
	}
}

func lastModelCall(events []audit.Event) (audit.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == "model_call" {
			return events[i], true
		}
	}
	return audit.Event{}, false
}

func countEvents(events []audit.Event, kind, action string) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == kind && ev.Action == action {
			n++
		}
	}
	return n
}

// TestModelCallRecordsCost: a priced call records its USD cost on
// the audit event. Mock usage for content "你好" is 10 prompt tokens
// (2 runes + 8) and 24 completion tokens ("[mock-model] 我收到了你
// 的消息：你好"), priced at mock-eco $0.50/$1.50 per million:
// 10*0.5 + 24*1.5 = 41 micro-dollars.
func TestModelCallRecordsCost(t *testing.T) {
	e := setup(t)
	code, _ := e.chat(t, e.byoaTok, "mock-eco", "你好")
	if code != 200 {
		t.Fatalf("chat: got %d, want 200", code)
	}
	ev, ok := lastModelCall(e.al.All())
	if !ok {
		t.Fatal("no model_call event recorded")
	}
	if ev.Tokens != 34 {
		t.Fatalf("tokens = %d, want 34", ev.Tokens)
	}
	if math.Abs(ev.CostUSD-0.000041) > 1e-9 {
		t.Fatalf("cost = %v, want 0.000041", ev.CostUSD)
	}
	if ev.Channel == "" || ev.Model != "mock-eco" {
		t.Fatalf("event channel/model = %q/%q, want priced channel/mock-eco", ev.Channel, ev.Model)
	}
}

// TestStreamingCallRecordsCost: streamed calls report usage too
// (F10 fixed the extraction), so they are priced like any call.
func TestStreamingCallRecordsCost(t *testing.T) {
	e := setup(t)
	body, _ := json.Marshal(map[string]any{
		"model": "mock-eco", "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "你好"}},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.mgdTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(buf.String(), "data: [DONE]") {
		t.Fatalf("stream: got %d %q, want 200 ending with [DONE]", resp.StatusCode, buf.String())
	}
	ev, ok := lastModelCall(e.al.All())
	if !ok {
		t.Fatal("no model_call event recorded")
	}
	if ev.Tokens != 34 || ev.CostUSD <= 0 {
		t.Fatalf("streamed call tokens/cost = %d/%v, want 34/>0", ev.Tokens, ev.CostUSD)
	}
}

// TestDepartmentBudgetBlocksNextCall: once a department's spend this
// month reaches its cap, the next call is rejected with 429 and an
// audit entry — the call already in flight is never the one blocked.
// Other departments are unaffected.
func TestDepartmentBudgetBlocksNextCall(t *testing.T) {
	e := setup(t)
	// One mock-eco "你好" costs $0.000041; a $0.00001 cap is crossed
	// by the first call, so the second must be refused.
	setBudget(t, e, "研发部", 0.00001, 0)
	code, _ := e.chat(t, e.byoaTok, "mock-eco", "你好")
	if code != 200 {
		t.Fatalf("first call: got %d, want 200", code)
	}
	code, body := e.chat(t, e.byoaTok, "mock-eco", "你好")
	if code != 429 {
		t.Fatalf("over-budget call: got %d %s, want 429", code, body)
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &errBody)
	if errBody.Error.Code != "department_budget_exceeded" {
		t.Fatalf("error code = %q, want department_budget_exceeded", errBody.Error.Code)
	}
	// The block is audited as a violation, so F2 stats count it.
	blocked := 0
	for _, ev := range e.al.All() {
		if ev.Kind == "violation" && ev.Action == "model_call_blocked" &&
			ev.Detail["reason"] == "department_budget_exceeded" {
			blocked++
		}
	}
	if blocked != 1 {
		t.Fatalf("budget blocks audited = %d, want 1", blocked)
	}
	// 注册部 has no budget: the managed token is unaffected.
	code, _ = e.chat(t, e.mgdTok, "mock-eco", "你好")
	if code != 200 {
		t.Fatalf("other department call: got %d, want 200", code)
	}
}

// TestBudgetThresholdWarnsOnce: crossing the alert threshold writes
// exactly one budget_threshold event per month, however many calls
// follow.
func TestBudgetThresholdWarnsOnce(t *testing.T) {
	e := setup(t)
	// Cap $0.0001, threshold 50%: one "你好" ($0.000041) stays under,
	// the second ($0.000043) crosses, the third must not re-warn.
	// Each call uses different content: identical calls would be
	// served from the F11 response cache and never reach the
	// upstream, so they would add no spend at all.
	setBudget(t, e, "研发部", 0.0001, 0.5)
	for i := 0; i < 3; i++ {
		code, _ := e.chat(t, e.byoaTok, "mock-eco", "你好"+string(rune('1'+i)))
		if code != 200 {
			t.Fatalf("call %d: got %d, want 200", i+1, code)
		}
	}
	n := countEvents(e.al.All(), "budget", "budget_threshold")
	if n != 1 {
		t.Fatalf("budget_threshold events = %d, want exactly 1", n)
	}
}

// TestUnpricedModelRecordsZeroCost: a channel with no price for the
// model still serves the call, but the event carries no cost — stats
// report it as unpriced rather than free.
func TestUnpricedModelRecordsZeroCost(t *testing.T) {
	e := setup(t)
	e.st.WithLock(func(st *store.State) {
		st.Channels["ch_cloud"].Pricing = nil
		st.Channels["ch_cloud_conf"].Pricing = nil
	})
	code, _ := e.chat(t, e.byoaTok, "mock-eco", "你好")
	if code != 200 {
		t.Fatalf("chat: got %d, want 200", code)
	}
	ev, ok := lastModelCall(e.al.All())
	if !ok {
		t.Fatal("no model_call event recorded")
	}
	if ev.Tokens != 34 {
		t.Fatalf("tokens = %d, want 34", ev.Tokens)
	}
	if ev.CostUSD != 0 {
		t.Fatalf("unpriced cost = %v, want 0", ev.CostUSD)
	}
}
