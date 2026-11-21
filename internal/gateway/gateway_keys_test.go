package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// keyedUpstream is a mock upstream that rejects one key with 401 and
// answers the rest, recording every Authorization header it sees.
type keyedUpstream struct {
	mu   sync.Mutex
	seen []string
	dead string
}

func (u *keyedUpstream) handler(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	u.mu.Lock()
	u.seen = append(u.seen, key)
	u.mu.Unlock()
	if key == u.dead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"Incorrect API key"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
}

func (u *keyedUpstream) keys() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

func mkChannelKeys(t *testing.T, plains ...string) []store.ChannelKey {
	t.Helper()
	// The gateway in setup() was built with this same master key, so
	// keys encrypted here decrypt there.
	box, err := secretbox.New("test-master-key-123456")
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]store.ChannelKey, 0, len(plains))
	for _, p := range plains {
		enc, err := box.Encrypt(p)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, store.ChannelKey{Enc: enc, Masked: secretbox.Mask(p)})
	}
	return keys
}

func addTestChannel(t *testing.T, e *testEnv, id, model string, upURL string, keys []store.ChannelKey) {
	t.Helper()
	e.st.WithLock(func(s *store.State) {
		s.Channels[id] = &store.Channel{ID: id, Name: id + " (test)", BaseURL: upURL,
			Keys: keys, Models: []string{model}, MaxDataLevel: store.LevelInternal, Weight: 1}
	})
}

// TestChannelKey401DisablesKeyAndRetries covers the F12 acceptance:
// a key the upstream rejects with 401 is auto-disabled, the same
// in-flight request is retried with the next key, and later requests
// never touch the dead key again.
func TestChannelKey401DisablesKeyAndRetries(t *testing.T) {
	e := setup(t)
	up := &keyedUpstream{dead: "bad-key"}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	addTestChannel(t, e, "ch_multi", "mock-multi", srv.URL, mkChannelKeys(t, "bad-key", "good-key"))

	code, body := e.chat(t, e.mgdTok, "mock-multi", "多 key 轮换测试一")
	if code != 200 || !strings.Contains(body, `"content":"ok"`) {
		t.Fatalf("first call: got %d %s, want 200 with upstream reply", code, body)
	}
	seen := up.keys()
	if len(seen) != 2 || seen[0] != "bad-key" || seen[1] != "good-key" {
		t.Fatalf("upstream saw keys %v, want [bad-key good-key] (401 then retry)", seen)
	}
	var stored *store.Channel
	e.st.Read(func(s *store.State) { stored = s.Channels["ch_multi"] })
	if stored == nil || !stored.Keys[0].Disabled || stored.Keys[0].DisabledReason != "upstream_401" {
		t.Fatalf("bad key not disabled after 401: %+v", stored)
	}
	if stored.Keys[1].Disabled {
		t.Fatal("good key must stay enabled")
	}
	found := false
	for _, ev := range e.al.All() {
		if ev.Kind == "channel" && ev.Action == "channel_key_disabled" && ev.Detail["key_index"] == "0" {
			found = true
		}
	}
	if !found {
		t.Fatal("no channel_key_disabled audit event for key 0")
	}

	// Second request (different body, so the F11 cache cannot answer
	// it) goes straight to the surviving key.
	code, _ = e.chat(t, e.mgdTok, "mock-multi", "多 key 轮换测试二")
	if code != 200 {
		t.Fatalf("second call: got %d, want 200", code)
	}
	seen = up.keys()
	if len(seen) != 3 || seen[2] != "good-key" {
		t.Fatalf("upstream saw keys %v, want third call to use good-key only", seen)
	}
}

// TestChannelKeyRoundRobinDistributesCalls covers per-request rotation:
// two healthy keys take turns, so quota load spreads across them.
func TestChannelKeyRoundRobinDistributesCalls(t *testing.T) {
	e := setup(t)
	up := &keyedUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	addTestChannel(t, e, "ch_rr", "mock-rr", srv.URL, mkChannelKeys(t, "key-a", "key-b"))

	for i, content := range []string{"轮询一", "轮询二", "轮询三"} {
		code, _ := e.chat(t, e.mgdTok, "mock-rr", content)
		if code != 200 {
			t.Fatalf("call %d: got %d, want 200", i, code)
		}
	}
	seen := up.keys()
	if len(seen) != 3 || seen[0] != "key-a" || seen[1] != "key-b" || seen[2] != "key-a" {
		t.Fatalf("upstream saw keys %v, want round-robin [key-a key-b key-a]", seen)
	}
}

// TestChannelAllKeysDeadReturnsUpstream401 covers the end of the line:
// every key rejected means the caller sees the upstream 401 (not a
// rewritten 502), and the next request reports no enabled key.
func TestChannelAllKeysDeadReturnsUpstream401(t *testing.T) {
	e := setup(t)
	up := &keyedUpstream{dead: "dead-key"}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	addTestChannel(t, e, "ch_dead", "mock-dead", srv.URL, mkChannelKeys(t, "dead-key"))

	code, body := e.chat(t, e.mgdTok, "mock-dead", "全 key 失效测试")
	if code != 401 {
		t.Fatalf("all-dead call: got %d %s, want upstream 401", code, body)
	}
	var stored *store.Channel
	e.st.Read(func(s *store.State) { stored = s.Channels["ch_dead"] })
	if stored == nil || !stored.Keys[0].Disabled {
		t.Fatal("dead key not disabled after 401")
	}

	// The request body differs so the cache cannot mask the second call.
	code, body = e.chat(t, e.mgdTok, "mock-dead", "全 key 失效测试二")
	if code != 502 {
		t.Fatalf("second call with no enabled key: got %d %s, want 502", code, body)
	}
	var parsed map[string]any
	_ = json.Unmarshal([]byte(body), &parsed)
	errObj, _ := parsed["error"].(map[string]any)
	if errObj["code"] != "channel_no_enabled_key" {
		t.Fatalf("second call error code = %v, want channel_no_enabled_key", errObj["code"])
	}
}
