package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTerminalHeartbeatAndInventory(t *testing.T) {
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
		// The device code is approved by the employee the token
		// belongs to (lixiaofeng), not by the admin: approval binds
		// the token to the approving user.
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
	hb := func(tok, version string, extra map[string]any) (int, map[string]any) {
		body := map[string]any{"version": version, "config_hash": "abababababababababababababababababababababababababababababababab",
			"plugins": []string{}, "guard_applied": true, "os": "linux"}
		for k, v := range extra {
			body[k] = v
		}
		return call("POST", "/api/terminals/heartbeat", tok, body)
	}
	terminals := func(query string) []any {
		code, out := call("GET", "/api/terminals"+query, adminSess, nil)
		if code != 200 {
			t.Fatalf("list terminals %q: got %d %v", query, code, out)
		}
		list, _ := out["terminals"].([]any)
		return list
	}

	// The sidecar policy snapshot is served to any valid token.
	if code, _ := call("GET", "/api/sidecar/policy", "", nil); code != 401 {
		t.Fatalf("policy without auth: want 401, got %d", code)
	}
	code, pol := call("GET", "/api/sidecar/policy", empSess, nil)
	if code != 200 {
		t.Fatalf("policy: got %d %v", code, pol)
	}
	if pol["version"] == "" || pol["offline_max_level"] != "internal" {
		t.Fatalf("bad policy snapshot: %v", pol)
	}
	banned, _ := pol["banned_words"].([]any)
	found := false
	for _, w := range banned {
		if w == "test_sensitive" {
			found = true
		}
	}
	if !found {
		t.Fatalf("policy must carry the demo banned word, got %v", banned)
	}

	dshTok := mintToken("dsh", "byoa")
	ccTok := mintToken("claude-code", "byoa")

	// Validation: bad version and bad hash are rejected.
	if code, _ := hb(dshTok, "soon", nil); code != 400 {
		t.Fatalf("bad version: want 400, got %d", code)
	}
	if code, _ := hb(dshTok, "0.1.0", map[string]any{"config_hash": "zz"}); code != 400 {
		t.Fatalf("bad hash: want 400, got %d", code)
	}

	// First heartbeat registers the terminal: the seeded dsh policy
	// (min 0.1.0) passes, so no drift.
	code, out := hb(dshTok, "0.1.0", nil)
	if code != 200 {
		t.Fatalf("heartbeat: got %d %v", code, out)
	}
	termID, _ := out["terminal_id"].(string)
	if termID == "" || out["status"] != "online" {
		t.Fatalf("want registered online terminal, got %v", out)
	}
	if reasons, _ := out["drift_reasons"].([]any); len(reasons) != 0 {
		t.Fatalf("want no drift, got %v", reasons)
	}
	if code, _ := call("GET", "/api/terminals", empSess, nil); code != 403 {
		t.Fatalf("employee list: want 403, got %d", code)
	}
	list := terminals("")
	if len(list) != 1 {
		t.Fatalf("want 1 terminal, got %d", len(list))
	}
	code, one := call("GET", "/api/terminals/"+termID, adminSess, nil)
	if code != 200 || one["agent_type"] != "dsh" || one["username"] != "lixiaofeng" {
		t.Fatalf("detail: got %d %v", code, one)
	}
	if code, _ := call("GET", "/api/terminals/term_missing", adminSess, nil); code != 404 {
		t.Fatalf("missing terminal: want 404, got %d", code)
	}

	// A version below the policy minimum is drift.
	code, out = hb(dshTok, "0.0.9", nil)
	if code != 200 {
		t.Fatalf("drift heartbeat: got %d %v", code, out)
	}
	reasons, _ := out["drift_reasons"].([]any)
	if len(reasons) != 1 || reasons[0] != "version_below_minimum" {
		t.Fatalf("want [version_below_minimum], got %v", reasons)
	}
	if got := terminals("?drift=true"); len(got) != 1 {
		t.Fatalf("drift filter: want 1, got %d", len(got))
	}
	if got := terminals("?drift=false"); len(got) != 0 {
		t.Fatalf("no-drift filter: want 0, got %d", len(got))
	}
	if got := terminals("?version=0.0.9"); len(got) != 1 {
		t.Fatalf("version filter: want 1, got %d", len(got))
	}
	if got := terminals("?department=研发部"); len(got) != 1 {
		t.Fatalf("department filter: want 1, got %d", len(got))
	}
	if got := terminals("?department=注册部"); len(got) != 0 {
		t.Fatalf("other department filter: want 0, got %d", len(got))
	}
	if got := terminals("?status=online"); len(got) != 1 {
		t.Fatalf("status filter: want 1, got %d", len(got))
	}
	if got := terminals("?agent_type=claude-code"); len(got) != 0 {
		t.Fatalf("agent filter: want 0, got %d", len(got))
	}
	if code, _ := call("GET", "/api/terminals?status=bogus", adminSess, nil); code != 400 {
		t.Fatalf("bad status filter: want 400, got %d", code)
	}

	// An agent type with no policy never drifts, whatever it reports.
	if code, out := hb(ccTok, "0.0.1", nil); code != 200 {
		t.Fatalf("claude-code heartbeat: got %d %v", code, out)
	} else if reasons, _ := out["drift_reasons"].([]any); len(reasons) != 0 {
		t.Fatalf("no-policy agent must not drift, got %v", reasons)
	}

	// Fixing the version clears the drift, and the offline backfill
	// is audited with counts only.
	now := time.Now()
	code, out = hb(dshTok, "0.1.0", map[string]any{"offline": map[string]any{
		"events": 2, "blocked": 1, "unreachable": 1,
		"window_start": now.Add(-10 * time.Minute).Format(time.RFC3339),
		"window_end":   now.Format(time.RFC3339),
		"by_category":  map[string]int64{"banned_word": 1, "pii:email": 1},
	}})
	if code != 200 {
		t.Fatalf("backfill heartbeat: got %d %v", code, out)
	}
	if reasons, _ := out["drift_reasons"].([]any); len(reasons) != 0 {
		t.Fatalf("want drift cleared, got %v", reasons)
	}
	code, auditOut := call("GET", "/api/audit?kind=terminal", adminSess, nil)
	if code != 200 {
		t.Fatalf("audit query: got %d", code)
	}
	events, _ := auditOut["events"].([]any)
	seen := map[string]bool{}
	for _, e := range events {
		m, _ := e.(map[string]any)
		seen[m["action"].(string)] = true
	}
	for _, want := range []string{"terminal_registered", "terminal_drift", "terminal_drift_cleared", "terminal_offline_audit"} {
		if !seen[want] {
			t.Fatalf("want audit action %s in %v", want, seen)
		}
	}
	// The backfill detail carries counts, never content.
	for _, e := range events {
		m, _ := e.(map[string]any)
		if m["action"] == "terminal_offline_audit" {
			d, _ := m["detail"].(map[string]any)
			if d["events"] != "2" || d["blocked"] != "1" || d["unreachable"] != "1" {
				t.Fatalf("bad offline audit detail: %v", d)
			}
		}
	}

	// A session heartbeat (no token) falls back to the body's agent
	// type; the same (user, agent) key still upserts one record.
	if code, out := hb(adminSess, "1.2.3", map[string]any{"agent_type": "codex"}); code != 200 {
		t.Fatalf("session heartbeat: got %d %v", code, out)
	} else if out["terminal_id"] == termID {
		t.Fatalf("session heartbeat must be a different terminal record")
	}
	if got := terminals(""); len(got) != 3 {
		t.Fatalf("want 3 terminals (dsh, claude-code, codex), got %d", len(got))
	}
}

// TestTerminalHeartbeatPluginDrift covers the F38 heartbeat wiring:
// the same plugin evaluation as login approval runs on every
// heartbeat, drift reasons carry the plugin codes, and denylist
// unknown plugins come back in plugin_unknown.
func TestTerminalHeartbeatPluginDrift(t *testing.T) {
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
	hb := func(tok string, details []any) (int, map[string]any) {
		body := map[string]any{"version": "0.1.0",
			"config_hash":    "abababababababababababababababababababababababababababababababab",
			"plugins":        []string{},
			"plugin_details": details,
			"guard_applied":  true, "os": "linux"}
		return call("POST", "/api/terminals/heartbeat", tok, body)
	}
	hasReason := func(out map[string]any, want string) bool {
		reasons, _ := out["drift_reasons"].([]any)
		for _, r := range reasons {
			if r == want {
				return true
			}
		}
		return false
	}

	// Allowlist policy: only good-plugin.
	code, created := call("POST", "/api/plugin/policies", adminSess,
		map[string]any{"name": "allow", "mode": "allowlist",
			"scope": map[string]any{"level": "global"}, "action": "downgrade", "enabled": true,
			"rules": []any{map[string]any{"id": "good-plugin"}}})
	if code != 201 {
		t.Fatalf("create allowlist: got %d (%v)", code, created)
	}
	pol, _ := created["policy"].(map[string]any)
	allowID, _ := pol["id"].(string)

	tok := mintToken("dsh", "byoa")
	// Unlisted plugin drifts with plugin_not_allowed.
	code, out := hb(tok, []any{map[string]any{"id": "bad-plugin", "version": "1.0.0"}})
	if code != 200 {
		t.Fatalf("heartbeat: got %d (%v)", code, out)
	}
	if !hasReason(out, "plugin_not_allowed") {
		t.Errorf("drift reasons %v, want plugin_not_allowed", out["drift_reasons"])
	}
	// Listed plugin clears the drift.
	code, out = hb(tok, []any{map[string]any{"id": "good-plugin", "version": "1.0.0"}})
	if code != 200 {
		t.Fatalf("heartbeat: got %d (%v)", code, out)
	}
	if reasons, _ := out["drift_reasons"].([]any); len(reasons) != 0 {
		t.Errorf("drift reasons %v, want none after fix", reasons)
	}
	// Bad hash drifts with plugin_hash_mismatch (version matches).
	code, _ = call("PUT", "/api/plugin/policies/"+allowID, adminSess,
		map[string]any{"name": "allow", "mode": "allowlist",
			"scope": map[string]any{"level": "global"}, "action": "downgrade", "enabled": true,
			"rules": []any{map[string]any{"id": "good-plugin", "hash": strings.Repeat("a", 64)}}})
	if code != 200 {
		t.Fatalf("update policy: got %d", code)
	}
	code, out = hb(tok, []any{map[string]any{"id": "good-plugin", "version": "1.0.0", "hash": strings.Repeat("b", 64)}})
	if code != 200 {
		t.Fatalf("heartbeat: got %d (%v)", code, out)
	}
	if !hasReason(out, "plugin_hash_mismatch") {
		t.Errorf("drift reasons %v, want plugin_hash_mismatch", out["drift_reasons"])
	}
	// Switch to a denylist: an unknown plugin is allowed but
	// reported in plugin_unknown.
	code, _ = call("PUT", "/api/plugin/policies/"+allowID, adminSess,
		map[string]any{"name": "deny", "mode": "denylist",
			"scope": map[string]any{"level": "global"}, "action": "downgrade",
			"warn_unknown": true, "enabled": true,
			"rules": []any{map[string]any{"id": "bad-plugin"}}})
	if code != 200 {
		t.Fatalf("update policy: got %d", code)
	}
	code, out = hb(tok, []any{map[string]any{"id": "newthing", "version": "1.0.0"}})
	if code != 200 {
		t.Fatalf("heartbeat: got %d (%v)", code, out)
	}
	if reasons, _ := out["drift_reasons"].([]any); len(reasons) != 0 {
		t.Errorf("denylist unknown: drift reasons %v, want none", reasons)
	}
	unknown, _ := out["plugin_unknown"].([]any)
	if len(unknown) != 1 || unknown[0] != "newthing" {
		t.Errorf("plugin_unknown %v, want [newthing]", unknown)
	}
}
