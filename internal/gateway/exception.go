package gateway

import (
	"net/http"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/exception"
	"github.com/myjnathan/agentgate/internal/store"
)

// Exception grants (backlog F24) are the one-time waiver for the two
// classification blocks in ChatCompletions: the BYOA tier may not
// send confidential+ data, and secret data may only go to a local
// model. A grant never waives the banned-word block (that check runs
// before any grant is looked up), never changes the policy itself,
// and is consumed exactly once — at the point the gateway commits to
// forwarding the waived call.
//
// The grant is consulted only when the policy would otherwise block:
// a grant presented on a call that needs no waiver is ignored, never
// consumed, and an invalid grant on such a call is harmless. That
// keeps a stray header from breaking clean traffic, while a grant
// that is presented where it matters and does not validate fails
// closed with an explicit error instead of a misleading policy block.

// exceptionGrantHeader carries the one-time grant token for a call.
const exceptionGrantHeader = "X-AgentGate-Exception-Grant"

// findGrant validates a presented grant token for this caller and
// data level. The second return reports whether a token was
// presented at all, so the caller can tell "no grant" apart from
// "a grant that does not cover this call".
func (g *Gateway) findGrant(r *http.Request, userID string, level store.DataLevel) (grant *store.Exception, presented bool) {
	raw := strings.TrimSpace(r.Header.Get(exceptionGrantHeader))
	if raw == "" {
		return nil, false
	}
	g.st.Read(func(st *store.State) {
		grant = exception.FindValidGrant(st.Exceptions, exception.HashGrant(raw), userID, level, time.Now())
	})
	return grant, true
}

// consumeGrant spends a validated grant: under the store lock it
// re-checks that the grant is still approved (a concurrent request
// with the same token may have spent it first) and marks it used.
// It reports whether this call spent the grant; on false the caller
// must block, because the waiver is gone. The spend is audited and
// the exception ID is attached to the call's detail map so the
// model_call event traces back to the approval.
func (g *Gateway) consumeGrant(grant *store.Exception, user *store.User, tok *store.APIToken, model string, level store.DataLevel, detail map[string]string) bool {
	now := time.Now()
	spent := false
	g.st.WithLock(func(st *store.State) {
		rec, ok := st.Exceptions[grant.ID]
		if !ok || rec.Status != exception.StatusApproved {
			return
		}
		if rec.ExpiresAt != nil && now.After(*rec.ExpiresAt) {
			return
		}
		rec.Status = exception.StatusUsed
		rec.UsedAt = &now
		spent = true
	})
	if !spent {
		return false
	}
	_ = g.st.Save()
	detail["exception_id"] = grant.ID
	g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "exception", Action: "exception_grant_used", Model: model,
		DataLevel: level.String(), Decision: "allow",
		Detail: map[string]string{"exception_id": grant.ID, "applicant": grant.Applicant}})
	return true
}

// rejectInvalidGrant answers a call that presented a grant which does
// not cover it (unknown, expired, already used, another employee's,
// or for a lower level) where the policy would otherwise have
// blocked. The audit event is a violation so a replayed or guessed
// token shows up in the security trail.
func (g *Gateway) rejectInvalidGrant(w http.ResponseWriter, user *store.User, tok *store.APIToken, model string, level store.DataLevel, detail map[string]string) {
	g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "violation", Action: "model_call_blocked", Model: model, DataLevel: level.String(), Decision: "block",
		Detail: mergeDetail(detail, "reason", "exception_grant_invalid")})
	writeErr(w, http.StatusForbidden, "exception_grant_invalid",
		"the exception grant is invalid, expired, already used, or does not cover this call; apply for a new exception")
}
