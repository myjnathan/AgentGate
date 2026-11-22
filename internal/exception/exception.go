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
