package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// chatRaw posts a raw chat-completion body and returns the status,
// response headers and body text.
func (e *testEnv) chatRaw(t *testing.T, token string, payload map[string]any, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, resp.Header, buf.String()
}

func cleanPayload(content string) map[string]any {
	return map[string]any{
		"model":    "mock-eco",
		"messages": []map[string]string{{"role": "user", "content": content}},
	}
}

func TestCacheHitServesIdenticalBody(t *testing.T) {
	e := setup(t)
	payload := cleanPayload("缓存测试：星河精密的考勤制度是什么")
	code, h1, body1 := e.chatRaw(t, e.byoaTok, payload, nil)
	if code != 200 || h1.Get("X-AgentGate-Cache") != "MISS" {
		t.Fatalf("first call = %d cache %q, want 200 MISS", code, h1.Get("X-AgentGate-Cache"))
	}
	// The mock stamps created=now (seconds), so a sleep past a
	// second boundary makes a fresh upstream reply observably
	// different from the stored one.
	time.Sleep(1200 * time.Millisecond)
	code, h2, body2 := e.chatRaw(t, e.byoaTok, payload, nil)
	if code != 200 || h2.Get("X-AgentGate-Cache") != "HIT" {
		t.Fatalf("second call = %d cache %q, want 200 HIT", code, h2.Get("X-AgentGate-Cache"))
	}
	if body2 != body1 {
		t.Fatalf("cache hit body differs from first response:\n%s\nvs\n%s", body2, body1)
	}
	// Audit: exactly one upstream call and one cache hit — the hit
	// consumed no upstream tokens.
	var modelCalls, cacheHits int
	for _, ev := range e.al.All() {
		switch ev.Kind {
		case "model_call":
			modelCalls++
		case "cache_hit":
			cacheHits++
		}
	}
	if modelCalls != 1 || cacheHits != 1 {
		t.Fatalf("audit: model_call=%d cache_hit=%d, want 1/1", modelCalls, cacheHits)
	}
}

func TestCacheKeyIncludesParameters(t *testing.T) {
	e := setup(t)
	p1 := cleanPayload("缓存测试：参数不同是否命中")
	p2 := cleanPayload("缓存测试：参数不同是否命中")
	p2["temperature"] = 0.7
	if _, h, _ := e.chatRaw(t, e.byoaTok, p1, nil); h.Get("X-AgentGate-Cache") != "MISS" {
		t.Fatal("first call must be a MISS")
	}
	if _, h, _ := e.chatRaw(t, e.byoaTok, p2, nil); h.Get("X-AgentGate-Cache") != "MISS" {
		t.Fatal("same messages with a different temperature must miss (exact match)")
	}
	if _, h, _ := e.chatRaw(t, e.byoaTok, p1, nil); h.Get("X-AgentGate-Cache") != "HIT" {
		t.Fatal("the original parameters must hit")
	}
}

func TestSensitiveRequestNeverCached(t *testing.T) {
	e := setup(t)
	// PII in the request: the BYOA tier does not redact, so the
	// finding alone must keep the call out of the cache. Ineligible
	// requests carry no cache header at all (the cache was never
	// consulted); the differing bodies after a second boundary prove
	// both calls reached the upstream.
	pii := cleanPayload("我的手机号是13812345678，帮我记下来")
	_, h1, b1 := e.chatRaw(t, e.byoaTok, pii, nil)
	time.Sleep(1200 * time.Millisecond)
	_, h2, b2 := e.chatRaw(t, e.byoaTok, pii, nil)
	if h1.Get("X-AgentGate-Cache") != "" || h2.Get("X-AgentGate-Cache") != "" {
		t.Fatalf("PII request: cache headers %q/%q, want absent/absent (ineligible, never consulted)",
			h1.Get("X-AgentGate-Cache"), h2.Get("X-AgentGate-Cache"))
	}
	if b1 == b2 {
		t.Fatal("PII request bodies identical after a second boundary — the second call must have hit the upstream, not the cache")
	}
	// Confidential content is never cached either.
	conf := map[string]any{"model": "mock-pro",
		"messages": []map[string]string{{"role": "user", "content": "请分析 XH-200 项目的进展"}}}
	if _, h, _ := e.chatRaw(t, e.mgdTok, conf, nil); h.Get("X-AgentGate-Cache") != "" {
		t.Fatal("confidential request must not consult the cache")
	}
	if _, h, _ := e.chatRaw(t, e.mgdTok, conf, nil); h.Get("X-AgentGate-Cache") != "" {
		t.Fatal("confidential request must never be served from cache")
	}
}

func TestStreamingNeverCached(t *testing.T) {
	e := setup(t)
	payload := cleanPayload("缓存测试：流式不应缓存")
	payload["stream"] = true
	_, h1, b1 := e.chatRaw(t, e.mgdTok, payload, nil)
	if h1.Get("X-AgentGate-Cache") != "" {
		t.Fatalf("streaming response carries cache header %q, want none", h1.Get("X-AgentGate-Cache"))
	}
	if !strings.Contains(b1, "data: [DONE]") {
		t.Fatalf("streaming response missing [DONE]: %s", b1)
	}
	if _, h2, _ := e.chatRaw(t, e.mgdTok, payload, nil); h2.Get("X-AgentGate-Cache") != "" {
		t.Fatal("second streaming call must not be served from cache")
	}
}

func TestResponseWithSensitiveContentNeverCached(t *testing.T) {
	e := setup(t)
	// An upstream that answers a clean prompt with a phone number:
	// the response itself is sensitive, so the gateway must serve
	// it once but never store it.
	piiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-pii","object":"chat.completion","created":1700000000,"model":"mock-eco","choices":[{"index":0,"message":{"role":"assistant","content":"客服电话是 13812345678"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`))
	}))
	t.Cleanup(piiUpstream.Close)
	box, _ := secretbox.New("test-master-key-123456")
	enc, err := box.Encrypt("pii-upstream-key")
	if err != nil {
		t.Fatal(err)
	}
	e.st.WithLock(func(s *store.State) {
		s.Channels["ch_pii_test"] = &store.Channel{
			ID: "ch_pii_test", Name: "pii-upstream (test)", BaseURL: piiUpstream.URL,
			APIKeyEnc: enc, Models: []string{"mock-eco"}, MaxDataLevel: store.LevelPublic, Weight: 1,
		}
	})
	payload := cleanPayload("请问客服电话是多少")
	code, h1, b1 := e.chatRaw(t, e.byoaTok, payload, nil)
	if code != 200 || !strings.Contains(b1, "13812345678") {
		t.Fatalf("expected upstream PII reply, got %d %s", code, b1)
	}
	if h1.Get("X-AgentGate-Cache") != "MISS" {
		t.Fatalf("first call cache header = %q, want MISS", h1.Get("X-AgentGate-Cache"))
	}
	if _, h2, _ := e.chatRaw(t, e.byoaTok, payload, nil); h2.Get("X-AgentGate-Cache") != "MISS" {
		t.Fatalf("second call cache header = %q, want MISS (response with PII must not be cached)",
			h2.Get("X-AgentGate-Cache"))
	}
}
