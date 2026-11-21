package gateway

import (
	"fmt"

	"github.com/myjnathan/agentgate/internal/store"
)

// Channel trust types (F17). Routing binds to where the data goes and
// what the deployment promises — not to whether the enterprise owns a
// model (most companies have none). The gradient:
//
//	public/internal -> any trust type (signed cloud by default)
//	confidential    -> dedicated cloud / local model as-is; signed cloud
//	                   only after gateway desensitization (the original
//	                   text never leaves the company, the cloud only
//	                   sees placeholders, the response is restored)
//	secret          -> local model only; otherwise the request is blocked
//	                   with an explicit "no compliant channel" reason.
//
// Conservative default: when no channel is compliant for the level, the
// request is blocked and audited — it never silently falls back to a
// less-trusted channel. Rollout follows the existing three phases:
// observe (DryRun: audit only) -> warn (audit decision "warn") -> block.

// trustAllowed reports whether a channel of the given trust type may
// receive data at the given level (before any redaction is considered).
// A signed cloud is "allowed" for confidential data only because the
// gateway desensitizes the request first; see trustRequiresRedaction.
func trustAllowed(level store.DataLevel, trust store.TrustType) bool {
	if level < store.LevelSecret {
		return true
	}
	// Secret: only a local model keeps the data inside the company.
	return trust == store.TrustLocalModel
}

// trustRequiresRedaction reports whether data at the given level must be
// desensitized by the gateway before it may be sent to a channel of the
// given trust type. This is a property of the trust type, not of the
// caller's tier setting: even if tier redaction is switched off, a
// confidential request on a signed cloud is always desensitized.
func trustRequiresRedaction(level store.DataLevel, trust store.TrustType) bool {
	return level >= store.LevelConfidential && trust == store.TrustSignedCloud
}

// noCompliantChannelMessage explains a block in user terms: which level
// was requested and what kind of channel would be compliant.
func noCompliantChannelMessage(level store.DataLevel) string {
	if level >= store.LevelSecret {
		return "no compliant channel for secret data: top-secret data may only go to a local model; " +
			"configure a local-model channel or remove the sensitive content"
	}
	return fmt.Sprintf("no compliant channel for %s data: no configured channel may receive it under the trust policy", level.String())
}
