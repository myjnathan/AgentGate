package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestAttestationFlow(t *testing.T) {
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
	startDevice := func(agent, tier string) (string, string) {
		t.Helper()
		code, out := call("POST", "/api/setup/device/start", "",
			map[string]any{"agent_type": agent, "tier": tier})
		if code != 200 {
			t.Fatalf("device start: got %d", code)
		}
		return out["code"].(string), out["secret"].(string)
	}
	attest := func(code, secret string, report map[string]any) (int, map[string]any) {
		return call("POST", "/api/setup/device/attest", "",
			map[string]any{"code": code, "secret": secret, "report": report})
	}
	approve := func(code string) (int, map[string]any) {
		return call("POST", "/api/device/approve", adminSess,
			map[string]any{"code": code, "device_name": "test device"})
	}
	poll := func(code, secret string) (int, map[string]any) {
		return call("POST", "/api/setup/device/poll", "",
			map[string]any{"code": code, "secret": secret})
	}
	tokenFor := func(agent, tier string) map[string]any {
		t.Helper()
		_, listed := call("GET", "/api/tokens", adminSess, nil)
		tokens, _ := listed["tokens"].([]any)
		for _, tk := range tokens {
			m, _ := tk.(map[string]any)
			if m["agent_type"] == agent && m["tier"] == tier {
				return m
			}
		}
		t.Fatalf("no %s/%s token in %v", agent, tier, listed)
		return nil
	}

	// The demo seed carries the dsh policy (min 0.1.0, report required).
	code, listed := call("GET", "/api/attestation/policies", adminSess, nil)
	if code != 200 {
		t.Fatalf("list policies: got %d", code)
	}
	policies, _ := listed["policies"].([]any)
	if len(policies) != 1 {
		t.Fatalf("policies = %d, want 1 seeded", len(policies))
	}
	seeded, _ := policies[0].(map[string]any)
	if seeded["agent_type"] != "dsh" || seeded["min_version"] != "0.1.0" || seeded["require_report"] != true {
		t.Fatalf("seeded policy = %+v, want dsh/0.1.0/require_report", seeded)
	}
	if code, _ := call("GET", "/api/attestation/policies", empSess, nil); code != 403 {
		t.Fatalf("employee list policies: got %d, want 403", code)
	}

	// Passing attestation -> managed token, attested.
	dcCode, dcSecret := startDevice("dsh", "managed")
	code, out := attest(dcCode, dcSecret, map[string]any{"version": "0.1.0", "guard_applied": false})
	if code != 200 {
		t.Fatalf("attest good: got %d (%v)", code, out)
	}
	if verdict, _ := out["verdict"].(map[string]any); verdict["status"] != "pass" {
		t.Fatalf("attest good verdict = %+v, want pass", verdict)
	}
	code, out = approve(dcCode)
	if code != 200 || out["issued_tier"] != "managed" {
		t.Fatalf("approve good: got %d (%v), want managed", code, out)
	}
	if code, out := poll(dcCode, dcSecret); code != 200 || out["token"] == "" {
		t.Fatalf("poll good: got %d (%v)", code, out)
	}
	if tk := tokenFor("dsh", "managed"); tk["attested"] != true || tk["attest_verdict"] != "pass" {
		t.Fatalf("managed token = %+v, want attested/pass", tk)
	}

	// Failing attestation (old version) -> BYOA token, downgrade audited.
	dcCode, dcSecret = startDevice("dsh", "managed")
	code, out = attest(dcCode, dcSecret, map[string]any{"version": "0.0.1"})
	if code != 200 {
		t.Fatalf("attest bad: got %d", code)
	}
	verdict, _ := out["verdict"].(map[string]any)
	if verdict["status"] != "downgraded" {
		t.Fatalf("attest bad verdict = %+v, want downgraded", verdict)
	}
	reasons, _ := verdict["reasons"].([]any)
	if len(reasons) != 1 || reasons[0] != "version_below_minimum" {
		t.Fatalf("attest bad reasons = %v, want [version_below_minimum]", reasons)
	}
	code, out = approve(dcCode)
	if code != 200 || out["issued_tier"] != "byoa" {
		t.Fatalf("approve bad: got %d (%v), want byoa", code, out)
	}
	if code, out := poll(dcCode, dcSecret); code != 200 || out["token"] == "" {
		t.Fatalf("poll bad: got %d (%v)", code, out)
	}
	if tk := tokenFor("dsh", "byoa"); tk["attested"] != false || tk["attest_verdict"] != "downgraded" {
		t.Fatalf("downgraded token = %+v, want unattested/downgraded", tk)
	}
	code, reports := call("GET", "/api/attestation/reports", adminSess, nil)
	if code != 200 {
		t.Fatalf("list reports: got %d", code)
	}
	found := false
	for _, ev := range reports["reports"].([]any) {
		if m, _ := ev.(map[string]any); m["action"] == "attest_downgrade" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no attest_downgrade event in reports %v", reports)
	}

	// No report at all while one is required -> BYOA.
	dcCode, dcSecret = startDevice("dsh", "managed")
	code, out = approve(dcCode)
	if code != 200 || out["issued_tier"] != "byoa" {
		t.Fatalf("approve without report: got %d (%v), want byoa", code, out)
	}

	// Attestation validation: wrong secret, non-hex hash, unknown code.
	dcCode, dcSecret = startDevice("dsh", "managed")
	if code, _ := attest(dcCode, "poll_wrongsecret", map[string]any{"version": "0.1.0"}); code != 400 {
		t.Fatalf("attest wrong secret: got %d, want 400", code)
	}
	if code, _ := attest(dcCode, dcSecret, map[string]any{"version": "0.1.0", "config_hash": "zz"}); code != 400 {
		t.Fatalf("attest non-hex hash: got %d, want 400", code)
	}
	if code, _ := attest("AG-NOPE", dcSecret, map[string]any{"version": "0.1.0"}); code != 400 {
		t.Fatalf("attest unknown code: got %d, want 400", code)
	}

	// Policy CRUD: create, reject invalid, delete, employee forbidden.
	code, created := call("POST", "/api/attestation/policies", adminSess,
		map[string]any{"agent_type": "codex", "min_version": "1.0.0", "require_report": true})
	if code != 200 {
		t.Fatalf("create policy: got %d (%v)", code, created)
	}
	if code, _ := call("POST", "/api/attestation/policies", adminSess,
		map[string]any{"agent_type": "codex", "min_version": "1.x"}); code != 400 {
		t.Fatalf("invalid min_version: got %d, want 400", code)
	}
	if code, _ := call("POST", "/api/attestation/policies", empSess,
		map[string]any{"agent_type": "codex", "min_version": "1.0.0"}); code != 403 {
		t.Fatalf("employee upsert policy: got %d, want 403", code)
	}
	if code, _ := call("DELETE", "/api/attestation/policies/codex", adminSess, nil); code != 200 {
		t.Fatalf("delete policy: got %d, want 200", code)
	}
	if code, _ := call("DELETE", "/api/attestation/policies/codex", adminSess, nil); code != 404 {
		t.Fatalf("delete missing policy: got %d, want 404", code)
	}
}
