package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/exception"
	"github.com/myjnathan/agentgate/internal/store"
)

// chatWithGrant is chatWithLevel plus the one-time exception grant
// header (backlog F24).
func chatWithGrant(e *testEnv, token, model, content, level, grant string) (int, string) {
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": content}},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if level != "" {
		req.Header.Set("X-AgentGate-Data-Level", level)
	}
	if grant != "" {
		req.Header.Set("X-AgentGate-Exception-Grant", grant)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// grantFor inserts an approved grant for username at level and
// returns its plaintext token, as the approve endpoint would.
func grantFor(e *testEnv, username string, level store.DataLevel) string {
	var uid string
	e.st.Read(func(s *store.State) {
		for _, u := range s.Users {
			if u.Username == username {
				uid = u.ID
			}
		}
	})
	token, hash := exception.NewGrant()
	now := time.Now()
	exp := now.Add(time.Hour)
	e.st.WithLock(func(s *store.State) {
		s.Exceptions["exc_test"] = &store.Exception{ID: "exc_test", ApplicantID: uid,
			Applicant: username, DataLevel: level, Status: exception.StatusApproved,
			GrantHash: hash, DecidedAt: &now, ExpiresAt: &exp}
	})
	return token
}

func grantStatus(e *testEnv) string {
	var status string
	e.st.Read(func(s *store.State) {
		if rec, ok := s.Exceptions["exc_test"]; ok {
			status = rec.Status
		}
	})
	return status
}

func TestExceptionGrantWaivesBYOABlock(t *testing.T) {
	e, up := setupTrust(t, trustPolicy())
	// Baseline: a BYOA token may not send confidential data.
	code, body := e.chat(t, e.byoaTok, "mock-pro", "请分析 XH-200 项目的进展")
	if code != 403 || !strings.Contains(body, "data_level_forbidden") {
		t.Fatalf("BYOA confidential without grant: got %d %s, want 403 data_level_forbidden", code, body)
	}
	grant := grantFor(e, "lixiaofeng", store.LevelConfidential)
	code, body = chatWithGrant(e, e.byoaTok, "mock-pro", "请分析 XH-200 项目的进展", "", grant)
	if code != 200 {
		t.Fatalf("BYOA confidential with grant: got %d %s, want 200", code, body)
	}
	// The waiver lifts the tier block, not the desensitization: the
	// signed cloud still only sees placeholders.
	if sent := up.body("desens"); strings.Contains(sent, "XH-200") {
		t.Fatalf("signed cloud received original confidential text under a grant: %s", sent)
	}
	if grantStatus(e) != exception.StatusUsed {
		t.Fatalf("grant must be consumed by the waived call, status is %q", grantStatus(e))
	}
	// The same token a second time: the grant is spent, so the call
	// fails closed with the explicit invalid-grant error.
	code, body = chatWithGrant(e, e.byoaTok, "mock-pro", "请分析 XH-200 项目的进展", "", grant)
	if code != 403 || !strings.Contains(body, "exception_grant_invalid") {
		t.Fatalf("reused grant: got %d %s, want 403 exception_grant_invalid", code, body)
	}
	// Audit: one grant use, and the waived call traces to its grant.
	used, traced := false, false
	for _, ev := range e.al.All() {
		if ev.Kind == "exception" && ev.Action == "exception_grant_used" && ev.Detail["exception_id"] == "exc_test" {
			used = true
		}
		if ev.Kind == "model_call" && ev.Detail["exception_id"] == "exc_test" {
			traced = true
		}
	}
	if !used {
		t.Fatal("audit should record exception_grant_used")
	}
	if !traced {
		t.Fatal("the waived model_call should carry the exception_id")
	}
}

func TestExceptionGrantWaivesNoCompliantChannel(t *testing.T) {
	e, up := setupTrust(t, trustPolicy())
	e.st.WithLock(func(s *store.State) {
		s.Channels["ch_local"].Disabled = true
	})
	// Baseline: secret data with no local model is blocked.
	code, body := chatWithLevel(e, e.mgdTok, "mock-pro", "普通内容", "secret")
	if code != 403 || !strings.Contains(body, "no_compliant_channel") {
		t.Fatalf("secret call without local model: got %d %s, want 403 no_compliant_channel", code, body)
	}
	grant := grantFor(e, "chenjing", store.LevelSecret)
	code, body = chatWithGrant(e, e.mgdTok, "mock-pro", "普通内容", "secret", grant)
	if code != 200 {
		t.Fatalf("secret call with grant: got %d %s, want 200", code, body)
	}
	// The waiver routes to the least-privileged channel that may
	// receive the level at all — here the dedicated cloud — and, as
	// approved, the text goes as-is (a dedicated cloud is not a
	// signed cloud, so no forced desensitization applies).
	if got := up.hitCount("vpc"); got != 1 {
		t.Fatalf("expected dedicated cloud hit once, got %d", got)
	}
	if sent := up.body("vpc"); !strings.Contains(sent, "普通内容") {
		t.Fatalf("dedicated cloud should receive the waived text, got %s", sent)
	}
	if grantStatus(e) != exception.StatusUsed {
		t.Fatalf("grant must be consumed, status is %q", grantStatus(e))
	}
}

func TestExceptionGrantInvalidRejected(t *testing.T) {
	e, _ := setupTrust(t, trustPolicy())
	// A made-up token fails closed with the explicit error, not the
	// generic tier block.
	code, body := chatWithGrant(e, e.byoaTok, "mock-pro", "请分析 XH-200 项目的进展", "", "exc_deadbeef")
	if code != 403 || !strings.Contains(body, "exception_grant_invalid") {
		t.Fatalf("garbage grant: got %d %s, want 403 exception_grant_invalid", code, body)
	}
	// Another employee's grant cannot be spent by this caller.
	grant := grantFor(e, "chenjing", store.LevelConfidential)
	code, body = chatWithGrant(e, e.byoaTok, "mock-pro", "请分析 XH-200 项目的进展", "", grant)
	if code != 403 || !strings.Contains(body, "exception_grant_invalid") {
		t.Fatalf("someone else's grant: got %d %s, want 403 exception_grant_invalid", code, body)
	}
	// A confidential grant never waives a secret-level call.
	grant = grantFor(e, "chenjing", store.LevelConfidential)
	e.st.WithLock(func(s *store.State) {
		s.Channels["ch_local"].Disabled = true
	})
	code, body = chatWithGrant(e, e.mgdTok, "mock-pro", "普通内容", "secret", grant)
	if code != 403 || !strings.Contains(body, "exception_grant_invalid") {
		t.Fatalf("confidential grant on secret call: got %d %s, want 403 exception_grant_invalid", code, body)
	}
	found := false
	for _, ev := range e.al.All() {
		if ev.Kind == "violation" && ev.Decision == "block" && ev.Detail["reason"] == "exception_grant_invalid" {
			found = true
		}
	}
	if !found {
		t.Fatal("audit should record invalid-grant blocks with reason exception_grant_invalid")
	}
}

func TestExceptionGrantDoesNotWaiveBannedWords(t *testing.T) {
	e, up := setupTrust(t, trustPolicy())
	grant := grantFor(e, "lixiaofeng", store.LevelConfidential)
	code, body := chatWithGrant(e, e.byoaTok, "mock-eco", "包含 test_sensitive 内容", "", grant)
	if code != 400 || !strings.Contains(body, "request_rejected") {
		t.Fatalf("banned word with grant: got %d %s, want 400 request_rejected", code, body)
	}
	if up.hitCount("cloud") != 0 {
		t.Fatal("banned call must not reach any upstream")
	}
	if grantStatus(e) != exception.StatusApproved {
		t.Fatalf("a grant must not be consumed by a call it cannot waive, status is %q", grantStatus(e))
	}
}

func TestExceptionGrantNotConsumedWhenUnneeded(t *testing.T) {
	e, _ := setupTrust(t, trustPolicy())
	// A managed confidential call routes compliantly on its own, so
	// a presented grant is ignored, never consumed: it stays
	// available for a call that really needs a waiver.
	grant := grantFor(e, "chenjing", store.LevelConfidential)
	code, _ := chatWithGrant(e, e.mgdTok, "mock-pro", "请分析 XH-200 项目的进展", "", grant)
	if code != 200 {
		t.Fatalf("compliant call with grant presented: got %d, want 200", code)
	}
	if grantStatus(e) != exception.StatusApproved {
		t.Fatalf("grant must survive a call that needed no waiver, status is %q", grantStatus(e))
	}
}
