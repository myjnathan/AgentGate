// Package report builds periodic compliance reports (backlog F25).
//
// A report is a frozen snapshot of one period (weekly or monthly) made
// of statistics and metadata only — call counts and tokens, blocked
// and desensitised call counts, the top risk events by category,
// channel health, token and terminal overviews, and the exception
// flow. It never contains request or response text, matched sensitive
// values, or any other sensitive original: the numbers are aggregated
// from the hash-chained audit log (the same source F2 usage stats
// read), and the channel/token/terminal sections are point-in-time
// counts taken when the report is generated.
//
// The report is evidence for a review (等保 / 保密评审), not a
// compliance verdict: like Wazuh's framework dashboards (PCI DSS,
// HIPAA, GDPR, NIST 800-53), mapping events to control areas helps an
// auditor find the facts, but whether the organisation meets a
// framework is for the auditor to decide. The disclaimer on every
// report says so.
//
// Reports are generated on demand (POST /api/reports) and kept in the
// store; at most MaxStoredReports are retained (a year of weekly
// reports) and the oldest is dropped when a new one is generated.
package report

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
)

// MaxStoredReports caps how many generated reports the store keeps:
// a year of weekly reports. Older reports are dropped on generate.
const MaxStoredReports = 52

// PeriodWeekly covers one ISO week (Monday 00:00 UTC to next Monday);
// PeriodMonthly covers one calendar month (UTC).
const (
	PeriodWeekly  = "weekly"
	PeriodMonthly = "monthly"
)

// Bucket is one row of an aggregation: calls, tokens consumed, how
// many of the calls were blocked, and the priced cost in USD. The
// shape mirrors the F2 usage-stats bucket so the two agree by
// construction.
type Bucket struct {
	Key     string  `json:"key"`
	Calls   int64   `json:"calls"`
	Tokens  int64   `json:"tokens"`
	Blocked int64   `json:"blocked"`
	CostUSD float64 `json:"cost_usd"`
}

// Count is a named tally (a reason, a finding category, a user).
// Only category names and usernames appear here — never content.
type Count struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// RiskEvent is one recurring risk shape in the period: the audit kind
// and action, the blocking reason or finding category when there is
// one, and how often it happened.
type RiskEvent struct {
	Kind   string `json:"kind"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Count  int64  `json:"count"`
}

// ChannelHealth is one channel as the report sees it: name, health
// (healthy | cooling_down | disabled) and consecutive failures. No
// keys, no URLs with credentials.
type ChannelHealth struct {
	Name                string `json:"name"`
	Health              string `json:"health"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
}

// TokenOverview counts the platform's API tokens at generation time.
type TokenOverview struct {
	Total    int64 `json:"total"`
	Managed  int64 `json:"managed"`
	BYOA     int64 `json:"byoa"`
	Attested int64 `json:"attested"` // managed tokens issued after a passing attestation (F14)
	Revoked  int64 `json:"revoked"`
	Expired  int64 `json:"expired"` // past ExpiresAt and not revoked
}

// TerminalOverview counts the terminal inventory (F23) at generation
// time. Drifted means the terminal currently fails its attestation
// policy; a terminal is counted in exactly one of Online / Stale /
// Offline by heartbeat freshness.
type TerminalOverview struct {
	Total   int64 `json:"total"`
	Online  int64 `json:"online"`
	Stale   int64 `json:"stale"`
	Offline int64 `json:"offline"`
	Drifted int64 `json:"drifted"`
}

// ExceptionFlow counts the exception approval flow (F24) inside the
// period, from its audit events.
type ExceptionFlow struct {
	Requested  int64 `json:"requested"`
	Approved   int64 `json:"approved"`
	Rejected   int64 `json:"rejected"`
	Revoked    int64 `json:"revoked"`
	GrantsUsed int64 `json:"grants_used"`
}

// AuditSection records what the report itself was built from.
type AuditSection struct {
	EventsInWindow int64 `json:"events_in_window"`
	ChainValid     bool  `json:"chain_valid"`
}

// Report is one frozen periodic compliance report. Every section is
// statistics or metadata; the report carries no sensitive originals.
type Report struct {
	ID          string    `json:"id"`
	Period      string    `json:"period"` // weekly | monthly
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	GeneratedAt time.Time `json:"generated_at"`
	GeneratedBy string    `json:"generated_by"`

	// Usage totals and breakdowns, same event scope as F2 stats:
	// model_call events plus violation/model_call_blocked.
	Calls         int64   `json:"calls"`
	Tokens        int64   `json:"tokens"`
	Blocked       int64   `json:"blocked"`
	CostUSD       float64 `json:"cost_usd"`
	UnpricedCalls int64   `json:"unpriced_calls"`
	// Desensitised counts model calls whose response the pipeline
	// redacted before forwarding; DryRun counts calls the DryRun
	// observation mode only audited (F17's three-step rollout).
	Desensitised int64    `json:"desensitised"`
	DryRun       int64    `json:"dry_run"`
	ByDepartment []Bucket `json:"by_department"`
	ByChannel    []Bucket `json:"by_channel"`
	ByModel      []Bucket `json:"by_model"`
	ByDataLevel  []Bucket `json:"by_data_level"`
	ByDecision   []Bucket `json:"by_decision"`

	BlockedByReason  []Count `json:"blocked_by_reason"`
	BlockedByFinding []Count `json:"blocked_by_finding"`
	BlockedByUser    []Count `json:"blocked_by_user"` // top 5 actors by blocked calls

	// TopRiskEvents lists the most frequent risk shapes in the
	// period (blocked calls, account lockouts, budget warnings,
	// rate limits), most frequent first, capped at 8.
	TopRiskEvents []RiskEvent `json:"top_risk_events"`

	Channels        []ChannelHealth `json:"channels"`
	ChannelTotal    int             `json:"channel_total"`
	ChannelHealthy  int             `json:"channel_healthy"`
	ChannelCooling  int             `json:"channel_cooling_down"`
	ChannelDisabled int             `json:"channel_disabled"`

	TokenOverview TokenOverview    `json:"token_overview"`
	Terminals     TerminalOverview `json:"terminals"`
	Exceptions    ExceptionFlow    `json:"exceptions"`
	Audit         AuditSection     `json:"audit"`

	// Note is the honesty disclaimer carried by every report: the
	// report is audit evidence, not a compliance certification.
	Note string `json:"note"`
}

// Disclaimer is printed on every report and in the CSV export.
const Disclaimer = "本报表仅含统计与元数据，不含敏感原文；报表是审计证据，不是合规认证。"

// Snapshot is the point-in-time platform state a report freezes
// alongside the audit window: channel health, token counts and
// terminal counts as observed when the report is generated, plus the
// audit chain verdict and a department resolver for actors.
type Snapshot struct {
	Channels   []ChannelHealth
	Tokens     TokenOverview
	Terminals  TerminalOverview
	ChainValid bool
	// DeptOf maps an actor ID to a department; "" means unknown.
	DeptOf func(actorID string) string
}

// ValidatePeriod reports whether period is a supported report period.
func ValidatePeriod(period string) bool {
	return period == PeriodWeekly || period == PeriodMonthly
}

// PeriodWindow returns the half-open [start, end) window containing
// ref for the given period, in UTC. Weekly windows start on Monday
// (ISO week); monthly windows start on the 1st. An unknown period is
// an error — a report must never silently cover the wrong span.
func PeriodWindow(period string, ref time.Time) (time.Time, time.Time, error) {
	ref = ref.UTC()
	switch period {
	case PeriodWeekly:
		// time.Weekday: Sunday=0 … Saturday=6; shift so Monday=0.
		offset := (int(ref.Weekday()) + 6) % 7
		day := time.Date(ref.Year(), ref.Month(), ref.Day(), 0, 0, 0, 0, time.UTC)
		start := day.AddDate(0, 0, -offset)
		return start, start.AddDate(0, 0, 7), nil
	case PeriodMonthly:
		start := time.Date(ref.Year(), ref.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0), nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unknown period %q (weekly|monthly)", period)
	}
}

// inScope reports whether an audit event is a model call for usage
// totals, and whether it was blocked. Same rule as F2 stats, so the
// report reconciles with /api/stats over the same window.
func inScope(ev audit.Event) (ok, blocked bool) {
	switch {
	case ev.Kind == "model_call":
		return true, false
	case ev.Kind == "violation" && ev.Action == "model_call_blocked":
		return true, true
	default:
		return false, false
	}
}

// isRisk reports whether an audit event is a risk shape the report
// surfaces: a blocked call, an account lockout, a budget warning, or
// a rate-limit refusal.
func isRisk(ev audit.Event) bool {
	switch {
	case ev.Kind == "violation":
		return true
	case ev.Kind == "auth" && ev.Action == "account_locked":
		return true
	case ev.Kind == "budget" && ev.Action == "budget_threshold":
		return true
	case ev.Kind == "rate_limit":
		return true
	default:
		return false
	}
}

// riskReason is the grouping reason for a risk event: the policy
// reason when the event carries one, else the first DLP finding
// category, else "".
func riskReason(ev audit.Event) string {
	if ev.Detail == nil {
		return ""
	}
	if r := ev.Detail["reason"]; r != "" {
		return r
	}
	if f := ev.Detail["findings"]; f != "" {
		return strings.Split(f, ",")[0]
	}
	return ""
}

// Generate folds the audit events inside [start, end) plus the
// point-in-time snapshot into a frozen report. Events outside the
// window are ignored, so regenerating the same window after the
// period closes reproduces the same numbers.
func Generate(events []audit.Event, snap Snapshot, period string, start, end time.Time, by string) *Report {
	rep := &Report{
		Period:      period,
		PeriodStart: start.UTC(),
		PeriodEnd:   end.UTC(),
		GeneratedAt: time.Now().UTC(),
		GeneratedBy: by,
		Note:        Disclaimer,
	}
	rep.Channels = snap.Channels
	if rep.Channels == nil {
		rep.Channels = []ChannelHealth{}
	}
	for _, ch := range rep.Channels {
		rep.ChannelTotal++
		switch ch.Health {
		case "healthy":
			rep.ChannelHealthy++
		case "cooling_down":
			rep.ChannelCooling++
		case "disabled":
			rep.ChannelDisabled++
		}
	}
	rep.TokenOverview = snap.Tokens
	rep.Terminals = snap.Terminals
	rep.Audit.ChainValid = snap.ChainValid

	acc := map[string]map[string]*Bucket{}
	add := func(dim, key string, tokens int64, cost float64, blocked bool) {
		m := acc[dim]
		if m == nil {
			m = map[string]*Bucket{}
			acc[dim] = m
		}
		b := m[key]
		if b == nil {
			b = &Bucket{Key: key}
			m[key] = b
		}
		b.Calls++
		b.Tokens += tokens
		b.CostUSD += cost
		if blocked {
			b.Blocked++
		}
	}
	tally := func(m map[string]int64, key string) {
		if key == "" {
			key = "unknown"
		}
		m[key]++
	}
	reasons := map[string]int64{}
	findings := map[string]int64{}
	blockedByUser := map[string]int64{}
	// Risk shapes group by (kind, action); Reason is the most
	// frequent reason inside the group (ties break alphabetically),
	// so the dominant shape stays one row while the detail lives in
	// the blocked-by-reason section.
	type riskKey struct{ kind, action string }
	risks := map[riskKey]int64{}
	riskReasons := map[riskKey]map[string]int64{}
	deptOf := snap.DeptOf
	if deptOf == nil {
		deptOf = func(string) string { return "" }
	}

	for _, ev := range events {
		if ev.Time.Before(start) || !ev.Time.Before(end) {
			continue
		}
		rep.Audit.EventsInWindow++
		if inScope, blocked := inScope(ev); inScope {
			rep.Calls++
			rep.Tokens += ev.Tokens
			rep.CostUSD += ev.CostUSD
			if !blocked && ev.Tokens > 0 && ev.CostUSD == 0 {
				rep.UnpricedCalls++
			}
			if blocked {
				rep.Blocked++
				reason := riskReason(ev)
				if reason == "" {
					reason = "dlp_findings"
				}
				tally(reasons, reason)
				if f := ev.Detail["findings"]; f != "" {
					for _, k := range strings.Split(f, ",") {
						tally(findings, k)
					}
				}
				tally(blockedByUser, ev.Actor)
			} else {
				if ev.Decision == "redact" {
					rep.Desensitised++
				}
				if strings.HasPrefix(ev.Decision, "dry_run") {
					rep.DryRun++
				}
			}
			decision := ev.Decision
			if blocked {
				decision = "block"
			}
			unknown := func(s string) string {
				if s == "" {
					return "unknown"
				}
				return s
			}
			none := func(s string) string {
				if s == "" {
					return "none"
				}
				return s
			}
			add("department", unknown(deptOf(ev.ActorID)), ev.Tokens, ev.CostUSD, blocked)
			add("channel", none(ev.Channel), ev.Tokens, ev.CostUSD, blocked)
			add("model", unknown(ev.Model), ev.Tokens, ev.CostUSD, blocked)
			add("data_level", unknown(ev.DataLevel), ev.Tokens, ev.CostUSD, blocked)
			add("decision", unknown(decision), ev.Tokens, ev.CostUSD, blocked)
		}
		if isRisk(ev) {
			k := riskKey{kind: ev.Kind, action: ev.Action}
			risks[k]++
			if riskReasons[k] == nil {
				riskReasons[k] = map[string]int64{}
			}
			riskReasons[k][riskReason(ev)]++
		}
		switch {
		case ev.Kind == "exception" && ev.Action == "exception_requested":
			rep.Exceptions.Requested++
		case ev.Kind == "exception" && ev.Action == "exception_approved":
			rep.Exceptions.Approved++
		case ev.Kind == "exception" && ev.Action == "exception_rejected":
			rep.Exceptions.Rejected++
		case ev.Kind == "exception" && ev.Action == "exception_revoked":
			rep.Exceptions.Revoked++
		case ev.Kind == "exception" && ev.Action == "exception_grant_used":
			rep.Exceptions.GrantsUsed++
		}
	}

	flattenBuckets := func(dim string) []Bucket {
		m := acc[dim]
		out := make([]Bucket, 0, len(m))
		for _, b := range m {
			out = append(out, *b)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Calls != out[j].Calls {
				return out[i].Calls > out[j].Calls
			}
			return out[i].Key < out[j].Key
		})
		return out
	}
	rep.ByDepartment = flattenBuckets("department")
	rep.ByChannel = flattenBuckets("channel")
	rep.ByModel = flattenBuckets("model")
	rep.ByDataLevel = flattenBuckets("data_level")
	rep.ByDecision = flattenBuckets("decision")

	flattenCounts := func(m map[string]int64, limit int) []Count {
		out := make([]Count, 0, len(m))
		for k, v := range m {
			out = append(out, Count{Key: k, Count: v})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Count != out[j].Count {
				return out[i].Count > out[j].Count
			}
			return out[i].Key < out[j].Key
		})
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out
	}
	rep.BlockedByReason = flattenCounts(reasons, 0)
	rep.BlockedByFinding = flattenCounts(findings, 0)
	rep.BlockedByUser = flattenCounts(blockedByUser, 5)

	rep.TopRiskEvents = make([]RiskEvent, 0, len(risks))
	for k, n := range risks {
		reason := ""
		best := int64(-1)
		for r, c := range riskReasons[k] {
			if c > best || (c == best && r < reason) {
				reason, best = r, c
			}
		}
		rep.TopRiskEvents = append(rep.TopRiskEvents, RiskEvent{Kind: k.kind, Action: k.action, Reason: reason, Count: n})
	}
	sort.Slice(rep.TopRiskEvents, func(i, j int) bool {
		if rep.TopRiskEvents[i].Count != rep.TopRiskEvents[j].Count {
			return rep.TopRiskEvents[i].Count > rep.TopRiskEvents[j].Count
		}
		if rep.TopRiskEvents[i].Kind != rep.TopRiskEvents[j].Kind {
			return rep.TopRiskEvents[i].Kind < rep.TopRiskEvents[j].Kind
		}
		if rep.TopRiskEvents[i].Action != rep.TopRiskEvents[j].Action {
			return rep.TopRiskEvents[i].Action < rep.TopRiskEvents[j].Action
		}
		return rep.TopRiskEvents[i].Reason < rep.TopRiskEvents[j].Reason
	})
	if len(rep.TopRiskEvents) > 8 {
		rep.TopRiskEvents = rep.TopRiskEvents[:8]
	}
	return rep
}

// WriteCSV renders the report as a flat table for spreadsheets:
// one header block with the report identity, then one row per
// breakdown entry (section, key, calls, tokens, blocked, cost_usd).
// Rows carry counts and category names only — no sensitive originals.
func WriteCSV(w io.Writer, rep *Report) error {
	cw := csv.NewWriter(w)
	meta := func(k, v string) error {
		return cw.Write([]string{k, v})
	}
	if err := meta("report_id", rep.ID); err != nil {
		return err
	}
	if err := meta("period", rep.Period); err != nil {
		return err
	}
	if err := meta("period_start", rep.PeriodStart.Format(time.RFC3339)); err != nil {
		return err
	}
	if err := meta("period_end", rep.PeriodEnd.Format(time.RFC3339)); err != nil {
		return err
	}
	if err := meta("generated_at", rep.GeneratedAt.Format(time.RFC3339)); err != nil {
		return err
	}
	if err := meta("generated_by", rep.GeneratedBy); err != nil {
		return err
	}
	if err := meta("note", rep.Note); err != nil {
		return err
	}
	if err := cw.Write([]string{}); err != nil {
		return err
	}
	if err := cw.Write([]string{"section", "key", "calls", "tokens", "blocked", "cost_usd"}); err != nil {
		return err
	}
	row := func(section, key string, calls, tokens, blocked int64, cost float64) error {
		return cw.Write([]string{section, key,
			strconv.FormatInt(calls, 10), strconv.FormatInt(tokens, 10),
			strconv.FormatInt(blocked, 10), strconv.FormatFloat(cost, 'f', 6, 64)})
	}
	if err := row("usage", "total", rep.Calls, rep.Tokens, rep.Blocked, rep.CostUSD); err != nil {
		return err
	}
	if err := row("usage", "desensitised", rep.Desensitised, 0, 0, 0); err != nil {
		return err
	}
	if err := row("usage", "dry_run", rep.DryRun, 0, 0, 0); err != nil {
		return err
	}
	if err := row("usage", "unpriced_calls", rep.UnpricedCalls, 0, 0, 0); err != nil {
		return err
	}
	buckets := func(section string, bs []Bucket) error {
		for _, b := range bs {
			if err := row(section, b.Key, b.Calls, b.Tokens, b.Blocked, b.CostUSD); err != nil {
				return err
			}
		}
		return nil
	}
	if err := buckets("by_department", rep.ByDepartment); err != nil {
		return err
	}
	if err := buckets("by_channel", rep.ByChannel); err != nil {
		return err
	}
	if err := buckets("by_model", rep.ByModel); err != nil {
		return err
	}
	if err := buckets("by_data_level", rep.ByDataLevel); err != nil {
		return err
	}
	if err := buckets("by_decision", rep.ByDecision); err != nil {
		return err
	}
	counts := func(section string, cs []Count) error {
		for _, c := range cs {
			if err := row(section, c.Key, c.Count, 0, 0, 0); err != nil {
				return err
			}
		}
		return nil
	}
	if err := counts("blocked_by_reason", rep.BlockedByReason); err != nil {
		return err
	}
	if err := counts("blocked_by_finding", rep.BlockedByFinding); err != nil {
		return err
	}
	if err := counts("blocked_by_user", rep.BlockedByUser); err != nil {
		return err
	}
	for _, r := range rep.TopRiskEvents {
		key := r.Kind + "/" + r.Action
		if r.Reason != "" {
			key += "/" + r.Reason
		}
		if err := row("top_risk_events", key, r.Count, 0, 0, 0); err != nil {
			return err
		}
	}
	if err := row("channels", "total", int64(rep.ChannelTotal), 0, 0, 0); err != nil {
		return err
	}
	if err := row("channels", "healthy", int64(rep.ChannelHealthy), 0, 0, 0); err != nil {
		return err
	}
	if err := row("channels", "cooling_down", int64(rep.ChannelCooling), 0, 0, 0); err != nil {
		return err
	}
	if err := row("channels", "disabled", int64(rep.ChannelDisabled), 0, 0, 0); err != nil {
		return err
	}
	for _, ch := range rep.Channels {
		if err := row("channel", ch.Name, int64(ch.ConsecutiveFailures), 0, 0, 0); err != nil {
			return err
		}
	}
	if err := row("tokens", "total", rep.TokenOverview.Total, 0, 0, 0); err != nil {
		return err
	}
	if err := row("tokens", "managed", rep.TokenOverview.Managed, 0, 0, 0); err != nil {
		return err
	}
	if err := row("tokens", "byoa", rep.TokenOverview.BYOA, 0, 0, 0); err != nil {
		return err
	}
	if err := row("tokens", "attested", rep.TokenOverview.Attested, 0, 0, 0); err != nil {
		return err
	}
	if err := row("tokens", "revoked", rep.TokenOverview.Revoked, 0, 0, 0); err != nil {
		return err
	}
	if err := row("tokens", "expired", rep.TokenOverview.Expired, 0, 0, 0); err != nil {
		return err
	}
	if err := row("terminals", "total", rep.Terminals.Total, 0, 0, 0); err != nil {
		return err
	}
	if err := row("terminals", "online", rep.Terminals.Online, 0, 0, 0); err != nil {
		return err
	}
	if err := row("terminals", "stale", rep.Terminals.Stale, 0, 0, 0); err != nil {
		return err
	}
	if err := row("terminals", "offline", rep.Terminals.Offline, 0, 0, 0); err != nil {
		return err
	}
	if err := row("terminals", "drifted", rep.Terminals.Drifted, 0, 0, 0); err != nil {
		return err
	}
	if err := row("exceptions", "requested", rep.Exceptions.Requested, 0, 0, 0); err != nil {
		return err
	}
	if err := row("exceptions", "approved", rep.Exceptions.Approved, 0, 0, 0); err != nil {
		return err
	}
	if err := row("exceptions", "rejected", rep.Exceptions.Rejected, 0, 0, 0); err != nil {
		return err
	}
	if err := row("exceptions", "revoked", rep.Exceptions.Revoked, 0, 0, 0); err != nil {
		return err
	}
	if err := row("exceptions", "grants_used", rep.Exceptions.GrantsUsed, 0, 0, 0); err != nil {
		return err
	}
	if err := row("audit", "events_in_window", rep.Audit.EventsInWindow, 0, 0, 0); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}
