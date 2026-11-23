package roles_test

import (
	"testing"

	"github.com/myjnathan/agentgate/internal/roles"
)

func TestMatrix(t *testing.T) {
	// Each row: permission -> roles that hold it.
	cases := []struct {
		perm roles.Permission
		want map[string]bool
	}{
		{roles.AuditRead, map[string]bool{"admin": true, "security": true, "auditor": true}},
		{roles.AuditExport, map[string]bool{"admin": true, "security": true, "auditor": true}},
		{roles.ReportsWrite, map[string]bool{"admin": true, "security": true, "auditor": true}},
		{roles.ReportsDelete, map[string]bool{"admin": true, "security": true}},
		{roles.AuditArchive, map[string]bool{"admin": true, "security": true}},
		{roles.Channels, map[string]bool{"admin": true, "security": true, "operator": true}},
		{roles.Tokens, map[string]bool{"admin": true, "security": true, "operator": true}},
		{roles.Cache, map[string]bool{"admin": true, "security": true, "operator": true}},
		{roles.OTel, map[string]bool{"admin": true, "security": true, "operator": true}},
		{roles.Policy, map[string]bool{"admin": true, "security": true}},
		{roles.UsersRead, map[string]bool{"admin": true, "security": true}},
		{roles.UsersWrite, map[string]bool{"admin": true}},
	}
	for _, tc := range cases {
		for _, role := range roles.All() {
			if got := roles.Can(role, tc.perm); got != tc.want[role] {
				t.Errorf("Can(%q, %q) = %v, want %v", role, tc.perm, got, tc.want[role])
			}
		}
		// Employee holds nothing; unknown roles hold nothing.
		if roles.Can("employee", tc.perm) {
			t.Errorf("Can(employee, %q) = true, want false", tc.perm)
		}
		if roles.Can("root", tc.perm) {
			t.Errorf("Can(root, %q) = true, want false", tc.perm)
		}
	}
	// Unknown permission is denied to everyone.
	for _, role := range roles.All() {
		if roles.Can(role, "nope") {
			t.Errorf("Can(%q, unknown) = true, want false", role)
		}
	}
}

func TestValidRoleAndAdmin(t *testing.T) {
	for _, r := range roles.All() {
		if !roles.ValidRole(r) {
			t.Errorf("ValidRole(%q) = false, want true", r)
		}
	}
	if roles.ValidRole("root") || roles.ValidRole("") {
		t.Error("ValidRole accepts an unknown role")
	}
	// requireAdmin semantics preserved: admin + security only.
	if !roles.IsAdminRole("admin") || !roles.IsAdminRole("security") {
		t.Error("IsAdminRole must accept admin and security")
	}
	for _, r := range []string{"auditor", "operator", "employee", ""} {
		if roles.IsAdminRole(r) {
			t.Errorf("IsAdminRole(%q) = true, want false", r)
		}
	}
}

func TestMappableViaIdP(t *testing.T) {
	for _, r := range []string{"auditor", "operator", "employee"} {
		if !roles.MappableViaIdP(r) {
			t.Errorf("MappableViaIdP(%q) = false, want true", r)
		}
	}
	// The IdP must never mint a privileged role, whatever the
	// mapping string says.
	for _, r := range []string{"admin", "security", "root", ""} {
		if roles.MappableViaIdP(r) {
			t.Errorf("MappableViaIdP(%q) = true, want false", r)
		}
	}
}

func TestDescribe(t *testing.T) {
	if len(roles.Descriptions()) != len(roles.All()) {
		t.Fatalf("Descriptions() = %d, want %d", len(roles.Descriptions()), len(roles.All()))
	}
	for _, r := range roles.All() {
		d, ok := roles.Describe(r)
		if !ok || d.Role != r || d.NameZH == "" || d.NameEN == "" || d.SummaryZH == "" || d.SummaryEN == "" {
			t.Errorf("Describe(%q) = %+v, %v", r, d, ok)
		}
	}
	if _, ok := roles.Describe("root"); ok {
		t.Error("Describe(root) should fail")
	}
}
