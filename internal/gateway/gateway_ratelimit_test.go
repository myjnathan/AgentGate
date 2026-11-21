package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/myjnathan/agentgate/internal/store"
)

// setTokenLimits configures per-token RPM/TPM for every token owned
// by username (tests mint exactly one per user).
func (e *testEnv) setTokenLimits(t *testing.T, username string, rpm, tpm int) {
	t.Helper()
	var uid string
	e.st.Read(func(s *store.State) {
		for _, u := range s.Users {
			if u.Username == username {
				uid = u.ID
			}
		}
	})
	if uid == "" {
		t.Fatalf("unknown user %q", username)
	}
	e.st.WithLock(func(s *store.State) {
		for _, tk := range s.Tokens {
			if tk.UserID == uid {
				tk.RPM = rpm
				tk.TPM = tpm
			}
		}
	})
}

// chatFull is e.chat plus response headers (for Retry-After).
func (e *testEnv) chatFull(t *testing.T, token, model, content string) (int, http.Header, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": content}},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, resp.Header, buf.String()
}

func TestRateLimitRPMEnforced(t *testing.T) {
	e := setup(t)
	e.setTokenLimits(t, "lixiaofeng", 2, 0)
	for i := 0; i < 2; i++ {
		code, _, body := e.chatFull(t, e.byoaTok, "mock-eco", "请假条怎么写")
		if code != 200 {
			t.Fatalf("request %d: got %d %s, want 200", i+1, code, body)
		}
	}
	code, hdr, body := e.chatFull(t, e.byoaTok, "mock-eco", "请假条怎么写")
	if code != http.StatusTooManyRequests {
		t.Fatalf("3rd request: got %d %s, want 429", code, body)
	}
	if !bytes.Contains([]byte(body), []byte("rate_limit_exceeded")) {
		t.Fatalf("body %s should carry rate_limit_exceeded", body)
	}
	if hdr.Get("Retry-After") == "" {
		t.Fatal("429 response should carry a Retry-After header")
	}
	// The denial is audited (no request content in the event).
	found := false
	for _, ev := range e.al.All() {
		if ev.Action == "rate_limited" && ev.Kind == "rate_limit" {
			found = true
			if ev.Detail["limit"] != "rpm" {
				t.Fatalf("rate_limited detail limit = %q, want rpm", ev.Detail["limit"])
			}
		}
	}
	if !found {
		t.Fatal("no rate_limited audit event found")
	}
}

func TestRateLimitTPMEnforced(t *testing.T) {
	e := setup(t)
	// TPM=1: the first request is allowed (nothing recorded yet),
	// but the mock reports >0 tokens, so the second is denied.
	e.setTokenLimits(t, "lixiaofeng", 100, 1)
	code, _, _ := e.chatFull(t, e.byoaTok, "mock-eco", "hi")
	if code != 200 {
		t.Fatalf("first request: got %d, want 200", code)
	}
	code, _, body := e.chatFull(t, e.byoaTok, "mock-eco", "hi")
	if code != http.StatusTooManyRequests || !bytes.Contains([]byte(body), []byte("rate_limit_exceeded")) {
		t.Fatalf("second request: got %d %s, want 429 rate_limit_exceeded", code, body)
	}
}

func TestRateLimitIsPerToken(t *testing.T) {
	e := setup(t)
	e.setTokenLimits(t, "lixiaofeng", 1, 0)
	if code, _ := e.chat(t, e.byoaTok, "mock-eco", "hi"); code != 200 {
		t.Fatalf("byoa first: got %d, want 200", code)
	}
	if code, _ := e.chat(t, e.byoaTok, "mock-eco", "hi"); code != http.StatusTooManyRequests {
		t.Fatalf("byoa second: got %d, want 429", code)
	}
	// The managed token has its own (default) budget.
	if code, _ := e.chat(t, e.mgdTok, "mock-eco", "hi"); code != 200 {
		t.Fatalf("managed token: got %d, want 200", code)
	}
}

func TestRateLimitDefaultsByTier(t *testing.T) {
	// Tier defaults are enforced even without explicit per-token
	// limits: BYOA (20 RPM) is tighter than managed (60 RPM).
	// Exhausting the BYOA default proves the default path is wired;
	// the managed default is covered by the same code path.
	e := setup(t)
	for i := 0; i < 20; i++ {
		code, _ := e.chat(t, e.byoaTok, "mock-eco", "hi")
		if code != 200 {
			t.Fatalf("request %d: got %d, want 200", i+1, code)
		}
	}
	if code, _ := e.chat(t, e.byoaTok, "mock-eco", "hi"); code != http.StatusTooManyRequests {
		t.Fatalf("21st BYOA request: got %d, want 429", code)
	}
}
