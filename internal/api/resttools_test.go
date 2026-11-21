package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/store"
)

func TestRESTToolsCRUD(t *testing.T) {
	srv, st, adminSess, empSess := setupTokensAPI(t)

	call := func(method, path, sess string, body any) (int, map[string]any) {
		t.Helper()
		var buf *bytes.Buffer
		if body != nil {
			b, _ := json.Marshal(body)
			buf = bytes.NewBuffer(b)
		} else {
			buf = bytes.NewBuffer(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, buf)
		req.Header.Set("Content-Type", "application/json")
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	schema := json.RawMessage(`{"type":"object","properties":{"employee_id":{"type":"string"}},"required":["employee_id"]}`)
	newTool := map[string]any{
		"name": "leave_balance_test", "description": "d", "base_url": "https://hr.example.invalid",
		"method": "GET", "path": "/leave-balance", "token": "hr-secret",
		"input_schema": schema, "max_data_level": "internal", "min_tier": "byoa",
	}

	// Employee may not register a REST tool.
	if code, _ := call("POST", "/api/mcp/rest-tools", empSess, newTool); code != 403 {
		t.Fatalf("employee create REST tool: got %d, want 403", code)
	}
	// Invalid name.
	bad := map[string]any{}
	for k, v := range newTool {
		bad[k] = v
	}
	bad["name"] = "Leave-Balance"
	if code, _ := call("POST", "/api/mcp/rest-tools", adminSess, bad); code != 400 {
		t.Fatalf("bad tool name: got %d, want 400", code)
	}
	// Invalid method.
	bad["name"] = "leave_balance_test"
	bad["method"] = "HEAD"
	if code, _ := call("POST", "/api/mcp/rest-tools", adminSess, bad); code != 400 {
		t.Fatalf("bad method: got %d, want 400", code)
	}
	// Path must start with /.
	bad["method"] = "GET"
	bad["path"] = "leave-balance"
	if code, _ := call("POST", "/api/mcp/rest-tools", adminSess, bad); code != 400 {
		t.Fatalf("bad path: got %d, want 400", code)
	}
	// Invalid schema (required names an undeclared property).
	bad["path"] = "/leave-balance"
	bad["input_schema"] = json.RawMessage(`{"type":"object","properties":{},"required":["nope"]}`)
	if code, _ := call("POST", "/api/mcp/rest-tools", adminSess, bad); code != 400 {
		t.Fatalf("bad schema: got %d, want 400", code)
	}

	code, created := call("POST", "/api/mcp/rest-tools", adminSess, newTool)
	if code != 201 {
		t.Fatalf("create REST tool: got %d (%v), want 201", code, created)
	}
	id, _ := created["id"].(string)
	if id == "" || created["has_token"] != true {
		t.Fatalf("create response = %+v, want id + has_token true", created)
	}
	// Duplicate name.
	if code, _ := call("POST", "/api/mcp/rest-tools", adminSess, newTool); code != 409 {
		t.Fatalf("duplicate REST tool name: got %d, want 409", code)
	}
	// List: the credential is never echoed back, and the seeded demo
	// tool is present alongside the new one.
	code, listed := call("GET", "/api/mcp/rest-tools", adminSess, nil)
	if code != 200 {
		t.Fatalf("list REST tools: got %d, want 200", code)
	}
	tools, _ := listed["rest_tools"].([]any)
	names := map[string]bool{}
	for _, x := range tools {
		m, _ := x.(map[string]any)
		names[m["name"].(string)] = true
		if strings.Contains(m["name"].(string), "leave_balance") {
			if _, hasToken := m["token"]; hasToken {
				t.Fatalf("list must never return the credential field")
			}
			if _, hasEnc := m["auth_token_enc"]; hasEnc {
				t.Fatalf("list must never return the encrypted credential field")
			}
		}
	}
	if !names["leave_balance_test"] || !names["leave_balance"] {
		t.Fatalf("list missing tools: %v", names)
	}
	// The credential is encrypted at rest, not stored in plaintext.
	var enc string
	st.Read(func(s *store.State) {
		enc = s.RESTTools[id].AuthTokenEnc
	})
	if enc == "" || strings.Contains(enc, "hr-secret") {
		t.Fatalf("credential must be stored encrypted, got %q", enc)
	}

	// Delete unknown id.
	if code, _ := call("DELETE", "/api/mcp/rest-tools/rt_nope", adminSess, nil); code != 404 {
		t.Fatalf("delete unknown REST tool: got %d, want 404", code)
	}
	if code, _ := call("DELETE", "/api/mcp/rest-tools/"+id, adminSess, nil); code != 200 {
		t.Fatalf("delete REST tool: got %d, want 200", code)
	}
}

func TestRESTToolTestEndpoint(t *testing.T) {
	srv, st, adminSess, _ := setupTokensAPI(t)

	call := func(method, path, sess string, body any) (int, map[string]any) {
		t.Helper()
		var buf *bytes.Buffer
		if body != nil {
			b, _ := json.Marshal(body)
			buf = bytes.NewBuffer(b)
		} else {
			buf = bytes.NewBuffer(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, buf)
		req.Header.Set("Content-Type", "application/json")
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// A live fake REST endpoint for the success path.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"employee_id":"lixiaofeng","balance_days":12}`))
	}))
	t.Cleanup(fake.Close)

	code, created := call("POST", "/api/mcp/rest-tools", adminSess, map[string]any{
		"name": "test_balance", "base_url": fake.URL, "method": "GET", "path": "/leave",
		"input_schema": json.RawMessage(`{"type":"object","properties":{"employee_id":{"type":"string"}},"required":["employee_id"]}`),
	})
	if code != 201 {
		t.Fatalf("create REST tool: got %d (%v), want 201", code, created)
	}
	id, _ := created["id"].(string)

	// Valid sample arguments: live call, preview returned.
	code, out := call("POST", "/api/mcp/rest-tools/"+id+"/test", adminSess,
		map[string]any{"arguments": map[string]any{"employee_id": "lixiaofeng"}})
	if code != 200 || out["ok"] != true {
		t.Fatalf("test live REST tool: status=%d out=%+v, want ok", code, out)
	}
	if preview, _ := out["preview"].(string); !strings.Contains(preview, "balance_days") {
		t.Fatalf("preview = %q, want upstream answer", preview)
	}
	// Missing required argument: 400, no upstream call.
	code, out = call("POST", "/api/mcp/rest-tools/"+id+"/test", adminSess,
		map[string]any{"arguments": map[string]any{}})
	if code != 400 || out["ok"] != false {
		t.Fatalf("test with bad arguments: status=%d out=%+v, want 400 + ok false", code, out)
	}
	// Unknown tool id.
	if code, _ := call("POST", "/api/mcp/rest-tools/rt_nope/test", adminSess,
		map[string]any{"arguments": map[string]any{}}); code != 404 {
		t.Fatalf("test unknown REST tool: got %d, want 404", code)
	}
	// The seeded demo tool points at a dead port in tests: 502.
	var seededID string
	st.Read(func(s *store.State) {
		seededID = s.RESTTools["rt_leave_balance"].ID
	})
	code, out = call("POST", "/api/mcp/rest-tools/"+seededID+"/test", adminSess,
		map[string]any{"arguments": map[string]any{"employee_id": "lixiaofeng"}})
	if code != 502 || out["ok"] != false {
		t.Fatalf("test unreachable REST tool: status=%d out=%+v, want 502 + ok false", code, out)
	}
}
