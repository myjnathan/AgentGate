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

// TestPluginAssets covers the F38 slice 2 asset view: the rows
// aggregate the latest heartbeat evidence, the counts reconcile
// with what the terminals reported, and the filters narrow them.
func TestPluginAssets(t *testing.T) {
	srv, _, adminSess, empSess := setupTokensAPI(t)

	call := func(method, path, auth string, body any) (int, map[string]any) {
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
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
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
	mintToken := func(agent, tier string) string {
		t.Helper()
		_, start := call("POST", "/api/setup/device/start", "", map[string]any{"agent_type": agent, "tier": tier})
		code, _ := start["code"].(string)
		secret, _ := start["secret"].(string)
		if code == "" || secret == "" {
			t.Fatalf("device start failed: %v", start)
		}
		if s, _ := call("POST", "/api/device/approve", empSess, map[string]any{"code": code, "device_name": "test"}); s != 200 {
			t.Fatalf("device approve: got %d", s)
		}
		_, poll := call("POST", "/api/setup/device/poll", "", map[string]any{"code": code, "secret": secret})
		tok, _ := poll["token"].(string)
		if tok == "" {
			t.Fatalf("device poll returned no token: %v", poll)
		}
		return tok
	}

	code, created := call("POST", "/api/plugin/policies", adminSess,
		map[string]any{"name": "allow", "mode": "allowlist",
			"scope": map[string]any{"level": "global"}, "action": "downgrade", "enabled": true,
			"rules": []any{map[string]any{"id": "good-plugin"}}})
	if code != 201 {
		t.Fatalf("create allowlist: got %d (%v)", code, created)
	}

	tok := mintToken("dsh", "byoa")
	body := map[string]any{"version": "0.1.0",
		"config_hash":    "abababababababababababababababababababababababababababababababab",
		"plugins":        []string{},
		"plugin_details": []any{map[string]any{"id": "good-plugin", "version": "1.5.0"}, map[string]any{"id": "bad-plugin", "version": "1.0.0"}},
		"guard_applied":  true, "os": "linux"}
	if code, out := call("POST", "/api/terminals/heartbeat", tok, body); code != 200 {
		t.Fatalf("heartbeat: got %d (%v)", code, out)
	}

	assets := func(query string) (int, map[string]any) {
		return call("GET", "/api/plugin/assets"+query, adminSess, nil)
	}
	findRow := func(out map[string]any, id string) map[string]any {
		rows, _ := out["plugins"].([]any)
		for _, r := range rows {
			m, _ := r.(map[string]any)
			if m["id"] == id {
				return m
			}
		}
		return nil
	}
	code, out := assets("")
	if code != 200 {
		t.Fatalf("assets: got %d (%v)", code, out)
	}
	if total, _ := out["total"].(float64); total != 2 {
		t.Fatalf("total %v, want 2: %v", total, out)
	}
	bad := findRow(out, "bad-plugin")
	if bad == nil || bad["status"] != "violating" || bad["installs"] != float64(1) {
		t.Errorf("bad-plugin row %v, want violating with 1 install", bad)
	}
	good := findRow(out, "good-plugin")
	if good == nil || good["status"] != "compliant" || good["installs"] != float64(1) {
		t.Errorf("good-plugin row %v, want compliant with 1 install", good)
	}
	// The violating row's install names the terminal that
	// reported it, so the count reconciles with the heartbeat.
	installs, _ := bad["terminals"].([]any)
	if len(installs) != 1 {
		t.Fatalf("bad-plugin installs %v, want 1", installs)
	}
	inst, _ := installs[0].(map[string]any)
	if inst["username"] != "lixiaofeng" || inst["version"] != "1.0.0" || inst["status"] != "violating" {
		t.Errorf("bad-plugin install %v, want lixiaofeng/1.0.0/violating", inst)
	}

	code, out = assets("?status=violating")
	if code != 200 || out["total"] != float64(1) || findRow(out, "good-plugin") != nil {
		t.Errorf("status filter: got %d %v, want only bad-plugin", code, out)
	}
	code, out = assets("?q=GOOD")
	if code != 200 || out["total"] != float64(1) || findRow(out, "good-plugin") == nil {
		t.Errorf("q filter (case-insensitive): got %d %v, want only good-plugin", code, out)
	}
	code, out = assets("?department=研发部")
	if code != 200 || out["total"] != float64(2) {
		t.Errorf("department filter: got %d %v, want 2", code, out)
	}
	code, out = assets("?department=注册部")
	if code != 200 || out["total"] != float64(0) {
		t.Errorf("other department filter: got %d %v, want 0", code, out)
	}
	if code, _ := assets("?status=bogus"); code != 400 {
		t.Errorf("bad status filter: got %d, want 400", code)
	}
	if code, _ := call("GET", "/api/plugin/assets", empSess, nil); code != 403 {
		t.Errorf("employee assets: got %d, want 403", code)
	}
}
