package ldap

import "testing"

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if _, ok := ConfigFromEnv(get); ok {
		t.Fatal("empty env must not enable LDAP")
	}
	env["AGENTGATE_LDAP_URL"] = "ldaps://ad.example.corp:636"
	env["AGENTGATE_LDAP_BIND_DN"] = "cn=agentgate,ou=services,dc=example,dc=corp"
	env["AGENTGATE_LDAP_BIND_PASSWORD"] = "secret"
	env["AGENTGATE_LDAP_BASE_DN"] = "ou=users,dc=example,dc=corp"
	cfg, ok := ConfigFromEnv(get)
	if !ok {
		t.Fatal("full env must enable LDAP")
	}
	if cfg.UserFilter != "(uid=%s)" || cfg.DepartmentAttr != "department" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if Implemented() {
		t.Fatal("LDAP client must not claim to be implemented in this round")
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	base := Config{URL: "ldaps://ad.example.corp:636", BindDN: "cn=x", BindPassword: "pw",
		BaseDN: "ou=users,dc=example,dc=corp", UserFilter: "(uid=%s)", DepartmentAttr: "department"}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"plain http url", func(c *Config) { c.URL = "http://ad.example.corp" }},
		{"no host", func(c *Config) { c.URL = "ldaps://" }},
		{"starttls with ldaps", func(c *Config) { c.StartTLS = true }},
		{"filter without placeholder", func(c *Config) { c.UserFilter = "(uid=alice)" }},
		{"missing base dn", func(c *Config) { c.BaseDN = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected rejection: %s", tc.name)
			}
		})
	}
	// ldap:// with StartTLS is the valid plain-text upgrade path.
	cfg := base
	cfg.URL = "ldap://ad.example.corp:389"
	cfg.StartTLS = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ldap:// + StartTLS must validate: %v", err)
	}
}
