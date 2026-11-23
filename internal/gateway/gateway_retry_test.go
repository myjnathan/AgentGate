package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// attemptTrail returns the forward_attempt audit events for the
// test, oldest first.
func attemptTrail(t *testing.T, e *testEnv) []audit.Event {
	t.Helper()
	var out []audit.Event
	for _, ev := range e.al.All() {
		if ev.Kind == "channel" && ev.Action == "forward_attempt" {
			out = append(out, ev)
		}
	}
	return out
}

func TestTransient500RetriesSameKeyThenSuccess(t *testing.T) {
	e, up := setupFailover(t, failoverPolicy(time.Minute),
		map[string][]int{"a": {500, 500, 200}, "b": {200}, "p": {200}})
	code, body := e.chat(t, e.byoaTok, "mock-eco", "瞬时失败重试测试")
	if code != 200 || !strings.Contains(body, "[upstream-a]") {
		t.Fatalf("expected 200 from primary a after retries, got %d %s", code, body)
	}
	if got := up.hitCount("a"); got != 3 {
		t.Fatalf("expected a hit 3 times (initial + 2 same-key retries), got %d", got)
	}
	if got := up.hitCount("b"); got != 0 {
		t.Fatalf("retries must not touch the backup, b got %d hits", got)
	}
	trail := attemptTrail(t, e)
	if len(trail) != 3 {
		t.Fatalf("expected 3 forward_attempt events, got %d", len(trail))
	}
	wantClass := []string{"transient", "transient", "success"}
	wantStatus := []string{"500", "500", "200"}
	wantResult := []string{"failed", "failed", "served"}
	for i, ev := range trail {
		if ev.Detail["class"] != wantClass[i] {
			t.Fatalf("attempt %d: class = %q, want %q", i+1, ev.Detail["class"], wantClass[i])
		}
		if ev.Detail["status"] != wantStatus[i] {
			t.Fatalf("attempt %d: status = %q, want %q", i+1, ev.Detail["status"], wantStatus[i])
		}
		if ev.Detail["result"] != wantResult[i] {
			t.Fatalf("attempt %d: result = %q, want %q", i+1, ev.Detail["result"], wantResult[i])
		}
		if ev.Detail["attempt"] != strconv.Itoa(i+1) {
			t.Fatalf("attempt %d: attempt number = %q", i+1, ev.Detail["attempt"])
		}
		if ev.Detail["key_masked"] == "" {
			t.Fatalf("attempt %d: key_masked missing", i+1)
		}
		if _, ok := ev.Detail["duration_ms"]; !ok {
			t.Fatalf("attempt %d: duration_ms missing", i+1)
		}
		// The trail is metadata only: no request/response body, no
		// key material — exactly these seven detail keys.
		if len(ev.Detail) != 7 {
			t.Fatalf("attempt %d: detail keys = %v, want exactly 7 metadata keys", i+1, ev.Detail)
		}
	}
	// All three attempts used the same key (same-key retry).
	if trail[0].Detail["key_masked"] != trail[1].Detail["key_masked"] ||
		trail[1].Detail["key_masked"] != trail[2].Detail["key_masked"] {
		t.Fatalf("same-key retries used different keys: %q %q %q",
			trail[0].Detail["key_masked"], trail[1].Detail["key_masked"], trail[2].Detail["key_masked"])
	}
	// The call event itself carries the wire-attempt count.
	found := false
	for _, ev := range e.al.All() {
		if ev.Kind == "model_call" && ev.Detail["attempts"] == "3" {
			found = true
		}
	}
	if !found {
		t.Fatal("no model_call event with attempts=3")
	}
}

func TestRateLimit429RetriesSameKey(t *testing.T) {
	e, up := setupFailover(t, failoverPolicy(time.Minute),
		map[string][]int{"a": {429, 200}, "b": {200}, "p": {200}})
	code, _ := e.chat(t, e.byoaTok, "mock-eco", "限流重试测试")
	if code != 200 {
		t.Fatalf("expected 200 after 429 retry, got %d", code)
	}
	if got := up.hitCount("a"); got != 2 {
		t.Fatalf("expected a hit 2 times (429 then success), got %d", got)
	}
	trail := attemptTrail(t, e)
	if len(trail) != 2 || trail[0].Detail["class"] != "transient" || trail[0].Detail["status"] != "429" {
		t.Fatalf("expected transient/429 first attempt, got %+v", trail)
	}
}

// forbiddenKeyUpstream rejects one key with 403 (e.g. the key lacks
// scope for this model) and serves the rest.
type forbiddenKeyUpstream struct {
	mu   sync.Mutex
	seen []string
	dead string
}

func (u *forbiddenKeyUpstream) handler(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	u.mu.Lock()
	u.seen = append(u.seen, key)
	u.mu.Unlock()
	if key == u.dead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"model_not_allowed","message":"key cannot access this model"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
}

func TestAuth403RotatesKeyWithoutDisabling(t *testing.T) {
	e := setup(t)
	up := &forbiddenKeyUpstream{dead: "no-scope-key"}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	addTestChannel(t, e, "ch_403", "mock-403", srv.URL, mkChannelKeys(t, "no-scope-key", "good-key"))

	code, body := e.chat(t, e.mgdTok, "mock-403", "403 轮换测试")
	if code != 200 || !strings.Contains(body, `"content":"ok"`) {
		t.Fatalf("expected 200 via the next key, got %d %s", code, body)
	}
	up.mu.Lock()
	seen := append([]string(nil), up.seen...)
	up.mu.Unlock()
	if len(seen) != 2 || seen[0] != "no-scope-key" || seen[1] != "good-key" {
		t.Fatalf("upstream saw keys %v, want [no-scope-key good-key] (403 then rotate, no same-key retry)", seen)
	}
	var stored *store.Channel
	e.st.Read(func(s *store.State) { stored = s.Channels["ch_403"] })
	if stored == nil || stored.Keys[0].Disabled {
		t.Fatal("a 403 must not disable the key — it may only lack scope for this model")
	}
	for _, ev := range e.al.All() {
		if ev.Kind == "channel" && ev.Action == "channel_key_disabled" {
			t.Fatal("403 must not emit channel_key_disabled (only 401 does)")
		}
	}
	trail := attemptTrail(t, e)
	if len(trail) != 2 || trail[0].Detail["class"] != "auth" || trail[0].Detail["status"] != "403" {
		t.Fatalf("expected auth/403 first attempt, got %+v", trail)
	}
}

func TestAuth403AllKeysReturnsUpstream403(t *testing.T) {
	e := setup(t)
	up := &forbiddenKeyUpstream{dead: "only-key"}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	addTestChannel(t, e, "ch_403only", "mock-403only", srv.URL, mkChannelKeys(t, "only-key"))

	code, body := e.chat(t, e.mgdTok, "mock-403only", "全 key 403 测试")
	if code != 403 || !strings.Contains(body, "model_not_allowed") {
		t.Fatalf("expected upstream 403 passthrough, got %d %s", code, body)
	}
	// The key stays enabled: the next call tries it again instead of
	// reporting channel_no_enabled_key.
	code, _ = e.chat(t, e.mgdTok, "mock-403only", "全 key 403 测试二")
	if code != 403 {
		t.Fatalf("second call: got %d, want 403 again (key must stay enabled)", code)
	}
}

func TestTransportErrorRetriesThenFailover(t *testing.T) {
	e, up := setupFailover(t, failoverPolicy(time.Minute),
		map[string][]int{"b": {200}, "p": {200}})
	// Point the primary at a dead listener: connection refused on
	// every attempt.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	e.st.WithLock(func(s *store.State) {
		if ch, ok := s.Channels["ch_cloud"]; ok {
			ch.BaseURL = dead.URL + "/a/v1"
		}
	})
	code, body := e.chat(t, e.byoaTok, "mock-eco", "传输失败重试测试")
	if code != 200 || !strings.Contains(body, "[upstream-b]") {
		t.Fatalf("expected 200 from backup after transport retries, got %d %s", code, body)
	}
	trail := attemptTrail(t, e)
	var transportAttempts int
	for _, ev := range trail {
		if ev.Detail["status"] == "transport_error" {
			transportAttempts++
			if ev.Detail["class"] != "transient" || ev.Detail["result"] != "failed" {
				t.Fatalf("transport attempt misclassified: %+v", ev.Detail)
			}
		}
	}
	if transportAttempts != 3 {
		t.Fatalf("expected 3 transport_error attempts on the dead primary, got %d (trail %d)", transportAttempts, len(trail))
	}
	if got := up.hitCount("b"); got != 1 {
		t.Fatalf("expected backup hit once, got %d", got)
	}
}

func TestRetryBudgetBoundsAttempts(t *testing.T) {
	pol := failoverPolicy(time.Minute)
	pol.MaxTransientRetries = 1 // 2 attempts per key, not the default 3
	e, up := setupFailover(t, pol, map[string][]int{"a": {500}, "b": {200}, "p": {200}})
	code, body := e.chat(t, e.byoaTok, "mock-eco", "重试预算测试")
	if code != 200 || !strings.Contains(body, "[upstream-b]") {
		t.Fatalf("expected 200 from backup, got %d %s", code, body)
	}
	if got := up.hitCount("a"); got != 2 {
		t.Fatalf("expected a hit 2 times (budget 1 retry), got %d", got)
	}
}
