// Package exception implements the high-data-level call exception
// approval flow (backlog F24): an employee whose call was blocked by
// the classification policy applies for an exception (reason + scope +
// validity), an admin approves or rejects it in the management UI,
// and an approval issues a one-time grant token bound to that
// employee, that data level and an expiry. The gateway honours the
// grant for exactly one call, then consumes it.
//
// The flow mirrors the DLP outbound-decryption approval workflow
// used by IP-guard-class products (apply -> approve -> one-time
// release for this request only -> full audit trail): approval
// covers this request, never a standing permission, and the policy
// itself is never changed by a grant.
package exception

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/attest"
	"github.com/myjnathan/agentgate/internal/pluginpol"
	"github.com/myjnathan/agentgate/internal/store"
)

// Statuses of an exception request.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusUsed     = "used"
	// StatusExpired is assigned lazily when an approved grant is read
	// after its expiry; nothing is scheduled to flip it.
	StatusExpired = "expired"
	StatusRevoked = "revoked"
)

// Kinds of exception request: "call" is the F24 one-time grant for
// a blocked high-data-level call; "plugin" is the F38 slice 3
// time-limited waiver for one plugin triple. Records written before
// kinds existed carry "" and are treated as "call".
const (
	KindCall   = "call"
	KindPlugin = "plugin"
)

// MaxHours caps how long a grant may live: an exception is a
// short-lived waiver, not a standing permission. MinHours keeps
// sub-hour grants (which would mostly expire before use) out.
const (
	MinHours = 1
	MaxHours = 72
)

// The record itself lives in the store package (internal/store/
// exception.go) so the store can hold it without an import cycle;
// this package holds the validation, grant minting and lookup rules
// over *store.Exception.

// ValidateRequest checks an application before it is stored.
// allowTopSecret is the operator opt-in (AGENTGATE_ALLOW_TOPSECRET_EXCEPTIONS):
// top-secret calls are never exception-eligible by default.
func ValidateRequest(level store.DataLevel, reason, scope string, hours int, allowTopSecret bool) error {
	if level < store.LevelConfidential {
		return errors.New("only confidential or higher data levels need an exception")
	}
	if level >= store.LevelSecret && !allowTopSecret {
		return errors.New("top-secret exceptions are disabled; enable AGENTGATE_ALLOW_TOPSECRET_EXCEPTIONS to allow them")
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("reason required")
	}
	if len(reason) > 500 {
		return errors.New("reason too long (max 500 characters)")
	}
	if strings.TrimSpace(scope) == "" {
		return errors.New("scope required")
	}
	if len(scope) > 500 {
		return errors.New("scope too long (max 500 characters)")
	}
	if hours < MinHours || hours > MaxHours {
		return fmt.Errorf("hours must be between %d and %d", MinHours, MaxHours)
	}
	return nil
}

// ValidatePluginTriple checks the plugin identity of a plugin
// exception application or a pre-install check (backlog F38 slice
// 3): the ID is required, the version is an exact version when
// given (not a constraint — an exception waives one installed
// version, and a constraint would waive versions never reviewed),
// the hash is a 64-hex SHA-256 when given, and the source is one
// of the known install sources when given.
func ValidatePluginTriple(id, version, hash, source string) error {
	if len(strings.TrimSpace(id)) == 0 || len(id) > 128 {
		return errors.New("plugin id must be 1-128 characters")
	}
	if version != "" && !attest.ValidVersion(version) {
		return errors.New("plugin version is invalid")
	}
	if hash != "" {
		if len(hash) != 64 {
			return errors.New("plugin hash must be a 64-character SHA-256 hex digest")
		}
		for _, c := range hash {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return errors.New("plugin hash must be hex")
			}
		}
	}
	switch source {
	case "", pluginpol.SourcePlatform, pluginpol.SourceMarketplace, pluginpol.SourceLocal:
	default:
		return errors.New("plugin source must be platform, marketplace or local")
	}
	return nil
}

// ValidatePluginRequest checks a plugin exception application: the
// triple plus the same reason/scope/validity shape as a call
// exception (1–72 hours; a plugin waiver is a short-lived rule,
// not a standing permission).
func ValidatePluginRequest(id, version, hash, source, reason, scope string, hours int) error {
	if err := ValidatePluginTriple(id, version, hash, source); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("reason required")
	}
	if len(reason) > 500 {
		return errors.New("reason too long (max 500 characters)")
	}
	if strings.TrimSpace(scope) == "" {
		return errors.New("scope required")
	}
	if len(scope) > 500 {
		return errors.New("scope too long (max 500 characters)")
	}
	if hours < MinHours || hours > MaxHours {
		return fmt.Errorf("hours must be between %d and %d", MinHours, MaxHours)
	}
	return nil
}

// derefTime unwraps an optional expiry for the ExceptedPlugin
// value; a nil expiry reads as the zero time (unbounded).
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ExceptedPluginsByUser groups every approved, unexpired plugin
// exception by applicant, for views that judge many users at once
// (the asset view, slice 3). The filtering rule lives here once:
// any other listing must not re-implement it and drift.
func ExceptedPluginsByUser(exceptions map[string]*store.Exception, now time.Time) map[string][]pluginpol.ExceptedPlugin {
	out := map[string][]pluginpol.ExceptedPlugin{}
	for _, e := range exceptions {
		if e == nil || e.Kind != KindPlugin {
			continue
		}
		if e.Status != StatusApproved {
			continue
		}
		if e.ExpiresAt != nil && now.After(*e.ExpiresAt) {
			continue
		}
		out[e.ApplicantID] = append(out[e.ApplicantID], pluginpol.ExceptedPlugin{
			ID: e.PluginID, Version: e.PluginVersion,
			Hash: e.PluginHash, Source: e.PluginSource,
			ExpiresAt: derefTime(e.ExpiresAt),
		})
	}
	return out
}

// ExceptedPlugins collects one applicant's approved, unexpired
// plugin exceptions as pluginpol values, ready for evaluation.
// Expired approvals are skipped here as well as flipped lazily by
// RefreshStatus on read — an exception whose expiry passed never
// waives anything, even before its status row is rewritten.
func ExceptedPlugins(exceptions map[string]*store.Exception, applicantID string, now time.Time) []pluginpol.ExceptedPlugin {
	return ExceptedPluginsByUser(exceptions, now)[applicantID]
}

// NewGrant mints a one-time grant token and its hash. The token is
// random 24 bytes; guessing it is infeasible, so storing only the
// hash is enough for the gateway to verify a presented grant.
func NewGrant() (token, hash string) {
	var b [24]byte
	_, _ = rand.Read(b[:])
	token = "exc_" + hex.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:])
}

// HashGrant returns the stored hash for a presented grant token.
func HashGrant(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// RefreshStatus flips an approved-but-past-expiry grant to expired.
// It reports whether the status changed, so callers know to persist.
func RefreshStatus(e *store.Exception, now time.Time) bool {
	if e.Status == StatusApproved && e.ExpiresAt != nil && now.After(*e.ExpiresAt) {
		e.Status = StatusExpired
		return true
	}
	return false
}

// FindValidGrant looks up the approved, unexpired, unused grant
// matching a presented token hash. The grant must belong to the
// caller (by user ID) and cover the call's data level; a grant for
// confidential data never waives a secret-level call.
func FindValidGrant(exceptions map[string]*store.Exception, grantHash, applicantID string, level store.DataLevel, now time.Time) *store.Exception {
	for _, e := range exceptions {
		if e.GrantHash == "" || e.GrantHash != grantHash {
			continue
		}
		if e.Status != StatusApproved {
			continue
		}
		if e.ApplicantID != applicantID {
			continue
		}
		if e.ExpiresAt != nil && now.After(*e.ExpiresAt) {
			continue
		}
		if level > e.DataLevel {
			continue
		}
		return e
	}
	return nil
}
