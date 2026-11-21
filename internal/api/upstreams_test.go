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

func TestUpstreamsCRUD(t *testing.T) {
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

	// Employee may not register an upstream.
	if code, _ := call("POST", "/api/mcp/upstreams", empSess, map[string]any{
		"name": "hr", "url": "https://hr.example.invalid/mcp"}); code != 403 {
		t.Fatalf("employee create upstream: got %d, want 403", code)
	}
	// Invalid namespace.
	if code, _ := call("POST", "/api/mcp/upstreams", adminSess, map[string]any{
		"name": "HR!", "url": "https://hr.example.invalid/mcp"}); code != 400 {
		t.Fatalf("bad namespace: got %d, want 400", code)
	}
	// Non-https, non-loopback URL.
	if code, _ := call("POST", "/api/mcp/upstreams", adminSess, map[string]any{
		"name": "hr", "url": "http://hr.example.invalid/mcp"}); code != 400 {
		t.Fatalf("plain-http upstream: got %d, want 400", code)
	}

	code, created := call("POST", "/api/mcp/upstreams", adminSess, map[string]any{
		"name": "hr", "url": "https://hr.example.invalid/mcp",
		"token": "upstream-secret", "max_data_level": "internal", "min_tier": "byoa"})
	if code != 201 {
		t.Fatalf("create upstream: got %d (%v), want 201", code, created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("create upstream returned no id")
	}
	// The token is stored encrypted and never returned.
	var stored *store.UpstreamServer
	st.Read(func(s *store.State) { stored = s.Upstreams[id] })
	if stored == nil || stored.AuthTokenEnc == "" || stored.AuthTokenEnc == "upstream-secret" {
		t.Fatal("upstream token must be stored encrypted")
	}
	if code, _ := call("POST", "/api/mcp/upstreams", adminSess, map[string]any{
		"name": "hr", "url": "https://hr2.example.invalid/mcp"}); code != 409 {
		t.Fatalf("duplicate namespace: got %d, want 409", code)
	}

	code, listed := call("GET", "/api/mcp/upstreams", adminSess, nil)
	if code != 200 {
		t.Fatalf("list upstreams: got %d, want 200", code)
	}
	raw, _ := json.Marshal(listed)
	if strings.Contains(string(raw), "upstream-secret") {
		t.Fatal("list response leaks the upstream token")
	}
	if !strings.Contains(string(raw), `"has_token":true`) {
		t.Fatalf("list should report has_token=true, got %v", listed)
	}

	// Test against a dead URL fails closed with a generic error.
	if code, out := call("POST", "/api/mcp/upstreams/"+id+"/test", adminSess, map[string]any{}); code != 502 || out["error"] != "upstream_error" {
		t.Fatalf("test dead upstream: got %d %v, want 502 upstream_error", code, out)
	}
	// Unknown id.
	if code, _ := call("POST", "/api/mcp/upstreams/up_nope/test", adminSess, map[string]any{}); code != 404 {
		t.Fatalf("test unknown upstream: got %d, want 404", code)
	}
	if code, _ := call("DELETE", "/api/mcp/upstreams/"+id, adminSess, nil); code != 200 {
		t.Fatalf("delete upstream: got %d, want 200", code)
	}
	if code, _ := call("DELETE", "/api/mcp/upstreams/"+id, adminSess, nil); code != 404 {
		t.Fatalf("delete again: got %d, want 404", code)
	}
}

func TestUpstreamTestEndpointLive(t *testing.T) {
	srv, _, adminSess, _ := setupTokensAPI(t)

	// A live fake upstream answering tools/list.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
			"tools": []map[string]any{{"name": "leave_balance", "description": "d",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}})
	}))
	defer fake.Close()

	body, _ := json.Marshal(map[string]any{"name": "hr", "url": fake.URL})
	req, _ := http.NewRequest("POST", srv.URL+"/api/mcp/upstreams", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminSess)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create upstream: got %d (%v)", resp.StatusCode, created)
	}
	id, _ := created["id"].(string)

	req, _ = http.NewRequest("POST", srv.URL+"/api/mcp/upstreams/"+id+"/test", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminSess)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool     `json:"ok"`
		Tools []string `json:"tools"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || !out.OK || len(out.Tools) != 1 || out.Tools[0] != "hr.leave_balance" {
		t.Fatalf("test live upstream: status=%d out=%+v, want ok + [hr.leave_balance]", resp.StatusCode, out)
	}
}
