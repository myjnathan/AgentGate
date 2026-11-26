// Package terminal implements continuous terminal inventory (backlog
// F23): the upgrade of F14's one-time login attestation into a
// standing proof. Each ag-guard sidecar reports a periodic heartbeat
// (version / config hash / binary hash / plugin list / guard state);
// the platform keeps one Terminal record per (user, agent type) with
// its last-seen time and any drift against the F14 appraisal policy,
// so admins get a live asset view instead of a login-time snapshot.
//
// The visibility model follows Fleet/osquery (fleetdm.com): hosts
// report vitals on a schedule, the server derives freshness from the
// age of the last report ("inventory-heartbeat-age-seconds" is the
// same idea as an osquery policy query), and a refetch is just "the
// next heartbeat". Fleet's model is a general device inventory; ours
// is narrower on purpose — one record per agent, drift judged by the
// attestation policy the platform already has, and nothing but
// metadata (no prompt text, no matched values, no file contents).
//
// The offline half of F23 lives here too: PolicySnapshot is the
// server policy the sidecar caches (banned/custom words, dry-run,
// offline max level). When the gateway is unreachable the sidecar
// keeps enforcing the cached snapshot and fails closed above the
// offline max level (IP-guard's offline policy cache is the model;
// see docs/integrations/ipguard.md). Heartbeats carry the sidecar's
// offline audit summary back, so the outage itself is audited.
package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/myjnathan/agentgate/internal/attest"
)

// Data levels, ordered low to high (same order as store.DataLevel;
// duplicated here so this package does not import store, which
// imports this package for the Terminal record).
const (
	LevelPublic       = "public"
	LevelInternal     = "internal"
	LevelConfidential = "confidential"
	LevelSecret       = "secret"
)

// levelRank orders levels for the offline fail-closed comparison.
func levelRank(level string) (int, bool) {
	switch level {
	case LevelPublic:
		return 0, true
	case LevelInternal:
		return 1, true
	case LevelConfidential:
		return 2, true
	case LevelSecret:
		return 3, true
	}
	return 0, false
}

// ParseLevel reports whether s is a known data level.
func ParseLevel(s string) bool {
	_, ok := levelRank(s)
	return ok
}

// DefaultOfflineMaxLevel is the highest data level the sidecar may
// still call "unreachable, retry later" for while the gateway is
// down. Anything above it is denied outright (fail-closed): a
// confidential request must never be queued for silent replay after
// reconnect, and must never be forwarded on a stale policy.
const DefaultOfflineMaxLevel = LevelInternal

// PolicySnapshot is the server policy a sidecar caches (backlog
// F23). Version is the SHA-256 of the canonical snapshot content,
// so the sidecar can tell a stale cache from a current one without
// trusting a timestamp. DryRun is the gateway's own mode, reported
// for visibility; the sidecar keeps its local dry-run flag and a
// refresh never flips it.
type PolicySnapshot struct {
	Version         string   `json:"version"`
	BannedWords     []string `json:"banned_words,omitempty"`
	CustomWords     []string `json:"custom_words,omitempty"`
	DryRun          bool     `json:"dry_run"`
	OfflineMaxLevel string   `json:"offline_max_level"`
}

// NewPolicySnapshot builds a snapshot and computes its version. Word
// lists are sorted so the same policy always hashes the same way
// regardless of the order it was configured in.
func NewPolicySnapshot(banned, custom []string, dryRun bool, offlineMaxLevel string) PolicySnapshot {
	if !ParseLevel(offlineMaxLevel) {
		offlineMaxLevel = DefaultOfflineMaxLevel
	}
	s := PolicySnapshot{
		BannedWords:     append([]string(nil), banned...),
		CustomWords:     append([]string(nil), custom...),
		DryRun:          dryRun,
		OfflineMaxLevel: offlineMaxLevel,
	}
	sort.Strings(s.BannedWords)
	sort.Strings(s.CustomWords)
	s.Version = snapshotVersion(s)
	return s
}

func snapshotVersion(s PolicySnapshot) string {
	canonical, _ := json.Marshal(struct {
		BannedWords     []string `json:"banned_words"`
		CustomWords     []string `json:"custom_words"`
		DryRun          bool     `json:"dry_run"`
		OfflineMaxLevel string   `json:"offline_max_level"`
	}{s.BannedWords, s.CustomWords, s.DryRun, s.OfflineMaxLevel})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// Verify reports whether the snapshot's version matches its
// content. A cache file that fails verification is ignored — a
// corrupt or hand-edited cache must never become the enforced
// policy.
func (s PolicySnapshot) Verify() bool {
	if s.Version == "" {
		return false
	}
	return s.Version == snapshotVersion(s)
}

// OfflineMaxLevelOrDefault returns the snapshot's offline ceiling,
// defaulting to internal for snapshots that predate the field.
func (s PolicySnapshot) OfflineMaxLevelOrDefault() string {
	if ParseLevel(s.OfflineMaxLevel) {
		return s.OfflineMaxLevel
	}
	return DefaultOfflineMaxLevel
}

// AllowsOffline reports whether a request at the given data level
// may be treated as "gateway unreachable, retry later" while
// offline. Higher levels are denied by the cached policy instead.
func (s PolicySnapshot) AllowsOffline(level string) bool {
	rank, ok := levelRank(level)
	if !ok {
		return false
	}
	max, _ := levelRank(s.OfflineMaxLevelOrDefault())
	return rank <= max
}

// OfflineSummary is the sidecar's backfill of decisions made while
// the gateway was unreachable (backlog F23). Counts and categories
// only — never matched text, never request content — so the backfill
// is safe to audit.
type OfflineSummary struct {
	Events      int              `json:"events"`
	Blocked     int              `json:"blocked"`
	Unreachable int              `json:"unreachable"`
	WindowStart time.Time        `json:"window_start"`
	WindowEnd   time.Time        `json:"window_end"`
	ByCategory  map[string]int64 `json:"by_category,omitempty"`
}

// Validate checks an offline summary's shape.
func (o OfflineSummary) Validate() error {
	if o.Events < 0 || o.Blocked < 0 || o.Unreachable < 0 {
		return fmt.Errorf("offline counts must be non-negative")
	}
	if o.Blocked+o.Unreachable > o.Events {
		return fmt.Errorf("offline blocked+unreachable exceeds events")
	}
	if !o.WindowStart.IsZero() && !o.WindowEnd.IsZero() && o.WindowEnd.Before(o.WindowStart) {
		return fmt.Errorf("offline window_end is before window_start")
	}
	if len(o.ByCategory) > 64 {
		return fmt.Errorf("too many offline categories (max 64)")
	}
	for k, v := range o.ByCategory {
		if k == "" || len(k) > 128 || v < 0 {
			return fmt.Errorf("invalid offline category %q", k)
		}
	}
	return nil
}

// Heartbeat is one periodic report from an ag-guard sidecar. The
// agent type and tier are NOT trusted from this body: the server
// overwrites the agent type from the bearer token and takes the tier
// from it too, so a heartbeat can only ever describe the agent that
// holds the token.
type Heartbeat struct {
	AgentType string `json:"agent_type,omitempty"`
	Version   string `json:"version"`
	// ConfigHash and BinaryHash are hex digests (see attest.Report).
	ConfigHash string `json:"config_hash,omitempty"`
	BinaryHash string `json:"binary_hash,omitempty"`
	// Plugins is the name-only list old sidecars report.
	// PluginDetails is the F38 triple evidence; when present it is
	// authoritative for plugin appraisal, same as attest.Report.
	Plugins       []string                `json:"plugins,omitempty"`
	PluginDetails []attest.PluginEvidence `json:"plugin_details,omitempty"`
	GuardApplied  bool                    `json:"guard_applied"`
	OS            string                  `json:"os,omitempty"`
	Offline       *OfflineSummary         `json:"offline,omitempty"`
}

// Validate checks a heartbeat's shape, reusing the attestation
// report rules (bounded sizes, hex hashes) so a hostile client
// cannot stuff the terminal inventory.
func (h Heartbeat) Validate() error {
	if len(h.AgentType) > 64 {
		return fmt.Errorf("agent_type too long")
	}
	if h.Version == "" || len(h.Version) > 64 {
		return fmt.Errorf("version must be 1-64 characters")
	}
	if !attest.ValidVersion(h.Version) {
		return fmt.Errorf("version %q is not a version", h.Version)
	}
	rep := h.Report()
	if err := rep.Validate(); err != nil {
		return err
	}
	if h.Offline != nil {
		if err := h.Offline.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Report converts the heartbeat into attestation evidence, so drift
// is judged by exactly the same appraisal as the login-time report
// (backlog F14). Plugin details ride along for the F38 appraisal.
func (h Heartbeat) Report() *attest.Report {
	return &attest.Report{
		AgentType:     h.AgentType,
		Version:       h.Version,
		ConfigHash:    h.ConfigHash,
		BinaryHash:    h.BinaryHash,
		Plugins:       h.Plugins,
		PluginDetails: h.PluginDetails,
		GuardApplied:  h.GuardApplied,
		OS:            h.OS,
	}
}

// DriftReasons appraises a heartbeat report against the agent type's
// attestation policy. A nil policy means no appraisal applies, so
// there is no drift to report. The reasons are the same stable
// category codes F14 audits (version_below_minimum,
// config_hash_not_allowed, ...), never the offending values.
func DriftReasons(report *attest.Report, policy *attest.Policy) []string {
	if policy == nil {
		return nil
	}
	if v := attest.Evaluate(report, *policy); v.Status == attest.VerdictDowngraded {
		return v.Reasons
	}
	return nil
}

// OnlineWindow is how recently a terminal must have reported to
// count as online; OfflineWindow is the age after which it counts
// as offline. Between the two it is stale — the same freshness
// ladder Fleet derives from osquery heartbeat age.
const (
	OnlineWindow  = 5 * time.Minute
	OfflineWindow = 60 * time.Minute
)

const (
	StatusOnline  = "online"
	StatusStale   = "stale"
	StatusOffline = "offline"
)

// Terminal is one enrolled endpoint agent: the standing record a
// heartbeat maintains. One record per (user, agent type); a second
// token for the same agent refreshes the same record.
type Terminal struct {
	ID         string   `json:"id"`
	UserID     string   `json:"user_id"`
	Username   string   `json:"username"`
	Department string   `json:"department"`
	AgentType  string   `json:"agent_type"`
	Tier       string   `json:"tier,omitempty"`
	Version    string   `json:"version"`
	ConfigHash string   `json:"config_hash,omitempty"`
	BinaryHash string   `json:"binary_hash,omitempty"`
	Plugins    []string `json:"plugins,omitempty"`
	// PluginDetails is the F38 triple evidence from the latest
	// heartbeat (empty for old sidecars that report names only);
	// the asset view (slice 2) aggregates from here.
	PluginDetails []attest.PluginEvidence `json:"plugin_details,omitempty"`
	// GuardApplied records whether the endpoint was running ag-guard
	// at the last heartbeat (F14's guard_applied, kept current).
	GuardApplied bool      `json:"guard_applied"`
	OS           string    `json:"os,omitempty"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	// HeartbeatCount counts accepted heartbeats since registration.
	HeartbeatCount int64 `json:"heartbeat_count"`
	// DriftReasons is the current appraisal against the F14 policy
	// (empty = no drift). Only reason codes, never hash values.
	DriftReasons []string `json:"drift_reasons,omitempty"`
	// PluginAction is the F38 disposition currently in force for
	// this terminal ("" = none): observe / warn / downgrade /
	// block_sensitive, set by the latest heartbeat's plugin
	// appraisal. A later heartbeat that reports clean plugins
	// clears it (auto-remediation, slice 2); the gateway and MCP
	// server read it on every call, so the disposition bites (and
	// lifts) without waiting for the next login.
	PluginAction string `json:"plugin_action,omitempty"`
	// PluginReasons are the plugin verdict reason codes driving
	// PluginAction (empty when none).
	PluginReasons []string `json:"plugin_reasons,omitempty"`
	// PluginUnknown lists denylist-unlisted plugin IDs flagged on
	// the latest heartbeat: allowed by default, flagged for the
	// admin (and for the asset view).
	PluginUnknown []string `json:"plugin_unknown,omitempty"`
	// PluginActionAt records when the current PluginAction took
	// effect (nil when no action is in force).
	PluginActionAt *time.Time `json:"plugin_action_at,omitempty"`
	// LastOfflineAudit is the most recent backfilled outage summary,
	// if any, and when the platform received it.
	LastOfflineAudit *OfflineSummary `json:"last_offline_audit,omitempty"`
	LastOfflineAt    *time.Time      `json:"last_offline_at,omitempty"`
}

// Status derives freshness from the last heartbeat's age.
func (t *Terminal) Status(now time.Time) string {
	age := now.Sub(t.LastSeen)
	switch {
	case age <= OnlineWindow:
		return StatusOnline
	case age <= OfflineWindow:
		return StatusStale
	default:
		return StatusOffline
	}
}

// IsDrifted reports whether the terminal currently fails its
// attestation policy.
func (t *Terminal) IsDrifted() bool {
	return len(t.DriftReasons) > 0
}
