package auth_test

import (
	"testing"

	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/store"
)

// mapper builds a RoleFromGroups function from a static group->role
// table, the way cmd/agentgate wires AGENTGATE_OIDC_ROLE_MAP.
func mapper(table map[string]string) func([]string) (string, bool) {
	return func(groups []string) (string, bool) {
		for _, g := range groups {
			if r, ok := table[g]; ok {
				return r, true
			}
		}
		return "", false
	}
}

func TestLoginOIDCRoleMapping(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.New(st)
	svc.RoleFromGroups = mapper(map[string]string{
		"auditors": "auditor",
		"ops":      "operator",
	})

	// JIT with a mapped group gets that role, marked IdP-managed.
	user, err := svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-aud-1",
		PreferredUsername: "aud1", Groups: []string{"auditors"}})
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "auditor" || user.RoleSource != "oidc" {
		t.Fatalf("JIT auditor: role=%q source=%q, want auditor/oidc", user.Role, user.RoleSource)
	}
	// JIT with an unmapped group stays employee.
	user, err = svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-emp-1",
		PreferredUsername: "emp1", Groups: []string{"randoms"}})
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "employee" {
		t.Fatalf("JIT unmapped: role=%q, want employee", user.Role)
	}
	// JIT with several groups: the first mapped group wins.
	user, err = svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-ops-1",
		PreferredUsername: "ops1", Groups: []string{"ops", "auditors"}})
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "operator" {
		t.Fatalf("JIT multi-group: role=%q, want operator", user.Role)
	}

	// Membership change syncs the role on the next login: leaving
	// the auditors group drops the user back to employee.
	user, err = svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-aud-1",
		PreferredUsername: "aud1", Groups: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "employee" {
		t.Fatalf("group left: role=%q, want employee", user.Role)
	}
	// And rejoining restores it.
	user, err = svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-aud-1",
		PreferredUsername: "aud1", Groups: []string{"auditors"}})
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "auditor" {
		t.Fatalf("group rejoined: role=%q, want auditor", user.Role)
	}

	// A role an admin assigned locally is never overwritten by the
	// IdP: promote the user in the store, then log in with the
	// auditors group again.
	st.WithLock(func(s *store.State) {
		if u, ok := s.Users[user.ID]; ok {
			u.Role = "operator"
			u.RoleSource = "local"
		}
	})
	user, err = svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-aud-1",
		PreferredUsername: "aud1", Groups: []string{"auditors"}})
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "operator" {
		t.Fatalf("local override: role=%q, want operator (kept)", user.Role)
	}

	// Legacy OIDC rows (empty RoleSource) are treated as IdP-managed.
	st.WithLock(func(s *store.State) {
		if u, ok := s.Users[user.ID]; ok {
			u.Role = "employee"
			u.RoleSource = ""
		}
	})
	user, err = svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-aud-1",
		PreferredUsername: "aud1", Groups: []string{"auditors"}})
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "auditor" {
		t.Fatalf("legacy row: role=%q, want auditor", user.Role)
	}
}

func TestLoginOIDCRoleMappingNoEscalation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.New(st)
	// A miswired mapper that returns admin must not escalate: the
	// auth layer only accepts auditor / operator / employee.
	svc.RoleFromGroups = func(groups []string) (string, bool) { return "admin", true }
	user, err := svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-esc-1",
		PreferredUsername: "esc1", Groups: []string{"admins"}})
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "employee" {
		t.Fatalf("escalating mapper: role=%q, want employee", user.Role)
	}

	// With no mapper configured, OIDC logins never change a role.
	svc.RoleFromGroups = nil
	user, err = svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-esc-1",
		PreferredUsername: "esc1", Groups: []string{"auditors"}})
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "employee" {
		t.Fatalf("no mapper: role=%q, want employee", user.Role)
	}
}
