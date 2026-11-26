package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPluginPoliciesCRUD(t *testing.T) {
	srv, _, adminSess, empSess := setupTokensAPI(t)

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

	// Empty list to start.
	code, listed := call("GET", "/api/plugin/policies", adminSess, nil)
	if code != 200 {
		t.Fatalf("list: got %d", code)
	}
	if policies, _ := listed["policies"].([]any); len(policies) != 0 {
		t.Fatalf("policies = %d, want 0", len(policies))
	}

	// Employee may not manage policies.
	code, _ = call("GET", "/api/plugin/policies", empSess, nil)
	if code != 403 {
		t.Errorf("employee list: got %d, want 403", code)
	}
	code, _ = call("POST", "/api/plugin/policies", empSess,
		map[string]any{"name": "x", "mode": "allowlist", "scope": map[string]any{"level": "global"}, "action": "downgrade"})
	if code != 403 {
		t.Errorf("employee create: got %d, want 403", code)
	}
	code, _ = call("GET", "/api/plugin/policies", "", nil)
	if code != 401 {
		t.Errorf("anonymous list: got %d, want 401", code)
	}

	// Validation errors, each a 400.
	bad := []map[string]any{
		{"name": "x", "mode": "whatever", "scope": map[string]any{"level": "global"}, "action": "downgrade"},
		{"name": "x", "mode": "denylist", "scope": map[string]any{"level": "global"}, "action": "downgrade", "warn_unknown": false},
		{"name": "x", "mode": "allowlist", "scope": map[string]any{"level": "global"}, "action": "downgrade",
			"rules": []any{map[string]any{"id": "a", "version_constraint": ">=banana"}}},
		{"name": "x", "mode": "allowlist", "scope": map[string]any{"level": "global"}, "action": "downgrade",
			"rules": []any{map[string]any{"id": "a", "hash": "xyz"}}},
		{"name": "x", "mode": "allowlist", "scope": map[string]any{"level": "department"}, "action": "downgrade"},
	}
	for i, body := range bad {
		if code, _ := call("POST", "/api/plugin/policies", adminSess, body); code != 400 {
			t.Errorf("bad policy %d: got %d, want 400", i, code)
		}
	}

	// Create an allowlist policy (server-assigned ID).
	good := map[string]any{
		"name": "dsh allowlist", "mode": "allowlist",
		"scope":  map[string]any{"level": "global"},
		"action": "downgrade", "enabled": true,
		"rules": []any{
			map[string]any{"id": "git-helper", "version_constraint": ">=1.2.0"},
			map[string]any{"id": "vault", "version_constraint": "2.0.0", "hash": strings.Repeat("a", 64), "source": "platform"},
		},
	}
	code, created := call("POST", "/api/plugin/policies", adminSess, good)
	if code != 201 {
		t.Fatalf("create: got %d (%v)", code, created)
	}
	pol, _ := created["policy"].(map[string]any)
	id, _ := pol["id"].(string)
	if id == "" {
		t.Fatalf("create: empty id in %v", created)
	}

	// Duplicate client-supplied ID is a 409.
	good["id"] = id
	if code, _ := call("POST", "/api/plugin/policies", adminSess, good); code != 409 {
		t.Errorf("duplicate id: got %d, want 409", code)
	}
	delete(good, "id")

	// Get one, list, and update.
	code, got := call("GET", "/api/plugin/policies/"+id, adminSess, nil)
	if code != 200 {
		t.Fatalf("get: got %d", code)
	}
	if gp, _ := got["policy"].(map[string]any); gp["name"] != "dsh allowlist" {
		t.Errorf("get: name %v", gp["name"])
	}
	code, _ = call("GET", "/api/plugin/policies/does-not-exist", adminSess, nil)
	if code != 404 {
		t.Errorf("get missing: got %d, want 404", code)
	}
	code, listed = call("GET", "/api/plugin/policies", adminSess, nil)
	if code != 200 {
		t.Fatalf("list after create: got %d", code)
	}
	if policies, _ := listed["policies"].([]any); len(policies) != 1 {
		t.Fatalf("policies = %d, want 1", len(policies))
	}
	updated := map[string]any{
		"name": "dsh allowlist v2", "mode": "denylist",
		"scope":  map[string]any{"level": "global"},
		"action": "warn", "warn_unknown": true, "enabled": true,
		"rules": []any{map[string]any{"id": "bad-plugin"}},
	}
	code, upd := call("PUT", "/api/plugin/policies/"+id, adminSess, updated)
	if code != 200 {
		t.Fatalf("update: got %d (%v)", code, upd)
	}
	if up, _ := upd["policy"].(map[string]any); up["name"] != "dsh allowlist v2" || up["mode"] != "denylist" {
		t.Errorf("update: policy %v", up)
	}
	code, _ = call("PUT", "/api/plugin/policies/does-not-exist", adminSess, updated)
	if code != 404 {
		t.Errorf("update missing: got %d, want 404", code)
	}

	// Delete, then list is empty again; deleting twice 404s.
	code, _ = call("DELETE", "/api/plugin/policies/"+id, adminSess, nil)
	if code != 200 {
		t.Fatalf("delete: got %d", code)
	}
	code, _ = call("DELETE", "/api/plugin/policies/"+id, adminSess, nil)
	if code != 404 {
		t.Errorf("delete twice: got %d, want 404", code)
	}
	code, listed = call("GET", "/api/plugin/policies", adminSess, nil)
	if policies, _ := listed["policies"].([]any); len(policies) != 0 {
		t.Fatalf("policies after delete = %d, want 0", len(policies))
	}
}
