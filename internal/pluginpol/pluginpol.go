// Package pluginpol implements plugin allow/deny-list policy
// (backlog F38, slice 1): the PluginPolicy object (mode, scope,
// triple rules, disposition), scope resolution that can only
// tighten, and the evaluation shared by login attestation and
// heartbeat drift — so the verdict is identical in both places.
//
// A plugin is identified by a triple (ID, version, content
// SHA-256) plus install source. A rule may constrain only the ID
// (all versions), ID + version constraint, or the full triple.
// Old agents report names only; those match by ID and are never
// failed for a missing version or hash (no evidence either way),
// but they are flagged as unverified in the asset view (slice 2).
//
// Scope merges global → department → group with only-tighten
// semantics: an allowlist is the intersection of every applicable
// allowlist (a lower scope can only narrow it), a denylist is the
// union of every applicable denylist (a lower scope can only add
// blocks), and a deny rule always wins over an allow rule — so no
// lower scope can ever re-allow what an upper scope forbids.
// When modes mix, the effective mode is allowlist if any
// applicable policy is an allowlist.
//
// No code is copied from another project here: the evaluation is
// ~250 lines of standard-library Go written for this codebase.
// The reuse-first check (Nathan 2026-11-21): plugin allow/deny
// lists in other gateways are either bound to a specific agent
// runtime or part of a whole EDR; none fits a gateway-side policy
// object, so this is self-written (recorded in DEVLOG).
package pluginpol

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/attest"
)

// Modes.
const (
	ModeAllowlist = "allowlist"
	ModeDenylist  = "denylist"
)

// Scopes.
const (
	ScopeGlobal     = "global"
	ScopeDepartment = "department"
	ScopeGroup      = "group"
)

// Dispositions, weakest to strongest. observe/warn record only
// (warn also alerts); downgrade drops the terminal's authorization
// tier to BYOA live, and block_sensitive adds the knowledge cut on
// top — see DowngradesTier / BlocksKnowledge (slice 2).
const (
	ActionObserve        = "observe"
	ActionWarn           = "warn"
	ActionDowngrade      = "downgrade"
	ActionBlockSensitive = "block_sensitive"
)

// Install sources a report may carry. "platform" means the plugin
// came from AgentGate's own distribution (backlog F20); anything
// else is outside the platform's chain of custody.
const (
	SourcePlatform    = "platform"
	SourceMarketplace = "marketplace"
	SourceLocal       = "local"
)

// MaxRulesPerPolicy bounds one policy's rule list.
const MaxRulesPerPolicy = 200

// PluginRule is one allow/deny rule: a plugin triple (ID required,
// version constraint and hash optional) plus a source constraint.
// VersionConstraint is "" (any version), an exact version
// ("1.2.3"), or an operator prefix (">=1.2.3", "<=2.0.0", ">",
// "<", "!="). ValidFrom/ValidUntil bound the rule in time; zero
// means unbounded.
type PluginRule struct {
	ID                string    `json:"id"`
	VersionConstraint string    `json:"version_constraint,omitempty"`
	Hash              string    `json:"hash,omitempty"`
	Source            string    `json:"source,omitempty"`
	Note              string    `json:"note,omitempty"`
	ValidFrom         time.Time `json:"valid_from,omitempty"`
	ValidUntil        time.Time `json:"valid_until,omitempty"`
}

// Validate checks a rule's shape.
func (r PluginRule) Validate() error {
	if len(r.ID) == 0 || len(r.ID) > 128 {
		return fmt.Errorf("rule id must be 1-128 characters")
	}
	if _, _, ok := parseConstraint(r.VersionConstraint); !ok {
		return fmt.Errorf("version constraint %q is invalid", r.VersionConstraint)
	}
	if r.Hash != "" {
		if len(r.Hash) != 64 {
			return fmt.Errorf("rule hash must be a 64-character SHA-256 hex digest")
		}
		for _, c := range r.Hash {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return fmt.Errorf("rule hash must be hex")
			}
		}
	}
	switch r.Source {
	case "", SourcePlatform, SourceMarketplace, SourceLocal:
	default:
		return fmt.Errorf("rule source must be platform, marketplace or local")
	}
	if len(r.Note) > 256 {
		return fmt.Errorf("rule note too long")
	}
	if !r.ValidFrom.IsZero() && !r.ValidUntil.IsZero() && !r.ValidUntil.After(r.ValidFrom) {
		return fmt.Errorf("valid_until must be after valid_from")
	}
	return nil
}

// InWindow reports whether the rule is in force at t.
func (r PluginRule) InWindow(t time.Time) bool {
	if !r.ValidFrom.IsZero() && t.Before(r.ValidFrom) {
		return false
	}
	if !r.ValidUntil.IsZero() && !t.Before(r.ValidUntil) {
		return false
	}
	return true
}

// Scope is where a policy applies: global (everyone), one
// department, or one user group. Groups are resolved from the
// caller's group list; the platform has no group membership model
// yet (slice 1), so a group-scoped policy matches nothing until
// one lands — the scope is defined now so policies can be written
// ahead of that.
type Scope struct {
	Level      string `json:"level"` // global | department | group
	Department string `json:"department,omitempty"`
	Group      string `json:"group,omitempty"`
}

// Validate checks a scope's shape.
func (s Scope) Validate() error {
	switch s.Level {
	case ScopeGlobal:
		if s.Department != "" || s.Group != "" {
			return fmt.Errorf("global scope takes no department or group")
		}
	case ScopeDepartment:
		if s.Department == "" || len(s.Department) > 128 {
			return fmt.Errorf("department scope needs a department")
		}
		if s.Group != "" {
			return fmt.Errorf("department scope takes no group")
		}
	case ScopeGroup:
		if s.Group == "" || len(s.Group) > 128 {
			return fmt.Errorf("group scope needs a group")
		}
		if s.Department != "" {
			return fmt.Errorf("group scope takes no department")
		}
	default:
		return fmt.Errorf("scope level must be global, department or group")
	}
	return nil
}

// Applies reports whether the scope covers a user in the given
// department and groups.
func (s Scope) Applies(department string, groups []string) bool {
	switch s.Level {
	case ScopeGlobal:
		return true
	case ScopeDepartment:
		return department != "" && strings.EqualFold(s.Department, department)
	case ScopeGroup:
		for _, g := range groups {
			if strings.EqualFold(g, s.Group) {
				return true
			}
		}
		return false
	}
	return false
}

// PluginPolicy is the standalone policy object (F38): mode, scope,
// rules, disposition, and the unknown-plugin alarm a denylist must
// carry (otherwise a renamed plugin slips past silently).
type PluginPolicy struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Mode        string       `json:"mode"`  // allowlist | denylist
	Scope       Scope        `json:"scope"` // where it applies
	Rules       []PluginRule `json:"rules,omitempty"`
	Action      string       `json:"action"` // observe | warn | downgrade | block_sensitive
	WarnUnknown bool         `json:"warn_unknown"`
	Enabled     bool         `json:"enabled"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// Validate checks a policy's shape. A denylist without WarnUnknown
// is rejected: denylist mode is only meaningful when unknown
// plugins are flagged, otherwise renaming a plugin defeats it.
func (p PluginPolicy) Validate() error {
	if len(p.ID) == 0 || len(p.ID) > 64 {
		return fmt.Errorf("policy id must be 1-64 characters")
	}
	if len(p.Name) == 0 || len(p.Name) > 128 {
		return fmt.Errorf("policy name must be 1-128 characters")
	}
	switch p.Mode {
	case ModeAllowlist, ModeDenylist:
	default:
		return fmt.Errorf("mode must be allowlist or denylist")
	}
	if err := p.Scope.Validate(); err != nil {
		return err
	}
	switch p.Action {
	case ActionObserve, ActionWarn, ActionDowngrade, ActionBlockSensitive:
	default:
		return fmt.Errorf("action must be observe, warn, downgrade or block_sensitive")
	}
	if p.Mode == ModeDenylist && !p.WarnUnknown {
		return fmt.Errorf("denylist mode requires warn_unknown (otherwise renames slip past)")
	}
	if len(p.Rules) > MaxRulesPerPolicy {
		return fmt.Errorf("too many rules (max %d)", MaxRulesPerPolicy)
	}
	for _, r := range p.Rules {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Effective is the merged appraisal for one user: the applicable
// policies reduced so lower scopes can only tighten.
type Effective struct {
	// Mode is "" when no policy applies (no appraisal).
	Mode string
	// AllowSets holds one rule set per applicable allowlist policy;
	// a plugin must satisfy every set (intersection).
	AllowSets [][]PluginRule
	// DenyRules is the union of every applicable denylist's rules;
	// any matching rule blocks.
	DenyRules []PluginRule
	// Action is the strictest disposition among applicable policies.
	Action string
	// WarnUnknown is set when any applicable policy wants unknown
	// plugins flagged.
	WarnUnknown bool
}

// actionRank orders dispositions weakest to strongest.
func actionRank(a string) int {
	switch a {
	case ActionObserve:
		return 0
	case ActionWarn:
		return 1
	case ActionDowngrade:
		return 2
	case ActionBlockSensitive:
		return 3
	}
	return -1
}

// Resolve merges the policies that apply to one user (department +
// groups) at time now into an Effective policy. Policies are
// considered in ID order so the merge is deterministic.
func Resolve(policies map[string]*PluginPolicy, department string, groups []string, now time.Time) *Effective {
	ids := make([]string, 0, len(policies))
	for id := range policies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	eff := &Effective{}
	for _, id := range ids {
		p := policies[id]
		if p == nil || !p.Enabled {
			continue
		}
		if !p.Scope.Applies(department, groups) {
			continue
		}
		// Rules outside their time window are dropped; an
		// allowlist whose rules all dropped out still applies as
		// an empty set (deny-all, fail-closed).
		rules := make([]PluginRule, 0, len(p.Rules))
		for _, r := range p.Rules {
			if r.InWindow(now) {
				rules = append(rules, r)
			}
		}
		switch p.Mode {
		case ModeAllowlist:
			eff.Mode = ModeAllowlist
			eff.AllowSets = append(eff.AllowSets, rules)
		case ModeDenylist:
			eff.DenyRules = append(eff.DenyRules, rules...)
		}
		if actionRank(p.Action) > actionRank(eff.Action) {
			eff.Action = p.Action
		}
		if p.WarnUnknown {
			eff.WarnUnknown = true
		}
	}
	if eff.Mode == "" && len(eff.DenyRules) == 0 {
		return nil
	}
	if eff.Mode == "" {
		eff.Mode = ModeDenylist
	}
	if eff.Action == "" {
		eff.Action = ActionDowngrade
	}
	return eff
}

// matchKind is the outcome of matching one rule against one
// reported plugin.
const (
	matchYes     = "match"   // rule fully satisfied
	matchNo      = "nomatch" // rule does not cover this plugin
	matchPartial = "partial" // ID matches but the rule constrains
	// something the report carries no evidence for, so the rule
	// can neither allow nor block on this evidence.
)

// matchRule evaluates one rule against one reported plugin and, on
// a mismatch of a constrained field, the specific reason code.
func matchRule(rule PluginRule, ev attest.PluginEvidence) (string, string) {
	if !strings.EqualFold(rule.ID, ev.ID) {
		return matchNo, ""
	}
	if rule.VersionConstraint != "" {
		if ev.Version == "" {
			return matchPartial, ""
		}
		if !matchesConstraint(ev.Version, rule.VersionConstraint) {
			return matchNo, attest.ReasonPluginVersionMismatch
		}
	}
	if rule.Hash != "" {
		if ev.Hash == "" {
			return matchPartial, ""
		}
		if !strings.EqualFold(rule.Hash, ev.Hash) {
			return matchNo, attest.ReasonPluginHashMismatch
		}
	}
	if rule.Source != "" {
		if ev.Source == "" {
			return matchPartial, ""
		}
		if !strings.EqualFold(rule.Source, ev.Source) {
			return matchNo, attest.ReasonPluginUnknownSource
		}
	}
	return matchYes, ""
}

// Evaluate appraises reported plugins against the resolved policy.
// reported is the plugin evidence (details preferred; name-only
// entries are PluginEvidence{ID: name}). It returns the verdict
// reason codes (deduped, in report order) and, in denylist mode
// with WarnUnknown, the IDs of plugins no rule covers at all —
// allowed by default, but flagged for the unknown-plugin alert.
// A nil effective policy means no appraisal: no reasons, no
// unknowns.
func Evaluate(reported []attest.PluginEvidence, eff *Effective) (reasons []string, unknown []string) {
	if eff == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	addReason := func(code string) {
		if code != "" && !seen[code] {
			seen[code] = true
			reasons = append(reasons, code)
		}
	}
	// Dedupe the report by ID (first wins); a hostile client that
	// lists the same plugin twice must not double-count.
	seenID := map[string]bool{}
	plugins := make([]attest.PluginEvidence, 0, len(reported))
	for _, ev := range reported {
		key := strings.ToLower(strings.TrimSpace(ev.ID))
		if key == "" || seenID[key] {
			continue
		}
		seenID[key] = true
		plugins = append(plugins, ev)
	}
	for _, ev := range plugins {
		// A deny rule always wins: any full match blocks.
		blocked := false
		for _, rule := range eff.DenyRules {
			if kind, _ := matchRule(rule, ev); kind == matchYes {
				blocked = true
				break
			}
		}
		if blocked {
			addReason(attest.ReasonBlockedPlugin)
			continue
		}
		if eff.Mode == ModeDenylist {
			// Default-allow: only flag plugins no rule covers.
			covered := false
			for _, rule := range eff.DenyRules {
				if kind, _ := matchRule(rule, ev); kind != matchNo {
					covered = true
					break
				}
			}
			if !covered && eff.WarnUnknown {
				unknown = append(unknown, ev.ID)
			}
			continue
		}
		// Allowlist: the plugin must satisfy every allow set.
		allowed := true
		var failReason string
		for _, set := range eff.AllowSets {
			setOK := false
			var setFail string
			for _, rule := range set {
				kind, reason := matchRule(rule, ev)
				if kind == matchYes || kind == matchPartial {
					// Partial counts as allowed: name-only
					// evidence cannot prove a version/hash
					// failure, and the design says it is not a
					// violation (it is flagged unverified in the
					// asset view, slice 2).
					setOK = true
					break
				}
				if reason != "" && setFail == "" {
					setFail = reason
				}
			}
			if !setOK {
				allowed = false
				failReason = setFail
				break
			}
		}
		if !allowed {
			if failReason != "" {
				addReason(failReason)
			} else {
				addReason(attest.ReasonPluginNotAllowed)
			}
		}
	}
	return reasons, unknown
}

// EvaluateReport resolves the policies for one user and evaluates
// the report in one call (login attestation and heartbeat drift
// share it). A nil report means no evidence: no reasons. It
// returns the verdict reasons, the unknown plugin IDs (denylist
// mode), and the strictest disposition among the applicable
// policies ("" when no policy applies).
func EvaluateReport(report *attest.Report, policies map[string]*PluginPolicy, department string, groups []string, now time.Time) (reasons []string, unknown []string, action string) {
	if report == nil {
		return nil, nil, ""
	}
	eff := Resolve(policies, department, groups, now)
	if eff == nil {
		return nil, nil, ""
	}
	var evidence []attest.PluginEvidence
	if len(report.PluginDetails) > 0 {
		evidence = report.PluginDetails
	} else {
		for _, name := range report.Plugins {
			evidence = append(evidence, attest.PluginEvidence{ID: name})
		}
	}
	reasons, unknown = Evaluate(evidence, eff)
	return reasons, unknown, eff.Action
}

// Disposition predicates (slice 2): the ladder is observe → warn →
// downgrade → block_sensitive. observe/warn only record (warn also
// alerts, via the terminal_drift / terminal_plugin_unknown audit
// events); downgrade drops the terminal's authorization tier to BYOA
// live (gateway + MCP level checks); block_sensitive adds the
// knowledge cut on top — company-knowledge tools are denied at
// every data level until remediation.
func DowngradesTier(action string) bool {
	return action == ActionDowngrade || action == ActionBlockSensitive
}

// BlocksKnowledge reports whether the disposition cuts sensitive
// capabilities entirely: only block_sensitive.
func BlocksKnowledge(action string) bool {
	return action == ActionBlockSensitive
}

// ExceptedPlugin is one approved plugin exception (backlog F38
// slice 3): the plugin triple it waives, for one applicant, until
// expiry. Empty Version/Hash/Source mean "any" — an exception that
// pins only the ID waives every version of that plugin for the
// applicant. The list is pre-filtered by the caller (approved,
// unexpired, this applicant); this package only matches triples.
// ExpiresAt is the waiver's expiry (zero when the caller did not
// supply one); matching never consults it.
type ExceptedPlugin struct {
	ID        string
	Version   string // "" = any version
	Hash      string // "" = any hash
	Source    string // "" = any source
	ExpiresAt time.Time
}

// Covers reports whether the exception waives the reported plugin:
// the ID must match (case-insensitive); a pinned version/hash/
// source must match the report — and a report that carries no
// evidence for a pinned field is NOT covered (fail-closed: a
// name-only report cannot prove it is the excepted version).
func (x ExceptedPlugin) Covers(ev attest.PluginEvidence) bool {
	if !strings.EqualFold(strings.TrimSpace(x.ID), strings.TrimSpace(ev.ID)) {
		return false
	}
	if x.Version != "" {
		if ev.Version == "" || attest.CompareVersion(ev.Version, x.Version) != 0 {
			return false
		}
	}
	if x.Hash != "" {
		if ev.Hash == "" || !strings.EqualFold(x.Hash, ev.Hash) {
			return false
		}
	}
	if x.Source != "" {
		if ev.Source == "" || !strings.EqualFold(x.Source, ev.Source) {
			return false
		}
	}
	return true
}

// coversAny reports whether any exception in the list waives ev.
func coversAny(excepted []ExceptedPlugin, ev attest.PluginEvidence) bool {
	for _, x := range excepted {
		if x.Covers(ev) {
			return true
		}
	}
	return false
}

// EvaluateWithExceptions appraises the report like Evaluate, but
// plugins covered by an approved exception are skipped entirely —
// no reasons, no unknown flag. The exception list is pre-filtered
// by the caller (approved, unexpired, this applicant).
func EvaluateWithExceptions(reported []attest.PluginEvidence, eff *Effective, excepted []ExceptedPlugin) (reasons []string, unknown []string) {
	if len(excepted) == 0 {
		return Evaluate(reported, eff)
	}
	kept := make([]attest.PluginEvidence, 0, len(reported))
	for _, ev := range reported {
		if coversAny(excepted, ev) {
			continue
		}
		kept = append(kept, ev)
	}
	return Evaluate(kept, eff)
}

// EvaluateReportWithExceptions resolves the policies for one user
// and evaluates the report with their plugin exceptions honoured —
// the shared entry login attestation and heartbeat drift use, so a
// waived plugin passes at login and stops drifting afterwards (or
// vice versa, once the exception expires).
func EvaluateReportWithExceptions(report *attest.Report, policies map[string]*PluginPolicy, excepted []ExceptedPlugin, department string, groups []string, now time.Time) (reasons []string, unknown []string, action string) {
	if report == nil {
		return nil, nil, ""
	}
	eff := Resolve(policies, department, groups, now)
	if eff == nil {
		return nil, nil, ""
	}
	var evidence []attest.PluginEvidence
	if len(report.PluginDetails) > 0 {
		evidence = report.PluginDetails
	} else {
		for _, name := range report.Plugins {
			evidence = append(evidence, attest.PluginEvidence{ID: name})
		}
	}
	reasons, unknown = EvaluateWithExceptions(evidence, eff, excepted)
	return reasons, unknown, eff.Action
}

// Asset statuses, worst first (used both for sorting and for the
// per-plugin worst-status rollup).
const (
	StatusViolating  = "violating"
	StatusUnknown    = "unknown"
	StatusUnverified = "unverified"
	// StatusExcepted is an install that would be violating or
	// unknown but is waived by an approved, unexpired plugin
	// exception for that terminal's user (slice 3). It is neither
	// a violation nor plain compliant — the admin can still see
	// that an exception is doing the work, and it flips back to
	// violating/unknown the moment the exception expires.
	StatusExcepted  = "excepted"
	StatusCompliant = "compliant"
)

func statusRank(s string) int {
	switch s {
	case StatusViolating:
		return 0
	case StatusUnknown:
		return 1
	case StatusUnverified:
		return 2
	case StatusExcepted:
		return 3
	case StatusCompliant:
		return 4
	}
	return 5
}

// Classify appraises one reported plugin against the resolved
// policy and returns its asset status plus the verdict reason code
// ("" when the status needs none):
//
//   - violating: the plugin matches a deny rule, or an allowlist
//     does not admit it (specific mismatch code when the rule
//     constrains a field the report contradicts, plugin_not_allowed
//     otherwise);
//   - unknown: denylist mode, no rule covers the plugin at all, and
//     the policy wants unknowns flagged (allowed by default, but
//     surfaced so a rename cannot hide);
//   - unverified: name-only evidence (no version, no hash) that is
//     not violating — it cannot prove a version/hash failure, so it
//     is flagged rather than failed;
//   - compliant: everything else.
func Classify(ev attest.PluginEvidence, eff *Effective) (status, reason string) {
	if eff != nil {
		// A deny rule always wins.
		for _, rule := range eff.DenyRules {
			if kind, _ := matchRule(rule, ev); kind == matchYes {
				return StatusViolating, attest.ReasonBlockedPlugin
			}
		}
		if eff.Mode == ModeAllowlist {
			allowed := true
			var failReason string
			for _, set := range eff.AllowSets {
				setOK := false
				var setFail string
				for _, rule := range set {
					kind, r := matchRule(rule, ev)
					if kind == matchYes || kind == matchPartial {
						setOK = true
						break
					}
					if r != "" && setFail == "" {
						setFail = r
					}
				}
				if !setOK {
					allowed = false
					failReason = setFail
					break
				}
			}
			if !allowed {
				if failReason == "" {
					failReason = attest.ReasonPluginNotAllowed
				}
				return StatusViolating, failReason
			}
		} else {
			// Denylist: flag plugins no rule covers at all.
			covered := false
			for _, rule := range eff.DenyRules {
				if kind, _ := matchRule(rule, ev); kind != matchNo {
					covered = true
					break
				}
			}
			if !covered && eff.WarnUnknown {
				return StatusUnknown, ""
			}
		}
	}
	if ev.Version == "" && ev.Hash == "" {
		return StatusUnverified, ""
	}
	return StatusCompliant, ""
}

// ClassifyWithExceptions classifies one reported plugin like
// Classify, but an install that would be violating or unknown and
// is waived by an approved exception reports "excepted" instead.
// A compliant or unverified install is never re-labelled: the
// exception is irrelevant to it, and the view should not suggest
// an exception is doing work it is not.
func ClassifyWithExceptions(ev attest.PluginEvidence, eff *Effective, excepted []ExceptedPlugin) (status, reason string) {
	status, reason = Classify(ev, eff)
	if status == StatusViolating || status == StatusUnknown {
		if coversAny(excepted, ev) {
			return StatusExcepted, ""
		}
	}
	return status, reason
}

// AssetTerminal is one terminal's plugin evidence for aggregation:
// the triple details when the sidecar reports them, the name-only
// list otherwise. UserID keys the terminal owner's approved plugin
// exceptions (slice 3); without it every install is judged on
// policy alone.
type AssetTerminal struct {
	ID         string
	UserID     string
	Username   string
	Department string
	AgentType  string
	Details    []attest.PluginEvidence
	Names      []string
}

// AssetInstall is one terminal's install of one plugin.
type AssetInstall struct {
	TerminalID string `json:"terminal_id"`
	Username   string `json:"username"`
	Department string `json:"department"`
	AgentType  string `json:"agent_type"`
	Version    string `json:"version,omitempty"`
	Hash       string `json:"hash,omitempty"`
	Source     string `json:"source,omitempty"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
}

// AssetRow is one plugin aggregated across terminals: install count,
// distinct versions, the worst status across installs, and the
// per-terminal installs behind the numbers (so the counts in the
// asset view always reconcile with the heartbeat evidence).
type AssetRow struct {
	ID        string         `json:"id"`
	Installs  int            `json:"installs"`
	Versions  []string       `json:"versions"`
	Status    string         `json:"status"`
	Reasons   []string       `json:"reasons,omitempty"`
	Terminals []AssetInstall `json:"terminals"`
}

// Aggregate rolls terminal plugin evidence up into per-plugin rows.
// Each terminal's plugins are classified against the policies that
// apply to its department (resolved once per department); a plugin
// with no evidence of a version or hash is unverified, never failed.
// Rows sort by worst status, then install count (descending), then
// ID; installs within a row sort by username, then agent type.
// No exception is honoured: use AggregateWithExceptions when the
// caller's approved plugin exceptions are known.
func Aggregate(terminals []AssetTerminal, policies map[string]*PluginPolicy, now time.Time) []AssetRow {
	return AggregateWithExceptions(terminals, policies, nil, now)
}

// AggregateWithExceptions is Aggregate with one terminal owner's
// approved, unexpired plugin exceptions honoured per install:
// exceptedByUser maps a terminal's UserID to its waivers, and an
// install waived for its owner reports "excepted" instead of
// violating/unknown. An exception for another user never waives
// this terminal's install.
func AggregateWithExceptions(terminals []AssetTerminal, policies map[string]*PluginPolicy, exceptedByUser map[string][]ExceptedPlugin, now time.Time) []AssetRow {
	byDept := map[string]*Effective{}
	effFor := func(department string) *Effective {
		if eff, ok := byDept[department]; ok {
			return eff
		}
		eff := Resolve(policies, department, nil, now)
		byDept[department] = eff
		return eff
	}
	rows := map[string]*AssetRow{}
	for _, t := range terminals {
		eff := effFor(t.Department)
		evidence := t.Details
		if len(evidence) == 0 {
			for _, name := range t.Names {
				evidence = append(evidence, attest.PluginEvidence{ID: name})
			}
		}
		// Dedupe one terminal's report by ID (first wins), as in
		// Evaluate: a client that lists the same plugin twice
		// must not double-count.
		seenID := map[string]bool{}
		for _, ev := range evidence {
			key := strings.ToLower(strings.TrimSpace(ev.ID))
			if key == "" || seenID[key] {
				continue
			}
			seenID[key] = true
			status, reason := ClassifyWithExceptions(ev, eff, exceptedByUser[t.UserID])
			row, ok := rows[key]
			if !ok {
				row = &AssetRow{ID: ev.ID}
				rows[key] = row
			}
			row.Terminals = append(row.Terminals, AssetInstall{
				TerminalID: t.ID, Username: t.Username, Department: t.Department,
				AgentType: t.AgentType, Version: ev.Version, Hash: ev.Hash,
				Source: ev.Source, Status: status, Reason: reason,
			})
		}
	}
	out := make([]AssetRow, 0, len(rows))
	for _, row := range rows {
		sort.Slice(row.Terminals, func(i, j int) bool {
			if row.Terminals[i].Username != row.Terminals[j].Username {
				return row.Terminals[i].Username < row.Terminals[j].Username
			}
			return row.Terminals[i].AgentType < row.Terminals[j].AgentType
		})
		row.Installs = len(row.Terminals)
		versions := map[string]bool{}
		reasons := map[string]bool{}
		worst := StatusCompliant
		for _, inst := range row.Terminals {
			if inst.Version != "" {
				versions[inst.Version] = true
			}
			if inst.Reason != "" {
				reasons[inst.Reason] = true
			}
			if statusRank(inst.Status) < statusRank(worst) {
				worst = inst.Status
			}
		}
		for v := range versions {
			row.Versions = append(row.Versions, v)
		}
		sort.Strings(row.Versions)
		for r := range reasons {
			row.Reasons = append(row.Reasons, r)
		}
		sort.Strings(row.Reasons)
		row.Status = worst
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if statusRank(out[i].Status) != statusRank(out[j].Status) {
			return statusRank(out[i].Status) < statusRank(out[j].Status)
		}
		if out[i].Installs != out[j].Installs {
			return out[i].Installs > out[j].Installs
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// parseConstraint splits a version constraint into operator and
// version. "" means "any version" (op "", ok); a bare version is
// an exact match.
func parseConstraint(c string) (op, ver string, ok bool) {
	c = strings.TrimSpace(c)
	if c == "" {
		return "", "", true
	}
	for _, p := range []string{">=", "<=", "!=", "==", ">", "<", "="} {
		if strings.HasPrefix(c, p) {
			ver = strings.TrimSpace(strings.TrimPrefix(c, p))
			if ver == "" || !attest.ValidVersion(ver) {
				return "", "", false
			}
			if p == "==" {
				p = "="
			}
			return p, ver, true
		}
	}
	if !attest.ValidVersion(c) {
		return "", "", false
	}
	return "=", c, true
}

// matchesConstraint reports whether version satisfies constraint
// (constraint already validated by parseConstraint).
func matchesConstraint(version, constraint string) bool {
	op, ver, ok := parseConstraint(constraint)
	if !ok {
		return false
	}
	if op == "" {
		return true
	}
	cmp := attest.CompareVersion(version, ver)
	switch op {
	case "=":
		return cmp == 0
	case "!=":
		return cmp != 0
	case ">=":
		return cmp >= 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case "<":
		return cmp < 0
	}
	return false
}
