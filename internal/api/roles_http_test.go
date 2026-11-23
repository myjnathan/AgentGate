package api_test

import (
	"testing"
)

// TestRolePermissionsHTTP pins the F26 permission matrix at the HTTP
// layer: the auditor reads audit and reports but cannot operate, the
// operator runs channels and tokens but cannot read audit, the
// employee can do neither, and the key operations (approve an
// exception, archive audit, delete a report) stay admin/security.
func TestRolePermissionsHTTP(t *testing.T) {
	srv, _, adminSess, empSess := setupTokensAPI(t)
	audSess := loginAs(t, srv.URL, "auditor", "demo12345")
	opsSess := loginAs(t, srv.URL, "ops", "demo12345")

	want := func(sess string, method, path string, body any, code int) {
		t.Helper()
		if got, out := callAPI(t, srv.URL, method, path, sess, body); got != code {
			t.Fatalf("%s %s as %s: got %d (%v), want %d", method, path,
				map[string]string{adminSess: "admin", audSess: "auditor", opsSess: "operator", empSess: "employee"}[sess], got, out, code)
		}
	}

	// Audit read: admin + auditor yes; operator + employee no.
	for _, sess := range []string{adminSess, audSess} {
		want(sess, "GET", "/api/audit", nil, 200)
		want(sess, "GET", "/api/audit/verify", nil, 200)
		want(sess, "GET", "/api/stats", nil, 200)
		want(sess, "GET", "/api/attestation/reports", nil, 200)
		want(sess, "GET", "/api/sidecar/stats", nil, 200)
	}
	for _, sess := range []string{opsSess, empSess} {
		want(sess, "GET", "/api/audit", nil, 403)
		want(sess, "GET", "/api/stats", nil, 403)
		want(sess, "GET", "/api/attestation/reports", nil, 403)
	}
	// Audit export: auditor yes, operator no.
	want(audSess, "GET", "/api/audit/export", nil, 200)
	want(opsSess, "GET", "/api/audit/export", nil, 403)
	// Audit archive (destructive): admin only.
	want(adminSess, "POST", "/api/audit/archive", map[string]any{"before": "2001-01-01T00:00:00Z"}, 200)
	want(audSess, "POST", "/api/audit/archive", map[string]any{"before": "2001-01-01T00:00:00Z"}, 403)
	want(opsSess, "POST", "/api/audit/archive", map[string]any{"before": "2001-01-01T00:00:00Z"}, 403)

	// Reports: the auditor generates and reads; deleting stays admin.
	want(audSess, "GET", "/api/reports", nil, 200)
	want(opsSess, "GET", "/api/reports", nil, 403)
	want(empSess, "GET", "/api/reports", nil, 403)
	_, rep := callAPI(t, srv.URL, "POST", "/api/reports", audSess, map[string]any{"period": "weekly"})
	repID, _ := rep["id"].(string)
	if repID == "" {
		t.Fatalf("auditor generate report failed: %v", rep)
	}
	want(audSess, "GET", "/api/reports/"+repID, nil, 200)
	want(audSess, "GET", "/api/reports/"+repID+"/export", nil, 200)
	want(opsSess, "GET", "/api/reports/"+repID+"/export", nil, 403)
	want(audSess, "DELETE", "/api/reports/"+repID, nil, 403)
	want(adminSess, "DELETE", "/api/reports/"+repID, nil, 200)

	// Channels and tokens: operator yes, auditor no.
	want(opsSess, "GET", "/api/channels", nil, 200)
	want(audSess, "GET", "/api/channels", nil, 403)
	want(audSess, "POST", "/api/channels", map[string]any{
		"name": "aud-try", "base_url": "https://llm.example.invalid/v1",
		"models": []string{"mock-eco"}}, 403)
	_, created := callAPI(t, srv.URL, "POST", "/api/channels", opsSess, map[string]any{
		"name": "ops-channel", "base_url": "https://llm.example.invalid/v1",
		"models": []string{"mock-eco"}})
	if created["id"] == nil {
		t.Fatalf("operator create channel failed: %v", created)
	}
	want(opsSess, "GET", "/api/tokens", nil, 200)
	want(audSess, "GET", "/api/tokens", nil, 403)
	want(empSess, "GET", "/api/tokens", nil, 403)
	// Cache and OTel are operator territory too. This harness wires
	// no cache, so the operator gets 503 "not configured" (past the
	// role guard) while the auditor gets 403.
	want(opsSess, "GET", "/api/cache", nil, 503)
	want(opsSess, "POST", "/api/cache/clear", nil, 503)
	want(audSess, "GET", "/api/cache", nil, 403)
	want(audSess, "POST", "/api/cache/clear", nil, 403)
	want(opsSess, "GET", "/api/otel", nil, 200)
	want(audSess, "GET", "/api/otel", nil, 403)

	// Exception approval is a key operation: admin/security only.
	// The employee files one; the auditor and the operator may not
	// approve it, the admin may.
	_, exc := callAPI(t, srv.URL, "POST", "/api/exceptions", empSess, map[string]any{
		"data_level": "confidential", "reason": "客户现场演示需要",
		"scope": "演示会话", "hours": 2})
	excID, _ := exc["id"].(string)
	if excID == "" {
		t.Fatalf("create exception failed: %v", exc)
	}
	want(audSess, "POST", "/api/exceptions/"+excID+"/approve", map[string]any{}, 403)
	want(opsSess, "POST", "/api/exceptions/"+excID+"/approve", map[string]any{}, 403)
	want(empSess, "POST", "/api/exceptions/"+excID+"/approve", map[string]any{}, 403)
	if code, out := callAPI(t, srv.URL, "POST", "/api/exceptions/"+excID+"/approve", adminSess, map[string]any{}); code != 200 {
		t.Fatalf("admin approve exception: got %d (%v), want 200", code, out)
	}
	// Attestation policies are policy: operator may not touch them.
	want(opsSess, "POST", "/api/attestation/policies", map[string]any{
		"agent_type": "dsh", "min_version": "0.1.0"}, 403)
	want(audSess, "POST", "/api/attestation/policies", map[string]any{
		"agent_type": "dsh", "min_version": "0.1.0"}, 403)
}
