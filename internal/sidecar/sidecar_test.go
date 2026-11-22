package sidecar_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/myjnathan/agentgate/internal/sidecar"
	"github.com/myjnathan/agentgate/internal/store"
	"github.com/myjnathan/agentgate/internal/terminal"
)

// fakeGateway records what it received (like mockllm, it echoes the
// last user message so tests can observe redaction + restore). It
// also emulates the F23 server endpoints: GET /api/sidecar/policy
// returns a snapshot built from policyBanned/policyCustom, and POST
// /api/terminals/heartbeat records the heartbeat. Chat bodies and
// headers are recorded separately from policy/heartbeat traffic, so
// calls() still counts model calls only. Set failChat to make chat
// requests fail at the transport level (gateway unreachable).
type fakeGateway struct {
	mu           sync.Mutex
	bodies       []string
	headers      []http.Header
	modelsOK     bool
	policyBanned []string
	policyCustom []string
	policyHits   int
	heartbeats   []string
	failChat     bool
}

func (f *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"mock-eco","object":"model"}]}`))
		return
	}
	if r.URL.Path == "/api/sidecar/policy" {
		f.mu.Lock()
		f.policyHits++
		banned := append([]string(nil), f.policyBanned...)
		custom := append([]string(nil), f.policyCustom...)
		f.mu.Unlock()
		snap := terminal.NewPolicySnapshot(banned, custom, false, "internal")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"source": "gateway", "version": snap.Version,
			"banned_words": snap.BannedWords, "custom_words": snap.CustomWords,
			"dry_run": snap.DryRun, "offline_max_level": snap.OfflineMaxLevel})
		return
	}
	if r.URL.Path == "/api/terminals/heartbeat" {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.heartbeats = append(f.heartbeats, string(body))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"terminal_id": "term_test", "status": "online", "drift_reasons": []string{}})
		return
	}
	f.mu.Lock()
	fail := f.failChat
	f.mu.Unlock()
	if fail {
		// Drop the connection without a response: the client's Do
		// fails, which is exactly the gateway-unreachable path.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	var req struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	last := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				last = s
			}
		}
	}
	reply := "[fake-model] echo: " + last
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunk := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": reply}}}}
		b, _ := json.Marshal(chunk)
		// Write byte-by-byte so the restore path is exercised with
		// placeholders split across reads.
		data := []byte("data: " + string(b) + "\n\ndata: [DONE]\n\n")
		for _, c := range data {
			_, _ = w.Write([]byte{c})
			fl.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": reply}}},
		"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10},
	})
}

func (f *fakeGateway) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeGateway) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[len(f.bodies)-1]
}

func (f *fakeGateway) lastHeader() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers[len(f.headers)-1]
}

func (f *fakeGateway) policyHitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policyHits
}

func (f *fakeGateway) heartbeatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.heartbeats)
}

func (f *fakeGateway) lastHeartbeat() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeats[len(f.heartbeats)-1]
}

func (f *fakeGateway) setFailChat(fail bool) {
	f.mu.Lock()
	f.failChat = fail
	f.mu.Unlock()
}

func startSidecar(t *testing.T, gw *fakeGateway, cfg sidecar.Config) *httptest.Server {
	t.Helper()
	gwSrv := httptest.NewServer(gw)
	t.Cleanup(gwSrv.Close)
	cfg.GatewayURL = gwSrv.URL
	srv, err := sidecar.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(srv.Handler())
	t.Cleanup(s.Close)
	return s
}

func postChat(t *testing.T, base, token, model, content string) (int, string) {
	t.Helper()
	return postChatRaw(t, base, token, map[string]any{
		"model": model, "messages": []map[string]string{{"role": "user", "content": content}},
	})
}

func postChatRaw(t *testing.T, base, token string, payload map[string]any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(buf)
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  sidecar.Config
		want string // "" = valid
	}{
		{"empty gateway", sidecar.Config{}, "gateway_url is required"},
		{"ftp scheme", sidecar.Config{GatewayURL: "ftp://example.com"}, "must be http(s)"},
		{"userinfo", sidecar.Config{GatewayURL: "http://user:pass@example.com"}, "must not embed credentials"},
		{"no host", sidecar.Config{GatewayURL: "http:///v1"}, "must have a host"},
		{"loop", sidecar.Config{GatewayURL: "http://127.0.0.1:18765", Listen: "127.0.0.1:18765"}, "points back at the sidecar"},
		{"bad listen", sidecar.Config{GatewayURL: "http://127.0.0.1:8787", Listen: "nolisten"}, "invalid listen address"},
		{"valid", sidecar.Config{GatewayURL: "http://127.0.0.1:8787"}, ""},
	} {
		_, err := sidecar.New(tc.cfg)
		if tc.want == "" && err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestCleanRequestPassesThrough(t *testing.T) {
	gw := &fakeGateway{}
	s := startSidecar(t, gw, sidecar.Config{})
	code, body := postChat(t, s.URL, "tok", "mock-eco", "帮我写请假条")
	if code != 200 {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	if !strings.Contains(body, "帮我写请假条") {
		t.Fatalf("echo missing in %s", body)
	}
	if gw.calls() != 1 {
		t.Fatalf("want 1 upstream call, got %d", gw.calls())
	}
	if h := gw.lastHeader().Get("X-AgentGate-Sidecar"); h != sidecar.SidecarHeader {
		t.Fatalf("want sidecar header %q, got %q", sidecar.SidecarHeader, h)
	}
	if h := gw.lastHeader().Get("X-AgentGate-Data-Level"); h != "" {
		t.Fatalf("public request must not declare a data level, got %q", h)
	}
}

func TestPIIRedactedUpstreamAndRestoredDownstream(t *testing.T) {
	gw := &fakeGateway{}
	s := startSidecar(t, gw, sidecar.Config{})
	email := "lixiaofeng@xinghe.example"
	code, body := postChat(t, s.URL, "tok", "mock-eco", "我的邮箱是 "+email+"，请帮我写封邮件")
	if code != 200 {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	// The agent sees the original value: the response was restored.
	if !strings.Contains(body, email) {
		t.Fatalf("restored email missing in response: %s", body)
	}
	if strings.Contains(body, "<PII_") {
		t.Fatalf("placeholder leaked to the agent: %s", body)
	}
	// The gateway only ever saw the placeholder. The sidecar
	// re-marshals JSON with Go's default HTML escaping, so decode
	// before asserting (same as the gateway's own redactBody).
	var upDecoded struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(gw.lastBody()), &upDecoded); err != nil {
		t.Fatalf("upstream body must be valid JSON: %v", err)
	}
	upContent := upDecoded.Messages[0].Content
	if strings.Contains(upContent, email) {
		t.Fatalf("original email reached the gateway: %s", upContent)
	}
	if !strings.Contains(upContent, "<PII_1>") {
		t.Fatalf("placeholder missing upstream: %s", upContent)
	}
	// The sidecar raised the declared data level (email = internal).
	if h := gw.lastHeader().Get("X-AgentGate-Data-Level"); h != store.LevelInternal.String() {
		t.Fatalf("want data level internal, got %q", h)
	}
	// Status counters agree, and carry no original values.
	resp, err := http.Get(s.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st sidecar.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Counters.RedactedRequests != 1 || st.Counters.RestoredValues != 1 {
		t.Fatalf("want 1 redacted request / 1 restored value, got %+v", st.Counters)
	}
	if st.Counters.FindingsByKind["pii:email"] != 1 {
		t.Fatalf("want 1 pii:email finding, got %+v", st.Counters.FindingsByKind)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), email) {
		t.Fatalf("status leaks the original value")
	}
}

func TestBannedWordBlockedLocally(t *testing.T) {
	gw := &fakeGateway{}
	s := startSidecar(t, gw, sidecar.Config{BannedWords: []string{"test_sensitive"}})
	code, body := postChat(t, s.URL, "tok", "mock-eco", "包含 test_sensitive 内容")
	if code != 400 {
		t.Fatalf("want 400, got %d: %s", code, body)
	}
	if !strings.Contains(body, "sidecar_blocked") {
		t.Fatalf("want sidecar_blocked code, got %s", body)
	}
	if gw.calls() != 0 {
		t.Fatalf("blocked request must never reach the gateway, got %d calls", gw.calls())
	}
	resp, err := http.Get(s.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st sidecar.Status
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if st.Counters.BlockedLocal != 1 {
		t.Fatalf("want blocked_local 1, got %+v", st.Counters)
	}
}

func TestDryRunRecordsWithoutBlockingOrRedacting(t *testing.T) {
	gw := &fakeGateway{}
	s := startSidecar(t, gw, sidecar.Config{BannedWords: []string{"test_sensitive"}, DryRun: true})
	email := "lixiaofeng@xinghe.example"
	code, body := postChat(t, s.URL, "tok", "mock-eco", "包含 test_sensitive 内容，我的邮箱是 "+email)
	if code != 200 {
		t.Fatalf("dry-run must not block, got %d: %s", code, body)
	}
	// Dry-run forwards the original text untouched.
	if !strings.Contains(gw.lastBody(), email) || !strings.Contains(gw.lastBody(), "test_sensitive") {
		t.Fatalf("dry-run must forward original text, got %s", gw.lastBody())
	}
	resp, err := http.Get(s.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st sidecar.Status
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if st.Counters.BlockedLocal != 0 || st.Counters.RedactedRequests != 0 {
		t.Fatalf("dry-run must not block or redact, got %+v", st.Counters)
	}
	if len(st.Recent) == 0 || st.Recent[0].Decision != "dry_run" {
		t.Fatalf("want a dry_run decision recorded, got %+v", st.Recent)
	}
}

func TestStreamingRestoreAcrossChunkBoundaries(t *testing.T) {
	gw := &fakeGateway{}
	s := startSidecar(t, gw, sidecar.Config{})
	email := "lixiaofeng@xinghe.example"
	code, body := postChatRaw(t, s.URL, "tok", map[string]any{
		"model": "mock-eco", "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "我的邮箱是 " + email}},
	})
	if code != 200 {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	if !strings.Contains(body, email) {
		t.Fatalf("streamed response must restore the email, got %s", body)
	}
	if strings.Contains(body, "<PII_") {
		t.Fatalf("placeholder leaked in stream: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream must end with [DONE], got %s", body)
	}
}

func TestToolCallsArgumentsRedactedWithSharedNumbering(t *testing.T) {
	gw := &fakeGateway{}
	s := startSidecar(t, gw, sidecar.Config{})
	email := "lixiaofeng@xinghe.example"
	code, body := postChatRaw(t, s.URL, "tok", map[string]any{
		"model": "mock-eco",
		"messages": []map[string]any{{
			"role": "assistant",
			"tool_calls": []map[string]any{{
				"id": "call_1", "type": "function",
				"function": map[string]any{
					"name":      "send_mail",
					"arguments": `{"to": "` + email + `", "cc": "` + email + `"}`,
				},
			}},
		}},
	})
	if code != 200 {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	// The same value in two arguments gets one placeholder (F10 rule).
	// The sidecar re-marshals JSON with Go's default HTML escaping
	// (<PII_1> travels as \u003cPII_1\u003e on the wire, exactly as
	// the gateway's own redactBody does); decode before counting.
	up := gw.lastBody()
	var decoded struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(up), &decoded); err != nil {
		t.Fatalf("upstream body must be valid JSON: %v", err)
	}
	args := decoded.Messages[0].ToolCalls[0].Function.Arguments
	if strings.Count(args, "<PII_1>") != 2 {
		t.Fatalf("want the same placeholder twice upstream, got %s", args)
	}
	if strings.Contains(args, email) {
		t.Fatalf("original email reached the gateway: %s", args)
	}
}

func TestModelsPassthrough(t *testing.T) {
	gw := &fakeGateway{}
	s := startSidecar(t, gw, sidecar.Config{})
	resp, err := http.Get(s.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(buf), "mock-eco") {
		t.Fatalf("want model list proxied, got %d %s", resp.StatusCode, buf)
	}
}

func TestHealthz(t *testing.T) {
	gw := &fakeGateway{}
	s := startSidecar(t, gw, sidecar.Config{})
	resp, err := http.Get(s.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}
