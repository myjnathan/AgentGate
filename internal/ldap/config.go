// Package ldap holds the LDAP directory configuration for AgentGate
// (backlog F9). This round ships the configuration shape and
// validation only: there is no LDAP test server in this environment,
// so the bind/search client is NOT implemented yet and no live LDAP
// login has been verified. See docs/ldap.md for the design, the
// environment variables and what remains before LDAP can be enabled.
package ldap

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Config is the LDAP directory configuration, taken from the
// environment (see ConfigFromEnv). The client secret (bind password)
// is never logged and never returned by any API.
type Config struct {
	// URL of the directory, e.g. ldaps://ad.example.corp:636.
	URL string
	// BindDN is the service account used for the initial bind and for
	// user searches, e.g. cn=agentgate,ou=services,dc=example,dc=corp.
	BindDN string
	// BindPassword is the service account password.
	BindPassword string
	// BaseDN is the search base, e.g. ou=users,dc=example,dc=corp.
	BaseDN string
	// UserFilter is the search filter with %s for the login name,
	// e.g. (sAMAccountName=%s) for AD or (uid=%s) for OpenLDAP.
	UserFilter string
	// DepartmentAttr is the entry attribute mapped to the AgentGate
	// department (default "department"). Empty means no mapping.
	DepartmentAttr string
	// StartTLS upgrades a plain ldap:// connection (ldaps:// URLs
	// must not set it).
	StartTLS bool
}

// ConfigFromEnv reads the LDAP configuration from the environment:
//
//	AGENTGATE_LDAP_URL            e.g. ldaps://ad.example.corp:636
//	AGENTGATE_LDAP_BIND_DN        service account DN
//	AGENTGATE_LDAP_BIND_PASSWORD  service account password
//	AGENTGATE_LDAP_BASE_DN        search base DN
//	AGENTGATE_LDAP_USER_FILTER    default (uid=%s)
//	AGENTGATE_LDAP_DEPARTMENT_ATTR default "department"
//	AGENTGATE_LDAP_STARTTLS      "1" to upgrade ldap:// with StartTLS
//
// The second return value reports whether LDAP is configured at all
// (URL + bind DN + password + base DN all present).
func ConfigFromEnv(getenv func(string) string) (Config, bool) {
	cfg := Config{
		URL:            strings.TrimSpace(getenv("AGENTGATE_LDAP_URL")),
		BindDN:         strings.TrimSpace(getenv("AGENTGATE_LDAP_BIND_DN")),
		BindPassword:   getenv("AGENTGATE_LDAP_BIND_PASSWORD"),
		BaseDN:         strings.TrimSpace(getenv("AGENTGATE_LDAP_BASE_DN")),
		UserFilter:     strings.TrimSpace(getenv("AGENTGATE_LDAP_USER_FILTER")),
		DepartmentAttr: strings.TrimSpace(getenv("AGENTGATE_LDAP_DEPARTMENT_ATTR")),
		StartTLS:       getenv("AGENTGATE_LDAP_STARTTLS") == "1",
	}
	if cfg.URL == "" || cfg.BindDN == "" || cfg.BindPassword == "" || cfg.BaseDN == "" {
		return cfg, false
	}
	if cfg.UserFilter == "" {
		cfg.UserFilter = "(uid=%s)"
	}
	if cfg.DepartmentAttr == "" {
		cfg.DepartmentAttr = "department"
	}
	return cfg, true
}

// Validate checks the configuration shape (not directory
// reachability). LDAP is not implemented in this round; Validate is
// the gate the future client must pass before any bind is attempted.
func (c Config) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil {
		return fmt.Errorf("ldap url: %w", err)
	}
	switch u.Scheme {
	case "ldaps":
		if c.StartTLS {
			return errors.New("ldap StartTLS must not be set for ldaps:// URLs")
		}
	case "ldap":
	default:
		return errors.New("ldap url must start with ldap:// or ldaps://")
	}
	if u.Host == "" {
		return errors.New("ldap url must include a host")
	}
	if c.BindDN == "" {
		return errors.New("ldap bind_dn is required")
	}
	if c.BindPassword == "" {
		return errors.New("ldap bind_password is required")
	}
	if c.BaseDN == "" {
		return errors.New("ldap base_dn is required")
	}
	if !strings.Contains(c.UserFilter, "%s") {
		return errors.New("ldap user_filter must contain %s for the login name")
	}
	if !strings.HasPrefix(c.UserFilter, "(") || !strings.HasSuffix(c.UserFilter, ")") {
		return errors.New("ldap user_filter must be a parenthesised filter")
	}
	return nil
}

// Implemented reports whether an LDAP client exists in this build.
// It is false until the bind/search client lands (backlog F9 follow-up).
func Implemented() bool { return false }
