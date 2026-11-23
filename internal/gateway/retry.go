// Failure-classified retries (backlog F33).
//
// The gateway does not retry blindly: each failed upstream attempt is
// classified first, and the class decides what happens next —
// borrowing Bifrost's failure-classified retries
// (docs.getbifrost.ai/features/retries-and-fallbacks):
//
//   - auth (401/403): the key is the problem. Rotate to the next key
//     at once, never retry the same key; a 401 also disables the key
//     (F12). Waiting changes nothing for a dead credential, so no
//     backoff is applied (Bifrost's "permanent per-key failure").
//   - transient (network error, 429, 5xx): the upstream is the
//     problem. Retry the same key with exponential backoff and jitter
//     (Bifrost's "transient server failure"). The budget bounds
//     retries against a key that might recover, not the walk to the
//     next key or channel — each fallback channel gets its own full
//     budget.
//   - client (other 4xx): the request is the problem. No key or
//     channel would have served it, so the response is returned
//     as-is, exactly like before F33.
//   - success (2xx/3xx): served.
//
// Differences from Bifrost, per the approved F33 spec: Bifrost
// rotates keys on 429 (providers often share account-level quotas
// across keys); we retry 429 on the same key with backoff, because
// our channels are per-deployment with their own keys and rotating
// would burn the pool for a quota the next key shares. Bifrost also
// returns 502 upstream_credentials_exhausted when every key is
// refused for an account-level reason; we return the last upstream
// auth response itself (the F12 convention — the caller sees the real
// auth failure, e.g. a 401 when every key is dead, a 403 when no key
// may reach the model).
//
// Red lines: classification never changes the F1/F17 fallback rules
// — retries and failover stay inside the first pick's MaxDataLevel
// and trust-compliant set, never escalating. Every attempt's
// metadata trail (channel, key fingerprint, failure class, duration,
// result) is written to the audit log; the trail never carries the
// request or response body, and the key fingerprint is the masked
// display form only.

package gateway

import (
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// failureClass is one outcome of classifying a failed upstream
// attempt. The zero value is never used: classifyStatus always
// returns one of the four classes below.
type failureClass string

const (
	// classAuth: 401/403 — the key (or its scope) is the problem.
	classAuth failureClass = "auth"
	// classTransient: network error, 429, 5xx — the upstream is the
	// problem; the same key is worth retrying with backoff.
	classTransient failureClass = "transient"
	// classClient: other 4xx — the request is the problem; no retry.
	classClient failureClass = "client"
	// classSuccess: 2xx/3xx — the attempt served the call.
	classSuccess failureClass = "success"
)

// classifyStatus maps an upstream HTTP status to its failure class.
func classifyStatus(code int) failureClass {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return classAuth
	case code == http.StatusTooManyRequests || code >= 500:
		return classTransient
	case code >= 400:
		return classClient
	default:
		return classSuccess
	}
}

// maxTransientRetries returns the configured same-key retry budget
// for transient failures: retries after the first attempt, per key
// (0 = default 2, so 3 attempts per key before the channel is
// abandoned for the next same-level channel).
func (g *Gateway) maxTransientRetries() int {
	if g.policy.MaxTransientRetries > 0 {
		return g.policy.MaxTransientRetries
	}
	return 2
}

// retryBackoff returns the wait before transient retry number n
// (0-based): base * 2^n, capped at 2s, plus up to 50% jitter so
// synchronized clients do not retry in lockstep (Bifrost's
// exponential backoff with jitter).
func (g *Gateway) retryBackoff(n int) time.Duration {
	base := g.policy.RetryBackoff
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	d := base << uint(n)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d + time.Duration(rand.Int63n(int64(d)/2+1))
}

// keyMasked returns the display (masked) form of key idx of ch, for
// the attempt trail — never the key itself. The store is read fresh
// so a key added or rotated mid-call reports its current mask.
func (g *Gateway) keyMasked(ch *store.Channel, idx int) string {
	var masked string
	g.st.Read(func(st *store.State) {
		if c, ok := st.Channels[ch.ID]; ok && idx >= 0 && idx < len(c.Keys) {
			masked = c.Keys[idx].Masked
		}
	})
	return masked
}

// auditAttempt records one upstream try in the audit trail (F33):
// channel, key fingerprint (masked), failure class, status, duration
// and result — metadata only, never the request or response body.
// n is the 1-based attempt number within the call. The event is Kind
// "channel" like the F1 fallback/cooldown events, so stats (which
// only count model_call/violation) and alerts (which only fire on
// named Kind+Action pairs) are unaffected.
func (g *Gateway) auditAttempt(user *store.User, tok *store.APIToken, model string, level store.DataLevel, n int, ch *store.Channel, keyMasked string, class failureClass, status string, dur time.Duration) {
	result := "failed"
	decision := "failed"
	if class == classSuccess || class == classClient {
		// The upstream answered and its response is returned to the
		// caller (a 4xx is the caller's fault, not the channel's).
		result = "served"
		decision = "served"
	}
	g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "channel", Action: "forward_attempt", Model: model, Channel: ch.Name,
		DataLevel: level.String(), Decision: decision,
		Detail: map[string]string{
			"channel_id":  ch.ID,
			"key_masked":  keyMasked,
			"attempt":     strconv.Itoa(n),
			"class":       string(class),
			"status":      status,
			"duration_ms": strconv.FormatInt(dur.Milliseconds(), 10),
			"result":      result,
		}})
}
