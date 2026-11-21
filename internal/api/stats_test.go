package api

import (
	"math"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
)

// TestAggregateStatsCost covers the F13 cost columns: priced calls
// sum into cost_usd per bucket and totals, and calls on unpriced
// models are counted separately as unpriced_calls.
func TestAggregateStatsCost(t *testing.T) {
	now := time.Now().UTC()
	ev := func(kind, action, actorID, model, channel string, tokens int64, cost float64) audit.Event {
		return audit.Event{Kind: kind, Action: action, Actor: actorID, ActorID: actorID,
			Model: model, Channel: channel, DataLevel: "internal", Decision: "allow",
			Tokens: tokens, CostUSD: cost, Time: now}
	}
	events := []audit.Event{
		ev("model_call", "chat_completions", "u1", "mock-eco", "eco", 100, 0.0002),
		ev("model_call", "chat_completions", "u1", "mock-pro", "pro", 200, 0.001),
		ev("model_call", "chat_completions", "u1", "mock-local", "local", 300, 0), // unpriced: tokens but no cost
		ev("violation", "model_call_blocked", "u1", "mock-pro", "", 0, 0),         // blocked: no cost, not unpriced
	}
	deptOf := func(id string) string { return "研发部" }

	s := aggregateStats(events, deptOf, time.Time{}, time.Time{})
	if s.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", s.Currency)
	}
	// Float sums are compared with tolerance: 0.0002 + 0.001 is
	// 0.0012000000000000001 in binary floating point.
	if math.Abs(s.Totals.CostUSD-0.0012) > 1e-12 {
		t.Fatalf("totals cost = %v, want 0.0012", s.Totals.CostUSD)
	}
	if s.Totals.UnpricedCalls != 1 {
		t.Fatalf("unpriced_calls = %d, want 1", s.Totals.UnpricedCalls)
	}
	find := func(bs []statsBucket, key string) *statsBucket {
		for i := range bs {
			if bs[i].Key == key {
				return &bs[i]
			}
		}
		return nil
	}
	if b := find(s.ByModel, "mock-eco"); b == nil || b.CostUSD != 0.0002 {
		t.Fatalf("by_model[mock-eco] = %+v, want cost 0.0002", b)
	}
	if b := find(s.ByModel, "mock-local"); b == nil || b.CostUSD != 0 || b.Tokens != 300 {
		t.Fatalf("by_model[mock-local] = %+v, want cost 0 tokens 300", b)
	}
	if b := find(s.ByDepartment, "研发部"); b == nil || math.Abs(b.CostUSD-0.0012) > 1e-12 || b.Calls != 4 {
		t.Fatalf("by_department[研发部] = %+v, want cost 0.0012 calls 4", b)
	}
}

// TestAggregateStats covers the pure aggregation: totals, every
// dimension, blocked calls, out-of-scope events, and the time window.
func TestAggregateStats(t *testing.T) {
	now := time.Now().UTC()
	ev := func(kind, action, actor, actorID, model, channel, level, decision, agent string, tokens int64, at time.Time) audit.Event {
		return audit.Event{Kind: kind, Action: action, Actor: actor, ActorID: actorID,
			Model: model, Channel: channel, DataLevel: level, Decision: decision,
			AgentType: agent, Tokens: tokens, Time: at}
	}
	events := []audit.Event{
		ev("model_call", "chat_completions", "lixiaofeng", "u1", "mock-eco", "eco", "internal", "allow", "claude-code", 100, now),
		ev("model_call", "chat_completions", "lixiaofeng", "u1", "mock-eco", "eco", "internal", "redact", "claude-code", 50, now),
		ev("model_call", "chat_completions", "admin", "u2", "mock-pro", "pro", "confidential", "reroute", "dsh", 200, now),
		ev("violation", "model_call_blocked", "lixiaofeng", "u1", "mock-pro", "", "confidential", "block", "claude-code", 0, now),
		// Out of scope: must never be counted.
		ev("auth", "login", "lixiaofeng", "u1", "", "", "", "allow", "", 0, now),
		ev("channel", "channel_cooldown", "system", "", "", "eco", "", "cooldown", "", 0, now),
		ev("tool_call", "kb_search", "lixiaofeng", "u1", "", "", "", "allow", "claude-code", 0, now),
		// Old event: only counted without a window.
		ev("model_call", "chat_completions", "lixiaofeng", "u1", "mock-eco", "eco", "internal", "allow", "claude-code", 999, now.Add(-48*time.Hour)),
	}
	depts := map[string]string{"u1": "研发部", "u2": "IT"}
	deptOf := func(id string) string { return depts[id] }

	find := func(bs []statsBucket, key string) *statsBucket {
		for i := range bs {
			if bs[i].Key == key {
				return &bs[i]
			}
		}
		return nil
	}

	s := aggregateStats(events, deptOf, time.Time{}, time.Time{})
	if s.Totals.Calls != 5 || s.Totals.Tokens != 1349 || s.Totals.Blocked != 1 {
		t.Fatalf("totals = %+v, want calls=5 tokens=1349 blocked=1", s.Totals)
	}
	if b := find(s.ByDepartment, "研发部"); b == nil || b.Calls != 4 || b.Tokens != 1149 || b.Blocked != 1 {
		t.Fatalf("by_department[研发部] = %+v, want calls=4 tokens=1149 blocked=1", b)
	}
	if b := find(s.ByDepartment, "IT"); b == nil || b.Calls != 1 || b.Tokens != 200 {
		t.Fatalf("by_department[IT] = %+v, want calls=1 tokens=200", b)
	}
	if b := find(s.ByUser, "lixiaofeng"); b == nil || b.Calls != 4 {
		t.Fatalf("by_user[lixiaofeng] = %+v, want calls=4", b)
	}
	if b := find(s.ByChannel, "eco"); b == nil || b.Calls != 3 || b.Tokens != 1149 {
		t.Fatalf("by_channel[eco] = %+v, want calls=3 tokens=1149", b)
	}
	if b := find(s.ByChannel, "none"); b == nil || b.Calls != 1 || b.Blocked != 1 {
		t.Fatalf("by_channel[none] = %+v, want calls=1 blocked=1", b)
	}
	if b := find(s.ByModel, "mock-eco"); b == nil || b.Calls != 3 {
		t.Fatalf("by_model[mock-eco] = %+v, want calls=3", b)
	}
	if b := find(s.ByAgentType, "claude-code"); b == nil || b.Calls != 4 {
		t.Fatalf("by_agent_type[claude-code] = %+v, want calls=4", b)
	}
	if b := find(s.ByDataLevel, "confidential"); b == nil || b.Calls != 2 || b.Blocked != 1 {
		t.Fatalf("by_data_level[confidential] = %+v, want calls=2 blocked=1", b)
	}
	if b := find(s.ByDecision, "block"); b == nil || b.Calls != 1 {
		t.Fatalf("by_decision[block] = %+v, want calls=1", b)
	}
	if b := find(s.ByDecision, "allow"); b == nil || b.Calls != 2 {
		t.Fatalf("by_decision[allow] = %+v, want calls=2", b)
	}
	// Buckets are sorted by calls desc, then key asc.
	if len(s.ByUser) < 2 || s.ByUser[0].Key != "lixiaofeng" || s.ByUser[1].Key != "admin" {
		t.Fatalf("by_user order = %v, want [lixiaofeng admin]", s.ByUser)
	}

	// Time window [now-24h, now+1h): the 48h-old event drops out.
	s = aggregateStats(events, deptOf, now.Add(-24*time.Hour), now.Add(time.Hour))
	if s.Totals.Calls != 4 || s.Totals.Tokens != 350 {
		t.Fatalf("window totals = %+v, want calls=4 tokens=350", s.Totals)
	}
	// Half-open window: until is exclusive.
	s = aggregateStats(events, deptOf, now.Add(-24*time.Hour), now)
	if s.Totals.Calls != 0 {
		t.Fatalf("until==event time: calls=%d, want 0 (half-open)", s.Totals.Calls)
	}
	// Empty input: all buckets empty, totals zero.
	s = aggregateStats(nil, deptOf, time.Time{}, time.Time{})
	if s.Totals.Calls != 0 || len(s.ByUser) != 0 || len(s.ByDecision) != 0 {
		t.Fatalf("empty input: %+v", s)
	}
}
