package mcp_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/myjnathan/agentgate/internal/otel"
)

// mcpCollector is a minimal OTLP/HTTP JSON sink for the MCP tests.
type mcpCollector struct {
	mu           sync.Mutex
	tracesBodies [][]byte
}

func (c *mcpCollector) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.URL.Path == "/v1/traces" {
		c.tracesBodies = append(c.tracesBodies, body)
	}
	w.WriteHeader(http.StatusOK)
}

func (c *mcpCollector) spans(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, raw := range c.tracesBodies {
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("collector got non-JSON traces body: %v", err)
		}
		for _, r := range payload["resourceSpans"].([]any) {
			for _, ss := range r.(map[string]any)["scopeSpans"].([]any) {
				for _, sp := range ss.(map[string]any)["spans"].([]any) {
					out = append(out, sp.(map[string]any))
				}
			}
		}
	}
	return out
}

func mcpSpanAttrs(sp map[string]any) map[string]string {
	out := map[string]string{}
	attrs, _ := sp["attributes"].([]any)
	for _, a := range attrs {
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

func callRPC(t *testing.T, env *testEnv, token string, method string, params map[string]any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, _ := http.NewRequest("POST", env.srv.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestMCPToolCallSpan(t *testing.T) {
	env := setup(t)
	col := &mcpCollector{}
	srv := httptest.NewServer(http.HandlerFunc(col.handler))
	defer srv.Close()
	tr := otel.New(otel.Config{Endpoint: srv.URL, ServiceName: "agentgate-test", ServiceVersion: "test"})
	env.msrv.SetTracer(tr)

	status, out := callRPC(t, env, env.byoaTok, "tools/call",
		map[string]any{"name": "kb_search", "arguments": map[string]any{"query": "报销 住宿"}})
	if status != 200 || out["result"] == nil {
		t.Fatalf("kb_search failed: %d %v", status, out)
	}
	tr.Flush()
	spans := col.spans(t)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0]["name"] != "mcp.tools.call" {
		t.Errorf("span name wrong: %v", spans[0]["name"])
	}
	attrs := mcpSpanAttrs(spans[0])
	if attrs["agentgate.tool"] != "kb_search" || attrs["agentgate.decision"] != "allow" {
		t.Errorf("span attrs wrong: %v", attrs)
	}
	if spans[0]["status"].(map[string]any)["code"] != float64(1) {
		t.Errorf("successful tool call span must be OK: %v", spans[0]["status"])
	}
	// Arguments must never appear in the exported body.
	for _, raw := range col.tracesBodies {
		if strings.Contains(string(raw), "报销") {
			t.Errorf("tool arguments leaked into exported span: %s", raw)
		}
	}
	st := tr.Stats()
	if st.Counters["agentgate.mcp.tool_calls{decision=allow,tool=kb_search}"] != 1 {
		t.Errorf("tool call counter wrong: %v", st.Counters)
	}
}

func TestMCPDeniedToolCallSpanIsError(t *testing.T) {
	env := setup(t)
	col := &mcpCollector{}
	srv := httptest.NewServer(http.HandlerFunc(col.handler))
	defer srv.Close()
	tr := otel.New(otel.Config{Endpoint: srv.URL, ServiceName: "agentgate-test", ServiceVersion: "test"})
	env.msrv.SetTracer(tr)

	// kb_get on a confidential document is managed-only; a BYOA
	// token is refused with a JSON-RPC error (still HTTP 200).
	status, out := callRPC(t, env, env.byoaTok, "tools/call",
		map[string]any{"name": "kb_get", "arguments": map[string]any{"doc_id": "kb_reimburse"}})
	if status != 200 || out["error"] == nil {
		t.Fatalf("expected JSON-RPC error, got %d %v", status, out)
	}
	tr.Flush()
	spans := col.spans(t)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0]["status"].(map[string]any)["code"] != float64(2) {
		t.Errorf("denied tool call span must be ERROR: %v", spans[0]["status"])
	}
	attrs := mcpSpanAttrs(spans[0])
	if attrs["agentgate.tool"] != "kb_get" || attrs["agentgate.decision"] != "error" {
		t.Errorf("span attrs wrong: %v", attrs)
	}
	if attrs["agentgate.rpc_error_code"] == "" {
		t.Errorf("span must carry the RPC error code: %v", attrs)
	}
}

func TestMCPUntracedMethodsProduceNoSpan(t *testing.T) {
	env := setup(t)
	col := &mcpCollector{}
	srv := httptest.NewServer(http.HandlerFunc(col.handler))
	defer srv.Close()
	tr := otel.New(otel.Config{Endpoint: srv.URL, ServiceName: "agentgate-test", ServiceVersion: "test"})
	env.msrv.SetTracer(tr)

	status, out := callRPC(t, env, env.byoaTok, "tools/list", nil)
	if status != 200 || out["result"] == nil {
		t.Fatalf("tools/list failed: %d %v", status, out)
	}
	tr.Flush()
	if spans := col.spans(t); len(spans) != 0 {
		t.Errorf("tools/list must not be traced, got %d spans", len(spans))
	}
}
