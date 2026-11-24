package gateway_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/gateway"
	"github.com/myjnathan/agentgate/internal/onboard"
	"github.com/myjnathan/agentgate/internal/store"
)

func TestLaunchPhaseDefaultIsBlock(t *testing.T) {
	gw := gateway.New(nil, nil, nil, nil, gateway.Policy{})
	if gw.LaunchPhase() != onboard.PhaseBlock {
		t.Fatalf("zero Policy must default to block, got %q", gw.LaunchPhase())
	}
	gw = gateway.New(nil, nil, nil, nil, gateway.Policy{DryRun: true})
	if gw.LaunchPhase() != onboard.PhaseObserve {
		t.Fatalf("legacy DryRun=true must map to observe, got %q", gw.LaunchPhase())
	}
	gw = gateway.New(nil, nil, nil, nil, gateway.Policy{LaunchPhase: "nonsense"})
	if gw.LaunchPhase() != onboard.PhaseBlock {
		t.Fatalf("invalid phase must fall back to block, got %q", gw.LaunchPhase())
	}
	gw.SetLaunchPhase("nonsense")
	if gw.LaunchPhase() != onboard.PhaseBlock {
		t.Fatalf("SetLaunchPhase must ignore invalid values, got %q", gw.LaunchPhase())
	}
	gw.SetLaunchPhase(onboard.PhaseWarn)
	if gw.LaunchPhase() != onboard.PhaseWarn {
		t.Fatalf("SetLaunchPhase(warn) did not stick, got %q", gw.LaunchPhase())
	}
}

func TestLaunchPhaseObserveNeverBlocks(t *testing.T) {
	e := setup(t)
	gw := e.gw
	gw.SetLaunchPhase(onboard.PhaseObserve)
	// Banned word: allowed in observe, and the audit decision is
	// prefixed dry_run:.
	status, body := e.chat(t, e.byoaTok, "mock-pro", "包含 test_sensitive 内容")
	if status != http.StatusOK {
		t.Fatalf("observe must not block banned words: %d %s", status, body)
	}
	evs := e.al.All()
	found := false
	for _, ev := range evs {
		if ev.Kind == "model_call" && len(ev.Decision) >= 8 && ev.Decision[:8] == "dry_run:" {
			found = true
		}
	}
	if !found {
		t.Fatalf("observe call must be audited with a dry_run: decision, events: %+v", evs)
	}
	// BYOA confidential: allowed in observe as well.
	status, body = e.chat(t, e.byoaTok, "mock-pro", "XH-200 的进度如何")
	if status != http.StatusOK {
		t.Fatalf("observe must not block BYOA confidential: %d %s", status, body)
	}
}

func TestLaunchPhaseWarnWaivesTierRuleOnly(t *testing.T) {
	e := setup(t)
	gw := e.gw
	gw.SetLaunchPhase(onboard.PhaseWarn)
	// BYOA confidential: allowed, decision "warn", and the reason
	// is recorded — but the data still only reaches a compliant
	// channel (mock-pro on the signed-cloud desensitized channel),
	// so the upstream must only ever see the placeholder.
	status, body := e.chat(t, e.byoaTok, "mock-pro", "XH-200 的进度如何")
	if status != http.StatusOK {
		t.Fatalf("warn must let the BYOA confidential call through: %d %s", status, body)
	}
	if got := lastModelCallDecision(t, e.al); got != "warn" {
		t.Fatalf("warn-phase call decision = %q, want warn", got)
	}
	evs := e.al.All()
	warned := false
	for _, ev := range evs {
		if ev.Kind == "model_call" && ev.Decision == "warn" && ev.Detail["warn_reason"] == "byoa_tier_would_block" {
			warned = true
		}
	}
	if !warned {
		t.Fatal("warn-phase call must carry warn_reason=byoa_tier_would_block in audit")
	}
	// Banned words still block in warn phase: it is a safety layer,
	// not a rollout gate.
	status, body = e.chat(t, e.byoaTok, "mock-pro", "包含 test_sensitive 内容")
	if status != http.StatusBadRequest {
		t.Fatalf("warn must still block banned words: %d %s", status, body)
	}
	// Back to block: the same BYOA confidential call is refused.
	gw.SetLaunchPhase(onboard.PhaseBlock)
	status, body = e.chat(t, e.byoaTok, "mock-pro", "XH-200 的进度如何")
	if status != http.StatusForbidden {
		t.Fatalf("block must refuse BYOA confidential: %d %s", status, body)
	}
}

func lastModelCallDecision(t *testing.T, al *audit.Log) string {
	t.Helper()
	evs := al.All()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == "model_call" {
			return evs[i].Decision
		}
	}
	t.Fatal("no model_call event recorded")
	return ""
}

func TestLaunchPhaseRedactForTierRuntime(t *testing.T) {
	e := setup(t)
	gw := e.gw
	// BYOA is not redacted by default: a PII finding in an internal
	// call goes to the mock upstream as-is (decision "warn").
	status, body := e.chat(t, e.byoaTok, "mock-eco", "我的电话是 138-0000-0000")
	if status != http.StatusOK {
		t.Fatalf("baseline BYOA call failed: %d %s", status, body)
	}
	if got := lastModelCallDecision(t, e.al); got != "warn" {
		t.Fatalf("baseline BYOA decision = %q, want warn (unredacted)", got)
	}
	// The conservative preset redacts both tiers at runtime.
	gw.SetRedactForTier(map[store.Tier]bool{store.TierManaged: true, store.TierBYOA: true})
	status, body = e.chat(t, e.byoaTok, "mock-eco", "我的电话是 138-0000-0001")
	if status != http.StatusOK {
		t.Fatalf("redacted BYOA call failed: %d %s", status, body)
	}
	if got := lastModelCallDecision(t, e.al); got != "redact" {
		t.Fatalf("redacted BYOA decision = %q, want redact", got)
	}
	// The response must restore the original number (round trip).
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Choices) == 0 {
		t.Fatalf("bad response body: %s (%v)", body, err)
	}
	if !strings.Contains(out.Choices[0].Message.Content, "138-0000-0001") {
		t.Fatalf("response must restore the original phone number, got %q", out.Choices[0].Message.Content)
	}
}

func TestLaunchPhaseSetWordsRuntime(t *testing.T) {
	e := setup(t)
	gw := e.gw
	// Before the template: the medical term is just prose.
	status, _ := e.chat(t, e.byoaTok, "mock-pro", "注册证号的办理进度")
	if status != http.StatusOK {
		t.Fatalf("baseline call failed: %d", status)
	}
	if got := lastModelCallDecision(t, e.al); got != "allow" {
		t.Fatalf("baseline decision = %q, want allow (word not yet a custom word)", got)
	}
	// Apply the medical-device template's words at runtime.
	banned, custom := gw.Words()
	gw.SetWords(banned, append(custom, "注册证号"))
	status, body := e.chat(t, e.byoaTok, "mock-pro", "注册证号的办理进度")
	if status != http.StatusForbidden {
		t.Fatalf("after SetWords the term must classify confidential and block BYOA: %d %s", status, body)
	}
	// And Words() reports the applied list back.
	_, custom2 := gw.Words()
	found := false
	for _, w := range custom2 {
		if w == "注册证号" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Words() must include the applied word, got %v", custom2)
	}
}
