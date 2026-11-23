package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/store"
)

func loginAs(t *testing.T, srvURL, username, password string) string {
	t.Helper()
	body := strings.NewReader(`{"username":"` + username + `","password":"` + password + `"}`)
	resp, err := http.Post(srvURL+"/api/login", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d struct {
		Session string `json:"session"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&d)
	if resp.StatusCode != 200 || d.Session == "" {
		t.Fatalf("login %s failed: %d", username, resp.StatusCode)
	}
	return d.Session
}

func callAPI(t *testing.T, srvURL, method, path, sess string, body any) (int, map[string]any) {
	t.Helper()
	var buf *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req, _ := http.NewRequest(method, srvURL+path, buf)
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

func usersByName(t *testing.T, srvURL, sess string) map[string]map[string]any {
	t.Helper()
	code, out := callAPI(t, srvURL, "GET", "/api/users", sess, nil)
	if code != 200 {
		t.Fatalf("list users: got %d, want 200 (%v)", code, out)
	}
	byName := map[string]map[string]any{}
	users, _ := out["users"].([]any)
	for _, u := range users {
		m, _ := u.(map[string]any)
		byName[m["username"].(string)] = m
	}
	return byName
}

func TestUsersAPI(t *testing.T) {
	srv, st, adminSess, empSess := setupTokensAPI(t)
	audSess := loginAs(t, srv.URL, "auditor", "demo12345")
	opsSess := loginAs(t, srv.URL, "ops", "demo12345")

	// Seed now carries five demo users (F26): admin, auditor,
	// operator, two employees. The list never leaks a hash.
	code, out := callAPI(t, srv.URL, "GET", "/api/users", adminSess, nil)
	if code != 200 {
		t.Fatalf("list users: got %d, want 200", code)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "password_hash") || strings.Contains(string(raw), "pbkdf2$") {
		t.Fatal("list users leaks password hashes")
	}
	users, _ := out["users"].([]any)
	if len(users) != 5 {
		t.Fatalf("users = %d, want 5", len(users))
	}
	byName := usersByName(t, srv.URL, adminSess)
	if byName["auditor"]["role"] != "auditor" || byName["ops"]["role"] != "operator" {
		t.Fatalf("seed roles wrong: auditor=%v ops=%v", byName["auditor"]["role"], byName["ops"]["role"])
	}
	if byName["admin"]["role_source"] != "local" {
		t.Fatalf("admin role_source = %v, want local", byName["admin"]["role_source"])
	}

	// The auditor may read audit but may not list users; the
	// operator may not either. Both are 403, not 401.
	if code, _ := callAPI(t, srv.URL, "GET", "/api/users", audSess, nil); code != 403 {
		t.Fatalf("auditor list users: got %d, want 403", code)
	}
	if code, _ := callAPI(t, srv.URL, "GET", "/api/users", opsSess, nil); code != 403 {
		t.Fatalf("operator list users: got %d, want 403", code)
	}
	if code, _ := callAPI(t, srv.URL, "GET", "/api/users", empSess, nil); code != 403 {
		t.Fatalf("employee list users: got %d, want 403", code)
	}

	// Role matrix endpoint (console copy of docs/roles.md).
	if code, out := callAPI(t, srv.URL, "GET", "/api/users/roles", adminSess, nil); code != 200 {
		t.Fatalf("list roles: got %d, want 200", code)
	} else if rs, _ := out["roles"].([]any); len(rs) != 5 {
		t.Fatalf("roles = %d, want 5", len(rs))
	}

	// Promote the auditor to operator: allowed, audited, and the
	// account becomes locally managed so OIDC never overwrites it.
	audID, _ := byName["auditor"]["id"].(string)
	code, updated := callAPI(t, srv.URL, "PATCH", "/api/users/"+audID, adminSess,
		map[string]any{"role": "operator"})
	if code != 200 {
		t.Fatalf("promote auditor: got %d (%v), want 200", code, updated)
	}
	if updated["role"] != "operator" || updated["role_source"] != "local" {
		t.Fatalf("promoted user = %+v, want role operator / source local", updated)
	}
	// The change is in the audit log (role before/after, no secrets).
	code, auditOut := callAPI(t, srv.URL, "GET", "/api/audit?kind=admin", adminSess, nil)
	if code != 200 {
		t.Fatalf("query audit: got %d, want 200", code)
	}
	found := false
	if events, _ := auditOut["events"].([]any); events != nil {
		for _, e := range events {
			m, _ := e.(map[string]any)
			d, _ := m["detail"].(map[string]any)
			if m["action"] == "user_updated" && d["target"] == "auditor" &&
				d["role_from"] == "auditor" && d["role_to"] == "operator" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("user_updated audit event missing: %v", auditOut)
	}
	// Promote back so later tests see the seed auditor again.
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+audID, adminSess,
		map[string]any{"role": "auditor"}); code != 200 {
		t.Fatalf("restore auditor role: got %d, want 200", code)
	}

	// Unknown role, unknown user.
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+audID, adminSess,
		map[string]any{"role": "root"}); code != 400 {
		t.Fatalf("unknown role: got %d, want 400", code)
	}
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/usr-nope", adminSess,
		map[string]any{"role": "operator"}); code != 404 {
		t.Fatalf("unknown user: got %d, want 404", code)
	}

	// Nobody changes their own role or disables themselves.
	adminID, _ := byName["admin"]["id"].(string)
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+adminID, adminSess,
		map[string]any{"role": "employee"}); code != 400 {
		t.Fatalf("self role change: got %d, want 400", code)
	}
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+adminID, adminSess,
		map[string]any{"disabled": true}); code != 400 {
		t.Fatalf("self disable: got %d, want 400", code)
	}

	// Role changes are admin-only: the auditor and the operator
	// both get 403 (their sessions are still valid here).
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+audID, audSess,
		map[string]any{"role": "employee"}); code != 403 {
		t.Fatalf("auditor patch user: got %d, want 403", code)
	}
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+audID, opsSess,
		map[string]any{"role": "employee"}); code != 403 {
		t.Fatalf("operator patch user: got %d, want 403", code)
	}

	// The last enabled admin cannot be demoted or disabled. Add a
	// second admin directly, demote the first (allowed), then the
	// second demotion / disable must fail.
	var second *store.User
	h, _ := auth.HashPassword("demo12345")
	st.WithLock(func(s *store.State) {
		second = &store.User{ID: store.NewID("usr"), Username: "admin2",
			DisplayName: "二管", Department: "IT", Role: "admin", PasswordHash: h}
		s.Users[second.ID] = second
	})
	secondSess := loginAs(t, srv.URL, "admin2", "demo12345")
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+adminID, secondSess,
		map[string]any{"role": "employee"}); code != 200 {
		t.Fatalf("demote first of two admins: got %d, want 200", code)
	}
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+second.ID, secondSess,
		map[string]any{"role": "employee"}); code != 400 {
		t.Fatalf("demote last admin: got %d, want 400", code)
	}
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+second.ID, secondSess,
		map[string]any{"disabled": true}); code != 400 {
		t.Fatalf("disable last admin: got %d, want 400", code)
	}
	// Restore the seed admin so the server is left usable.
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+adminID, secondSess,
		map[string]any{"role": "admin"}); code != 200 {
		t.Fatalf("restore seed admin: got %d, want 200", code)
	}

	// Disabling a user revokes their open sessions at once.
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+audID, adminSess,
		map[string]any{"disabled": true}); code != 200 {
		t.Fatalf("disable auditor: got %d, want 200", code)
	}
	if code, _ := callAPI(t, srv.URL, "GET", "/api/me", audSess, nil); code != 401 {
		t.Fatalf("disabled user session: got %d, want 401", code)
	}
	// Re-enable for a clean end state.
	if code, _ := callAPI(t, srv.URL, "PATCH", "/api/users/"+audID, adminSess,
		map[string]any{"disabled": false}); code != 200 {
		t.Fatalf("re-enable auditor: got %d, want 200", code)
	}
	if code, _ := callAPI(t, srv.URL, "GET", "/api/me", loginAs(t, srv.URL, "auditor", "demo12345"), nil); code != 200 {
		t.Fatalf("re-enabled auditor login: got %d, want 200", code)
	}
}
