package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/gateway"
	"github.com/myjnathan/agentgate/internal/store"
)

// scriptedUpstream serves /<name>/v1/chat/completions with a scripted
// status sequence per upstream name, records hits, and echoes a marker
// so tests can tell which upstream answered.
type scriptedUpstream struct {
	mu     sync.Mutex
	script map[string][]int // name -> status sequence (last repeats)
	hits   map[string]int
}

func newScriptedUpstream(script map[string][]int) *scriptedUpstream {
	return &scriptedUpstream{script: script, hits: map[string]int{}}
}

func (s *scriptedUpstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		name := parts[0]
		s.mu.Lock()
		s.hits[name]++
		n := s.hits[name]
		seq := s.script[name]
		status := 200
		if len(seq) > 0 {
			if n <= len(seq) {
				status = seq[n-1]
			} else {
				status = seq[len(seq)-1]
			}
		}
		s.mu.Unlock()
		if status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"code":"upstream_%d"}}`, status)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 0, "model": "mock-eco",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{
				"role": "assistant", "content": "[upstream-" + name + "] ok"}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	})
	return mux
}

func (s *scriptedUpstream) hitCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[name]
}

// setupFailover builds the standard env, then re-points the seeded
// channels at a scripted upstream: cloud -> a, a same-level backup
// cloud2 -> b, desensitized cloud -> c, private -> p, local model -> l.
func setupFailover(t *testing.T, pol gateway.Policy, script map[string][]int) (*testEnv, *scriptedUpstream) {
	t.Helper()
	e := setupWithPolicy(t, pol)
	up := newScriptedUpstream(script)
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)
	e.st.WithLock(func(s *store.State) {
		if ch, ok := s.Channels["ch_cloud"]; ok {
			ch.BaseURL = srv.URL + "/a/v1"
			cp := *ch
			cp.ID = "ch_cloud2"
			cp.Name = "cloud-backup (mock)"
			cp.BaseURL = srv.URL + "/b/v1"
			s.Channels["ch_cloud2"] = &cp
		}
		if ch, ok := s.Channels["ch_cloud_conf"]; ok {
			ch.BaseURL = srv.URL + "/c/v1"
		}
		if ch, ok := s.Channels["ch_private"]; ok {
			ch.BaseURL = srv.URL + "/p/v1"
		}
		if ch, ok := s.Channels["ch_local"]; ok {
			ch.BaseURL = srv.URL + "/l/v1"
		}
	})
	return e, up
}

func failoverPolicy(cooldown time.Duration) gateway.Policy {
	return gateway.Policy{
		BannedWords:   []string{"test_sensitive"},
		CustomWords:   []string{"XH-200", "星河 XH-200"},
		RedactForTier: map[store.Tier]bool{store.TierManaged: true},
		FailThreshold: 3,
		Cooldown:      cooldown,
	}
}

func TestFailoverToSameLevelBackupOn5xx(t *testing.T) {
	e, up := setupFailover(t, failoverPolicy(time.Minute),
		map[string][]int{"a": {500}, "b": {200}, "p": {200}})
	code, body := e.chat(t, e.byoaTok, "mock-eco", "你好")
	if code != 200 || !strings.Contains(body, "[upstream-b]") {
		t.Fatalf("expected 200 from backup upstream b, got %d %s", code, body)
	}
	if got := up.hitCount("a"); got != 1 {
		t.Fatalf("expected primary a hit once, got %d", got)
	}
}

func TestChannelCooldownAfterConsecutiveFailures(t *testing.T) {
	e, up := setupFailover(t, failoverPolicy(300*time.Millisecond),
		map[string][]int{"a": {500}, "b": {200}, "p": {200}})
	for i := 0; i < 3; i++ {
		code, _ := e.chat(t, e.byoaTok, "mock-eco", "你好")
		if code != 200 {
			t.Fatalf("request %d: expected failover success, got %d", i+1, code)
		}
	}
	if got := up.hitCount("a"); got != 3 {
		t.Fatalf("expected a hit 3 times, got %d", got)
	}
	// a is now in cooldown: the next request must skip it entirely.
	code, body := e.chat(t, e.byoaTok, "mock-eco", "你好")
	if code != 200 || !strings.Contains(body, "[upstream-b]") {
		t.Fatalf("expected direct backup success, got %d %s", code, body)
	}
	if got := up.hitCount("a"); got != 3 {
		t.Fatalf("expected a still at 3 hits while cooling down, got %d", got)
	}
	// After the cooldown expires, a becomes eligible again.
	time.Sleep(350 * time.Millisecond)
	code, _ = e.chat(t, e.byoaTok, "mock-eco", "你好")
	if code != 200 {
		t.Fatalf("expected success after cooldown expiry, got %d", code)
	}
	if got := up.hitCount("a"); got != 4 {
		t.Fatalf("expected a hit again after cooldown expiry, got %d hits", got)
	}
}

func TestAllChannelsCoolingDownReturns503(t *testing.T) {
	e, up := setupFailover(t, failoverPolicy(time.Minute),
		map[string][]int{"a": {500}, "b": {500}, "p": {200}})
	for i := 0; i < 3; i++ {
		code, _ := e.chat(t, e.byoaTok, "mock-eco", "你好")
		if code != 502 {
			t.Fatalf("request %d: expected 502, got %d", i+1, code)
		}
	}
	code, body := e.chat(t, e.byoaTok, "mock-eco", "你好")
	if code != 503 || !strings.Contains(body, "channels_in_cooldown") {
		t.Fatalf("expected 503 channels_in_cooldown, got %d %s", code, body)
	}
	if got := up.hitCount("p"); got != 0 {
		t.Fatalf("private channel must not be tried for public data, got %d hits", got)
	}
}

func TestNoFallbackToHigherPrivilegeChannel(t *testing.T) {
	e, up := setupFailover(t, failoverPolicy(time.Minute),
		map[string][]int{"a": {500}, "b": {500}, "p": {200}})
	// Public data, both same-level cloud channels down, private healthy:
	// the request must fail rather than "upgrade" to the private channel.
	code, _ := e.chat(t, e.byoaTok, "mock-pro", "你好")
	if code != 502 {
		t.Fatalf("expected 502, got %d", code)
	}
	if got := up.hitCount("p"); got != 0 {
		t.Fatalf("must not fall back to higher-privilege private channel, got %d hits", got)
	}
}

func TestConfidentialNeverFallsBackToLowerLevel(t *testing.T) {
	e, up := setupFailover(t, failoverPolicy(time.Minute),
		map[string][]int{"a": {200}, "b": {200}, "c": {500}, "p": {200}, "l": {200}})
	// Confidential content defaults to the desensitized cloud (c); when
	// it fails, the request fails instead of dropping to the lower-level
	// plain cloud channels (a/b) — and it must not "upgrade" to the
	// dedicated-cloud (p) or local-model (l) channels either: failover
	// stays inside the first pick's level.
	code, _ := e.chat(t, e.mgdTok, "mock-pro", "请分析 XH-200 项目的进展")
	if code != 502 {
		t.Fatalf("expected 502, got %d", code)
	}
	if got := up.hitCount("a") + up.hitCount("b"); got != 0 {
		t.Fatalf("must not fall back to lower-level cloud channels, got %d hits", got)
	}
	if got := up.hitCount("p") + up.hitCount("l"); got != 0 {
		t.Fatalf("must not upgrade to dedicated/local channels on failure, got %d hits", got)
	}
}

func TestNonRetryable4xxNotRetriedAndNotCounted(t *testing.T) {
	e, up := setupFailover(t, failoverPolicy(time.Minute),
		map[string][]int{"a": {400}, "b": {200}, "p": {200}})
	for i := 0; i < 4; i++ {
		code, body := e.chat(t, e.byoaTok, "mock-eco", "你好")
		if code != 400 || !strings.Contains(body, "upstream_400") {
			t.Fatalf("request %d: expected 400 passthrough, got %d %s", i+1, code, body)
		}
	}
	// Four 4xx in a row must not cool the channel down (it is still
	// picked on the 4th request) and must never touch the backup.
	if got := up.hitCount("a"); got != 4 {
		t.Fatalf("expected a hit 4 times, got %d", got)
	}
	if got := up.hitCount("b"); got != 0 {
		t.Fatalf("4xx must not trigger failover, backup got %d hits", got)
	}
}

func TestSuccessResetsConsecutiveFailures(t *testing.T) {
	e, up := setupFailover(t, failoverPolicy(300*time.Millisecond),
		map[string][]int{"a": {500, 500, 200, 500, 500, 500}, "b": {200}, "p": {200}})
	// a fails twice, succeeds (reset), then fails 3 times in a row and
	// cools down; the 7th request must skip it.
	for i := 0; i < 6; i++ {
		code, _ := e.chat(t, e.byoaTok, "mock-eco", "你好")
		if code != 200 {
			t.Fatalf("request %d: expected 200, got %d", i+1, code)
		}
	}
	code, body := e.chat(t, e.byoaTok, "mock-eco", "你好")
	if code != 200 || !strings.Contains(body, "[upstream-b]") {
		t.Fatalf("expected backup success with a cooling down, got %d %s", code, body)
	}
	if got := up.hitCount("a"); got != 6 {
		t.Fatalf("expected a hit 6 times (not re-tried while cooling), got %d", got)
	}
}
