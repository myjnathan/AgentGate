package oidc

import (
	"testing"
)

func TestParseRoleMap(t *testing.T) {
	m, err := ParseRoleMap("")
	if err != nil || m != nil {
		t.Fatalf("empty map: %v, %v", m, err)
	}
	m, err = ParseRoleMap("auditors:auditor,ops:operator,staff:employee")
	if err != nil {
		t.Fatal(err)
	}
	if m["auditors"] != "auditor" || m["ops"] != "operator" || m["staff"] != "employee" {
		t.Fatalf("parsed map = %v", m)
	}
	// Whitespace is tolerated.
	m, err = ParseRoleMap(" auditors : auditor , ops : operator ")
	if err != nil {
		t.Fatal(err)
	}
	if m["auditors"] != "auditor" || m["ops"] != "operator" {
		t.Fatalf("whitespace map = %v", m)
	}
	// The IdP must never mint a privileged role.
	for _, bad := range []string{
		"admins:admin",
		"secadmins:security",
		"root:root",
		"auditors",                           // missing :role
		":auditor",                           // empty group
		"auditors:",                          // empty role
		"auditors:auditor,auditors:operator", // duplicate group
	} {
		if _, err := ParseRoleMap(bad); err == nil {
			t.Errorf("ParseRoleMap(%q) accepted, want rejection", bad)
		}
	}
}

func TestParseGroupsClaim(t *testing.T) {
	p := &Provider{cfg: Config{GroupsClaim: "groups"}}
	claims, err := p.parseClaims([]byte(`{"sub":"s1","groups":["auditors","ops","auditors",""]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(claims.Groups) != 2 || claims.Groups[0] != "auditors" || claims.Groups[1] != "ops" {
		t.Fatalf("groups = %v, want [auditors ops]", claims.Groups)
	}
	// A lone string counts as a one-element array.
	claims, err = p.parseClaims([]byte(`{"sub":"s1","groups":"auditors"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(claims.Groups) != 1 || claims.Groups[0] != "auditors" {
		t.Fatalf("single-string groups = %v", claims.Groups)
	}
	// Non-string members and non-array values are not trusted.
	claims, err = p.parseClaims([]byte(`{"sub":"s1","groups":[1,{"x":1},null]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(claims.Groups) != 0 {
		t.Fatalf("bad groups = %v, want none", claims.Groups)
	}
	// A custom claim name is honoured.
	p2 := &Provider{cfg: Config{GroupsClaim: "teams"}}
	claims, err = p2.parseClaims([]byte(`{"sub":"s1","teams":["ops"],"groups":["auditors"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(claims.Groups) != 1 || claims.Groups[0] != "ops" {
		t.Fatalf("custom claim groups = %v, want [ops]", claims.Groups)
	}
}

func TestMergeClaimsGroups(t *testing.T) {
	dst := &Claims{Groups: []string{"auditors"}}
	mergeClaims(dst, &Claims{Groups: []string{"ops", "auditors"}})
	if len(dst.Groups) != 2 || dst.Groups[0] != "auditors" || dst.Groups[1] != "ops" {
		t.Fatalf("merged groups = %v, want [auditors ops]", dst.Groups)
	}
}

func TestConfigFromEnvRoleMap(t *testing.T) {
	getenv := func(k string) string {
		switch k {
		case "AGENTGATE_OIDC_ISSUER":
			return "https://idp.example.test"
		case "AGENTGATE_OIDC_CLIENT_ID":
			return "client"
		case "AGENTGATE_OIDC_CLIENT_SECRET":
			return "secret"
		case "AGENTGATE_OIDC_ROLE_MAP":
			return "auditors:auditor"
		}
		return ""
	}
	cfg, enabled := ConfigFromEnv(getenv)
	if !enabled {
		t.Fatal("OIDC should be enabled")
	}
	if cfg.RoleMap["auditors"] != "auditor" || cfg.GroupsClaim != "groups" {
		t.Fatalf("role map config = %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid role map rejected: %v", err)
	}
	// An invalid mapping disables OIDC at provider construction
	// (same failure mode as a bad issuer): local login continues.
	getenvBad := func(k string) string {
		if k == "AGENTGATE_OIDC_ROLE_MAP" {
			return "admins:admin"
		}
		return getenv(k)
	}
	bad, _ := ConfigFromEnv(getenvBad)
	if err := bad.Validate(); err == nil {
		t.Error("admin mapping should fail validation")
	}
}
