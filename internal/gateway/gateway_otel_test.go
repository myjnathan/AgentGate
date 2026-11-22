package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/gateway"
	"github.com/myjnathan/agentgate/internal/mockllm"
	"github.com/myjnathan/agentgate/internal/otel"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

// fakeCollector captures raw OTLP/HTTP JSON bodies so tests can
// assert both the wire shape and that no request content leaks.
type fakeCollector struct {
	mu            sync.Mutex
	tracesBodies  [][]byte
	metricsBodies [][]byte
	upstreamTP    []string // traceparent headers seen by the mock upstream
}

func (c *fakeCollector) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.URL.Path {
	case "/v1/traces":
		c.tracesBodies = append(c.tracesBodies, body)
	case "/v1/metrics":
		c.metricsBodies = append(c.metricsBodies, body)
	}
	w.WriteHeader(http.StatusOK)
}

func (c *fakeCollector) spans(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, raw := range c.tracesBodies {
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("collector got non-JSON traces body: %v", err)
		}
		rs := payload["resourceSpans"].([]any)
		for _, r := range rs {
			for _, ss := range r.(map[string]any)["scopeSpans"].([]any) {
				for _, sp := range ss.(map[string]any)["spans"].([]any) {
					out = append(out, sp.(map[string]any))
				}
			}
		}
	}
	return out
}

func spanAttrs(sp map[string]any) map[string]string {
	out := map[string]string{}
	for _, a := range sp["attributes"].([]any) {
		kv := a.(map[string]any)
		v := kv["value"].(map[string]any)
		if s, ok := v["stringValue"]; ok {
			out[kv["key"].(string)] = s.(string)
		} else if n, ok := v["intValue"]; ok {
			out[kv["key"].(string)] = n.(string)
		}
	}
	return out
}

// setupOTel mirrors setupWithPolicy but points the gateway's tracer
// at the fake collector and records traceparent headers arriving at
// the mock upstream.
func setupOTel(t *testing.T) (*testEnv, *otel.Tracer, *fakeCollector) {
	t.Helper()
	col := &fakeCollector{}
	colSrv := httptest.NewServer(http.HandlerFunc(col.handler))
	t.Cleanup(colSrv.Close)

	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secretbox.New("test-master-key-123456")
	au := auth.New(st)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		col.mu.Lock()
		col.upstreamTP = append(col.upstreamTP, r.Header.Get("traceparent"))
		col.mu.Unlock()
		mockllm.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(mock.Close)
	if err := seed.Demo(st, box, mock.URL); err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	tr := otel.New(otel.Config{Endpoint: colSrv.URL, ServiceName: "agentgate-test", ServiceVersion: "test"})
	gw := gateway.New(st, au, box, al, gateway.Policy{
		BannedWords:   seed.DemoBannedWords,
		CustomWords:   seed.DemoCustomWords,
		RedactForTier: map[store.Tier]bool{store.TierManaged: true},
		Tracer:        tr,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", gw.Models)
	mux.HandleFunc("POST /v1/chat/completions", gw.ChatCompletions)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Reuse the token minting from setupWithPolicy's testEnv: mint a
	// BYOA token for lixiaofeng via the same device-code flow.
	env := &testEnv{srv: srv, st: st, al: al, gw: gw}
	mkToken := func(username string, tier store.Tier) string {
		var user *store.User
		st.Read(func(s *store.State) {
			for _, u := range s.Users {
				if u.Username == username {
					cp := *u
					user = &cp
				}
			}
		})
		dc := au.StartDeviceCode("dsh", tier)
		if _, _, err := au.ApproveDeviceCode(dc.Code, user, "test device"); err != nil {
			t.Fatal(err)
		}
		_, plain := au.PollDeviceCode(dc.Code, dc.Secret)
		if plain == "" {
			t.Fatal("no token issued")
		}
		return plain
	}
	env.byoaTok = mkToken("lixiaofeng", store.TierBYOA)
	env.mgdTok = mkToken("chenjing", store.TierManaged)
	return env, tr, col
}

func TestGatewaySpanExportedWithMetadataOnly(t *testing.T) {
	env, tr, col := setupOTel(t)
	// A distinctive marker: if it ever appears in an exported span
	// body, request content has leaked into telemetry.
	marker := "OTELMARKER-7f3a9"
	status, _ := env.chat(t, env.byoaTok, "mock-eco", "请写请假条 "+marker)
	if status != 200 {
		t.Fatalf("clean chat failed: %d", status)
	}
	tr.Flush()
	spans := col.spans(t)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	sp := spans[0]
	if sp["name"] != "gateway.chat_completions" || sp["kind"] != float64(2) {
		t.Errorf("span name/kind wrong: %v", sp)
	}
	if sp["status"].(map[string]any)["code"] != float64(1) {
		t.Errorf("clean call span must be OK: %v", sp["status"])
	}
	attrs := spanAttrs(sp)
	if attrs["agentgate.model"] != "mock-eco" || attrs["agentgate.decision"] != "allow" {
		t.Errorf("span attrs wrong: %v", attrs)
	}
	if attrs["agentgate.tokens_total"] == "0" || attrs["agentgate.tokens_total"] == "" {
		t.Errorf("span must carry token count: %v", attrs)
	}
	if attrs["agentgate.data_level"] != "internal" && attrs["agentgate.data_level"] != "public" {
		t.Errorf("span must carry data level: %v", attrs)
	}
	for _, raw := range col.tracesBodies {
		if strings.Contains(string(raw), marker) {
			t.Errorf("request content leaked into exported span: %s", raw)
		}
	}
	// The upstream request carried this call's traceparent.
	col.mu.Lock()
	tp := append([]string{}, col.upstreamTP...)
	col.mu.Unlock()
	if len(tp) != 1 || !strings.HasPrefix(tp[0], "00-"+sp["traceId"].(string)+"-"+sp["spanId"].(string)+"-") {
		t.Errorf("upstream traceparent must match span: %v vs %v", tp, sp)
	}
}

func TestGatewayBlockedCallSpanIsError(t *testing.T) {
	env, tr, col := setupOTel(t)
	status, _ := env.chat(t, env.byoaTok, "mock-eco", "包含 test_sensitive 内容")
	if status != 400 {
		t.Fatalf("banned word must be blocked: %d", status)
	}
	tr.Flush()
	spans := col.spans(t)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	attrs := spanAttrs(spans[0])
	if attrs["agentgate.decision"] != "block" {
		t.Errorf("blocked call span decision must be block: %v", attrs)
	}
	if spans[0]["status"].(map[string]any)["code"] != float64(2) {
		t.Errorf("blocked call span must be ERROR: %v", spans[0]["status"])
	}
	for _, raw := range col.tracesBodies {
		if strings.Contains(string(raw), "test_sensitive") {
			t.Errorf("banned word leaked into exported span: %s", raw)
		}
	}
}

func TestGatewayJoinsIncomingTraceparent(t *testing.T) {
	env, tr, col := setupOTel(t)
	body, _ := json.Marshal(map[string]any{
		"model":    "mock-eco",
		"messages": []map[string]string{{"role": "user", "content": "你好"}},
	})
	req, _ := http.NewRequest("POST", env.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.byoaTok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("chat failed: %d", resp.StatusCode)
	}
	tr.Flush()
	spans := col.spans(t)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0]["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("span must join incoming trace: %v", spans[0]["traceId"])
	}
	if spans[0]["parentSpanId"] != "00f067aa0ba902b7" {
		t.Errorf("span must record incoming parent: %v", spans[0]["parentSpanId"])
	}
}

func TestGatewayCountersExportedAsMetrics(t *testing.T) {
	env, tr, col := setupOTel(t)
	if _, _ = env.chat(t, env.byoaTok, "mock-eco", "你好"); true {
	}
	tr.Flush()
	col.mu.Lock()
	defer col.mu.Unlock()
	if len(col.metricsBodies) != 1 {
		t.Fatalf("expected 1 metrics export, got %d", len(col.metricsBodies))
	}
	var payload map[string]any
	if err := json.Unmarshal(col.metricsBodies[0], &payload); err != nil {
		t.Fatal(err)
	}
	rm := payload["resourceMetrics"].([]any)[0].(map[string]any)
	ms := rm["scopeMetrics"].([]any)[0].(map[string]any)["metrics"].([]any)
	names := map[string]bool{}
	for _, m := range ms {
		names[m.(map[string]any)["name"].(string)] = true
	}
	if !names["agentgate.gateway.calls"] || !names["agentgate.gateway.tokens"] {
		t.Errorf("metrics must include gateway calls + tokens: %v", names)
	}
}
