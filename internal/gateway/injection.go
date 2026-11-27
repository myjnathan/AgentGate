package gateway

import (
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/inject"
	"github.com/myjnathan/agentgate/internal/store"
)

// Prompt-injection rule layer (backlog F39, slice 1) wiring: the
// gateway runs the rule check after the rule DLP and after the
// redaction decision, before the semantic review and before the
// cache — so a blocked request never reaches a channel, never
// lands in the cache, and never spends an F24 exception grant.
// A grant never waives an injection block: like a banned word,
// this is a safety layer, not a classification waiver.
//
// The check scans the original per-message text (pre-redaction):
// injection is about instructions, not PII, and the scan is
// purely local, so nothing leaves the process. What is recorded
// is only the source, rule id, category and severity — never
// request text.

// InjectionStats returns the runtime counters for the layer.
func (g *Gateway) InjectionStats() inject.Stats {
	g.injMu.Lock()
	defer g.injMu.Unlock()
	return g.injStats
}

// injectionConfig reads the stored policy, falling back to the
// built-in defaults when the admin never configured the layer.
func (g *Gateway) injectionConfig() *store.InjectionConfig {
	var cfg *store.InjectionConfig
	g.st.Read(func(st *store.State) {
		if st.Injection != nil {
			cp := *st.Injection
			cfg = &cp
		}
	})
	if cfg == nil {
		return inject.DefaultConfig()
	}
	return cfg
}

// runInjectionCheck executes one rule check and records the
// runtime counters. Messages are tagged by role (see
// inject.Message); tool_calls arguments are scanned as tool
// source even on an assistant message — they are parameters the
// model is about to execute, not user-typed text.
func (g *Gateway) runInjectionCheck(msgs []chatMessage, level store.DataLevel) inject.Decision {
	var in []inject.Message
	for _, m := range msgs {
		in = append(in, inject.Message{Role: m.Role, Text: contentText(m.Content)})
		for _, tc := range m.ToolCalls {
			in = append(in, inject.Message{Role: "tool",
				Text: tc.Function.Name + "\n" + tc.Function.Arguments})
		}
	}
	dec := inject.Scan(in, level, g.injectionConfig())
	g.injMu.Lock()
	g.injStats.Reviewed++
	switch dec.Action {
	case inject.DecisionBlock:
		g.injStats.Blocked++
	case inject.DecisionAlert:
		g.injStats.Alerted++
	case inject.DecisionObserve:
		g.injStats.Observed++
	}
	g.injMu.Unlock()
	return dec
}

// auditInjection writes the decision-log event for a check that
// alerted, observed, or would have blocked under DryRun. It
// carries the source, rule id, category and severity — never
// request text. Blocks are audited as violation /
// model_call_blocked by the caller instead, so one block produces
// exactly one alertable event.
func (g *Gateway) auditInjection(user *store.User, tok *store.APIToken, model string, level store.DataLevel, dec inject.Decision) {
	detail := map[string]string{
		"source":   dec.Source,
		"rule":     dec.RuleID,
		"category": dec.Category,
		"severity": dec.Severity,
	}
	g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "prompt_injection", Action: "prompt_injection_" + dec.Action, Model: model,
		DataLevel: level.String(), Decision: dec.Action, Detail: detail})
}

// injectionDetail is the one-line check outcome recorded on the
// model_call event, so every checked call is traceable in the
// audit trail even when the check allowed it (the same
// convention as the F28 semantic_review detail).
func injectionDetail(dec inject.Decision) string {
	if dec.Reason == "disabled" {
		return ""
	}
	if dec.Reason == "no_match" {
		return "rule:allow"
	}
	return dec.Source + ":" + dec.Action
}
