package report_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/report"
)

func ev(kind, action string, at time.Time) audit.Event {
	return audit.Event{Kind: kind, Action: action, Time: at, Actor: "lixiaofeng", ActorID: "u1",
		AgentType: "dsh", Tier: "managed", Model: "mock-pro", Channel: "ch_cloud",
		DataLevel: "internal", Decision: "allow", Tokens: 10, CostUSD: 0.001}
}

func TestPeriodWindow(t *testing.T) {
	// Sunday 2026-11-22 belongs to the week starting Monday 2026-11-16.
	start, end, err := report.PeriodWindow("weekly", time.Date(2026, 11, 22, 15, 4, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := start.Format("2006-01-02"), "2026-11-16"; got != want {
		t.Fatalf("weekly start = %s, want %s", got, want)
	}
	if got, want := end.Format("2006-01-02"), "2026-11-23"; got != want {
		t.Fatalf("weekly end = %s, want %s", got, want)
	}
	// Monday itself starts its own week.
	start, _, _ = report.PeriodWindow("weekly", time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))
	if got, want := start.Format("2006-01-02"), "2026-11-16"; got != want {
		t.Fatalf("monday start = %s, want %s", got, want)
	}
	// February in a leap year: 2028-02 has 29 days.
	start, end, err = report.PeriodWindow("monthly", time.Date(2028, 2, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := start.Format("2006-01-02"), "2028-02-01"; got != want {
		t.Fatalf("monthly start = %s, want %s", got, want)
	}
	if got, want := end.Format("2006-01-02"), "2028-03-01"; got != want {
		t.Fatalf("monthly end = %s, want %s", got, want)
	}
	if _, _, err := report.PeriodWindow("daily", time.Now()); err == nil {
		t.Fatal("unknown period must error, not silently cover a wrong span")
	}
	if report.ValidatePeriod("weekly") != true || report.ValidatePeriod("monthly") != true ||
		report.ValidatePeriod("yearly") != false {
		t.Fatal("ValidatePeriod mismatch")
	}
}

func TestGenerate(t *testing.T) {
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 7)
	in := func(d int) time.Time { return start.AddDate(0, 0, d) }

	events := []audit.Event{
		ev("model_call", "", in(0)), // allow, 10 tokens
		func() audit.Event {
			e := ev("model_call", "", in(1))
			e.Decision = "redact"
			e.DataLevel = "confidential"
			return e
		}(),
		func() audit.Event { e := ev("model_call", "", in(2)); e.Decision = "dry_run_allow"; return e }(),
		func() audit.Event { // blocked: banned word, findings are categories only
			e := ev("violation", "model_call_blocked", in(1))
			e.Decision = "block"
			e.Detail = map[string]string{"findings": "banned_word,cn_id_card"}
			e.Tokens = 0
			e.CostUSD = 0
			return e
		}(),
		func() audit.Event { // blocked: no compliant channel
			e := ev("violation", "model_call_blocked", in(3))
			e.Decision = "block"
			e.Detail = map[string]string{"reason": "no_compliant_channel"}
			e.Tokens = 0
			return e
		}(),
		func() audit.Event { // blocked by another actor
			e := ev("violation", "model_call_blocked", in(3))
			e.Actor = "wangwu"
			e.Decision = "block"
			e.Detail = map[string]string{"reason": "department_budget_exceeded"}
			e.Tokens = 0
			return e
		}(),
		func() audit.Event { e := ev("auth", "account_locked", in(4)); e.Tokens = 0; return e }(),
		func() audit.Event { e := ev("budget", "budget_threshold", in(4)); e.Tokens = 0; return e }(),
		func() audit.Event { e := ev("rate_limit", "rate_limited", in(5)); e.Tokens = 0; return e }(),
		func() audit.Event { e := ev("exception", "exception_requested", in(2)); e.Tokens = 0; return e }(),
		func() audit.Event { e := ev("exception", "exception_approved", in(2)); e.Tokens = 0; return e }(),
		func() audit.Event { e := ev("exception", "exception_grant_used", in(3)); e.Tokens = 0; return e }(),
		// Outside the window: must not be counted anywhere.
		func() audit.Event { e := ev("model_call", "", start.AddDate(0, 0, 7)); e.Tokens = 99; return e }(),
		func() audit.Event { e := ev("model_call", "", start.AddDate(0, 0, -1)); e.Tokens = 99; return e }(),
	}
	snap := report.Snapshot{
		Channels: []report.ChannelHealth{
			{Name: "签约云", Health: "healthy"},
			{Name: "本地模型", Health: "cooling_down", ConsecutiveFailures: 3},
			{Name: "旧渠道", Health: "disabled"},
		},
		Tokens:     report.TokenOverview{Total: 4, Managed: 2, BYOA: 2, Attested: 1, Revoked: 1},
		Terminals:  report.TerminalOverview{Total: 3, Online: 1, Stale: 1, Offline: 1, Drifted: 1},
		ChainValid: true,
		DeptOf:     func(id string) string { return "研发部" },
	}
	rep := report.Generate(events, snap, "weekly", start, end, "admin")
	if rep.Calls != 6 {
		t.Fatalf("calls = %d, want 6 (3 model_call + 3 blocked)", rep.Calls)
	}
	if rep.Blocked != 3 {
		t.Fatalf("blocked = %d, want 3", rep.Blocked)
	}
	if rep.Tokens != 30 {
		t.Fatalf("tokens = %d, want 30", rep.Tokens)
	}
	if rep.Desensitised != 1 {
		t.Fatalf("desensitised = %d, want 1", rep.Desensitised)
	}
	if rep.DryRun != 1 {
		t.Fatalf("dry_run = %d, want 1", rep.DryRun)
	}
	if rep.Audit.EventsInWindow != 12 {
		t.Fatalf("events_in_window = %d, want 12", rep.Audit.EventsInWindow)
	}
	if !rep.Audit.ChainValid {
		t.Fatal("chain_valid must pass through from the snapshot")
	}
	if rep.ChannelTotal != 3 || rep.ChannelHealthy != 1 || rep.ChannelCooling != 1 || rep.ChannelDisabled != 1 {
		t.Fatalf("channel health = %d/%d/%d/%d, want 3/1/1/1",
			rep.ChannelTotal, rep.ChannelHealthy, rep.ChannelCooling, rep.ChannelDisabled)
	}
	if rep.Exceptions.Requested != 1 || rep.Exceptions.Approved != 1 || rep.Exceptions.GrantsUsed != 1 {
		t.Fatalf("exceptions = %+v, want requested/approved/grants_used = 1", rep.Exceptions)
	}
	if len(rep.ByDepartment) != 1 || rep.ByDepartment[0].Key != "研发部" || rep.ByDepartment[0].Calls != 6 {
		t.Fatalf("by_department = %+v, want one 研发部 row with 6 calls", rep.ByDepartment)
	}
	byReason := map[string]int64{}
	for _, c := range rep.BlockedByReason {
		byReason[c.Key] = c.Count
	}
	if byReason["banned_word"] != 1 || byReason["no_compliant_channel"] != 1 || byReason["department_budget_exceeded"] != 1 {
		t.Fatalf("blocked_by_reason = %v", byReason)
	}
	byFinding := map[string]int64{}
	for _, c := range rep.BlockedByFinding {
		byFinding[c.Key] = c.Count
	}
	if byFinding["banned_word"] != 1 || byFinding["cn_id_card"] != 1 {
		t.Fatalf("blocked_by_finding = %v", byFinding)
	}
	if len(rep.BlockedByUser) != 2 || rep.BlockedByUser[0].Key != "lixiaofeng" || rep.BlockedByUser[0].Count != 2 {
		t.Fatalf("blocked_by_user = %+v, want lixiaofeng first with 2", rep.BlockedByUser)
	}
	// Top risk events: the blocked shape (3) first, then the three
	// singletons in deterministic order. The blocked shape's reason
	// is the most frequent one inside the group (all three appear
	// once, so the alphabetically first wins: banned_word).
	if len(rep.TopRiskEvents) != 4 {
		t.Fatalf("top_risk_events = %d, want 4 shapes", len(rep.TopRiskEvents))
	}
	top := rep.TopRiskEvents[0]
	if top.Kind != "violation" || top.Action != "model_call_blocked" || top.Count != 3 || top.Reason != "banned_word" {
		t.Fatalf("top risk = %+v, want violation/model_call_blocked/banned_word x3", top)
	}
	if rep.Note == "" || !strings.Contains(rep.Note, "不是合规认证") {
		t.Fatalf("report must carry the evidence-not-certification note, got %q", rep.Note)
	}
}

func TestGenerateEmptyWindow(t *testing.T) {
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	rep := report.Generate(nil, report.Snapshot{}, "monthly", start, start.AddDate(0, 1, 0), "admin")
	if rep.Calls != 0 || rep.Blocked != 0 || rep.Audit.EventsInWindow != 0 {
		t.Fatalf("empty window must be all zeros, got %+v", rep)
	}
	if rep.Channels == nil || rep.ByDepartment == nil {
		t.Fatal("empty-window report must still serialise slices as [], not null")
	}
}

func TestWriteCSV(t *testing.T) {
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	events := []audit.Event{
		func() audit.Event {
			e := ev("violation", "model_call_blocked", start.Add(time.Hour))
			e.Decision = "block"
			e.Detail = map[string]string{"findings": "banned_word"}
			e.Tokens = 0
			return e
		}(),
	}
	rep := report.Generate(events, report.Snapshot{DeptOf: func(string) string { return "研发部" }},
		"weekly", start, start.AddDate(0, 0, 7), "admin")
	rep.ID = "rep_test"
	var buf bytes.Buffer
	if err := report.WriteCSV(&buf, rep); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"report_id,rep_test",
		"section,key,calls,tokens,blocked,cost_usd",
		"usage,total,1,0,1,",
		"blocked_by_finding,banned_word,1,0,0,0",
		"top_risk_events,violation/model_call_blocked/banned_word,1,0,0,0",
		"audit,events_in_window,1,0,0,0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("csv missing %q:\n%s", want, out)
		}
	}
	// The CSV carries counts and category names only: no actor
	// content beyond usernames, no request text.
	if strings.Contains(out, "sk-") || strings.Contains(out, "13800138000") {
		t.Fatal("csv must not leak sensitive values")
	}
}
