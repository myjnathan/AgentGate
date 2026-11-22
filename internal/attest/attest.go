// Package attest implements agent attestation (backlog F14): a managed
// agent reports evidence about itself when it logs in, the platform
// verifies that evidence against an admin-configured appraisal policy,
// and only a passing agent receives a managed-tier token. Anything
// else is downgraded to BYOA and alerted.
//
// The role model follows IETF RATS (RFC 9334): the agent is the
// Attester producing Evidence (version / config hash / binary hash /
// plugin list / guard state); this package is the Verifier, appraising
// Evidence against Reference Values (allowed config hashes, minimum
// version, plugin allow/deny lists) under an Appraisal Policy; the
// Attestation Result (pass / downgraded / unverified) is consumed by
// the token issuer, which is the Relying Party. Freshness comes from
// the device-code login itself: a report is bound to one device code
// (code + polling secret, single use, 10-minute expiry), which plays
// the role of the RATS nonce — a report cannot be replayed against a
// later login.
//
// Honest limit, stated in docs/attestation.md: this round is
// *self-reported* software evidence. It stops misconfiguration and
// gives the platform an auditable downgrade decision; it does not
// stop a deliberately forged report. Hardware-rooted evidence (TPM
// quote, SPIRE-style platform attestation) is future work.
//
// No code is copied from another project here: RATS is an
// architecture document with no reference implementation, and the
// existing verifiers (SPIRE, Veraison) are whole attestation systems
// far larger than this feature. The evaluation below is ~150 lines of
// standard-library Go, written for this codebase (reuse-first check
// recorded in DEVLOG, 2026-11-22).
package attest

import (
	"fmt"
	"strconv"
	"strings"
)

// Verdict statuses. "pass" issues the requested tier; "downgraded"
// issues a BYOA token instead; "unverified" means no appraisal policy
// applied (or the policy does not require a report and none was
// given) — the requested tier is issued but the token is marked
// unverified.
const (
	VerdictPass       = "pass"
	VerdictDowngraded = "downgraded"
	VerdictUnverified = "unverified"
)

// Report is the attestation evidence an agent submits with its
// device-code login. Hashes are hex digests of the agent's own
// gateway configuration (never of a secret) and of its binary; the
// plugin list names installed plugins. None of these fields is
// sensitive, but the audit trail records only verdicts and reason
// codes, never the hash values.
type Report struct {
	AgentType    string   `json:"agent_type"`
	Version      string   `json:"version"`
	ConfigHash   string   `json:"config_hash,omitempty"`
	BinaryHash   string   `json:"binary_hash,omitempty"`
	Plugins      []string `json:"plugins,omitempty"`
	GuardApplied bool     `json:"guard_applied"`
	OS           string   `json:"os,omitempty"`
}

// Policy is the appraisal policy plus reference values for one agent
// type, configured by an admin. Empty allow-lists mean "any value
// accepted"; a non-empty list is fail-closed (an absent or
// unlisted value fails).
type Policy struct {
	AgentType           string   `json:"agent_type"`
	MinVersion          string   `json:"min_version,omitempty"`
	AllowedConfigHashes []string `json:"allowed_config_hashes,omitempty"`
	AllowedPlugins      []string `json:"allowed_plugins,omitempty"`
	BlockedPlugins      []string `json:"blocked_plugins,omitempty"`
	RequireGuard        bool     `json:"require_guard"`
	RequireReport       bool     `json:"require_report"`
}

// Verdict is the attestation result. Reasons are stable category
// codes (version_below_minimum, config_hash_not_allowed, ...), never
// the offending values, so they are safe to audit and alert on.
type Verdict struct {
	Status  string   `json:"status"` // pass | downgraded | unverified
	Reasons []string `json:"reasons,omitempty"`
}

// Reason codes.
const (
	ReasonReportMissing       = "report_missing"
	ReasonVersionBelowMinimum = "version_below_minimum"
	ReasonConfigHashMissing   = "config_hash_missing"
	ReasonConfigHashNotAllow  = "config_hash_not_allowed"
	ReasonBlockedPlugin       = "blocked_plugin"
	ReasonPluginNotAllowed    = "plugin_not_allowed"
	ReasonGuardNotApplied     = "guard_not_applied"
)

// Validate checks a report's shape: bounded sizes and hex hashes, so
// a hostile client cannot stuff the audit log or the snapshot.
func (r Report) Validate() error {
	// AgentType is client-supplied and overwritten server-side from
	// the device code; it is only length-bounded here, never trusted.
	if len(r.AgentType) > 64 {
		return fmt.Errorf("agent_type too long")
	}
	if len(r.Version) > 64 {
		return fmt.Errorf("version too long")
	}
	if len(r.ConfigHash) > 128 || len(r.BinaryHash) > 128 {
		return fmt.Errorf("hash too long")
	}
	for _, h := range []string{r.ConfigHash, r.BinaryHash} {
		if h == "" {
			continue
		}
		for _, c := range h {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return fmt.Errorf("hash must be hex")
			}
		}
	}
	if len(r.Plugins) > 100 {
		return fmt.Errorf("too many plugins (max 100)")
	}
	for _, p := range r.Plugins {
		if len(p) == 0 || len(p) > 128 {
			return fmt.Errorf("plugin name must be 1-128 characters")
		}
	}
	if len(r.OS) > 64 {
		return fmt.Errorf("os too long")
	}
	return nil
}

// Validate checks a policy's shape.
func (p Policy) Validate() error {
	if len(p.AgentType) == 0 || len(p.AgentType) > 64 {
		return fmt.Errorf("agent_type must be 1-64 characters")
	}
	if p.MinVersion != "" && !ValidVersion(p.MinVersion) {
		return fmt.Errorf("min_version %q is not a version", p.MinVersion)
	}
	for _, h := range p.AllowedConfigHashes {
		if h == "" || len(h) > 128 {
			return fmt.Errorf("allowed config hash must be 1-128 characters")
		}
	}
	for _, name := range append(append([]string{}, p.AllowedPlugins...), p.BlockedPlugins...) {
		if name == "" || len(name) > 128 {
			return fmt.Errorf("plugin name must be 1-128 characters")
		}
	}
	return nil
}

// Evaluate appraises a report against a policy. A nil report means
// the agent submitted no evidence for this login.
func Evaluate(report *Report, policy Policy) Verdict {
	if report == nil {
		if policy.RequireReport {
			return Verdict{Status: VerdictDowngraded, Reasons: []string{ReasonReportMissing}}
		}
		return Verdict{Status: VerdictUnverified}
	}
	var reasons []string
	if policy.MinVersion != "" && CompareVersion(report.Version, policy.MinVersion) < 0 {
		reasons = append(reasons, ReasonVersionBelowMinimum)
	}
	if len(policy.AllowedConfigHashes) > 0 {
		if report.ConfigHash == "" {
			reasons = append(reasons, ReasonConfigHashMissing)
		} else if !containsFold(policy.AllowedConfigHashes, report.ConfigHash) {
			reasons = append(reasons, ReasonConfigHashNotAllow)
		}
	}
	for _, p := range report.Plugins {
		if containsFold(policy.BlockedPlugins, p) {
			reasons = append(reasons, ReasonBlockedPlugin)
			break
		}
	}
	if len(policy.AllowedPlugins) > 0 {
		for _, p := range report.Plugins {
			if !containsFold(policy.AllowedPlugins, p) {
				reasons = append(reasons, ReasonPluginNotAllowed)
				break
			}
		}
	}
	if policy.RequireGuard && !report.GuardApplied {
		reasons = append(reasons, ReasonGuardNotApplied)
	}
	if len(reasons) > 0 {
		return Verdict{Status: VerdictDowngraded, Reasons: reasons}
	}
	return Verdict{Status: VerdictPass}
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

// ValidVersion reports whether s looks like a dotted numeric version
// ("1.2.3", optionally with a leading "v" and a "-prerelease" suffix).
func ValidVersion(s string) bool {
	_, _, ok := parseVersion(s)
	return ok
}

// CompareVersion compares dotted numeric versions: negative if a < b,
// zero if equal, positive if a > b. A leading "v" is ignored, missing
// segments count as 0, and a "-prerelease" suffix sorts below the same
// release version. Non-numeric segments compare as 0 (an unparseable
// version is treated as 0.0.0 — fail-closed against any minimum).
func CompareVersion(a, b string) int {
	an, aPre, _ := parseVersion(a)
	bn, bPre, _ := parseVersion(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		var av, bv int
		if i < len(an) {
			av = an[i]
		}
		if i < len(bn) {
			bv = bn[i]
		}
		if av != bv {
			return av - bv
		}
	}
	// Same numbers: a prerelease is lower than the release.
	if aPre && !bPre {
		return -1
	}
	if !aPre && bPre {
		return 1
	}
	return 0
}

// parseVersion splits "v1.2.3-beta.1" into [1 2 3] and prerelease=true.
// It returns ok=false for an empty string or a string with no leading
// number at all.
func parseVersion(s string) (nums []int, prerelease bool, ok bool) {
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if s == "" {
		return nil, false, false
	}
	if i := strings.Index(s, "-"); i >= 0 {
		prerelease = true
		s = s[:i]
	}
	if i := strings.Index(s, "+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	nums = make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, false, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false, false
		}
		nums = append(nums, n)
	}
	return nums, prerelease, true
}
