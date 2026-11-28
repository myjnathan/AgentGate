package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/inject"
	"github.com/myjnathan/agentgate/internal/semreview"
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
//
// Slice 2 adds the semantic route (sample matching over the
// injection library, then the F28 deep guard-model endpoint) and
// the session-suspicion marking that the MCP gateway enforces
// on high-risk tool calls. See internal/inject/semantic.go for
// the route's red lines.

// InjectionStats returns the runtime counters for the layer.
// SuspectSessions is read off the suspicion tracker at call
// time, so the number is current, not a snapshot.
func (g *Gateway) InjectionStats() inject.Stats {
	g.injMu.Lock()
	defer g.injMu.Unlock()
	st := g.injStats
	if g.injSuspicion != nil {
		st.SuspectSessions = int64(g.injSuspicion.Count())
	}
	return st
}

// InjectionSuspicion returns the session-suspicion tracker shared
// with the MCP gateway (which enforces the action fallback).
// It is created with the gateway and never nil in production;
// tests that build a Gateway through New always get one.
func (g *Gateway) InjectionSuspicion() *inject.Tracker {
	return g.injSuspicion
}

// injectionMessages tags one request's messages for the
// injection layers: content per message, tool_calls arguments
// as tool-source messages (they are parameters the model is
// about to execute, not user-typed text).
func injectionMessages(msgs []chatMessage) []inject.Message {
	var in []inject.Message
	for _, m := range msgs {
		in = append(in, inject.Message{Role: m.Role, Text: contentText(m.Content)})
		for _, tc := range m.ToolCalls {
			in = append(in, inject.Message{Role: "tool",
				Text: tc.Function.Name + "\n" + tc.Function.Arguments})
		}
	}
	return in
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
	dec := inject.Scan(injectionMessages(msgs), level, g.injectionConfig())
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

// runInjectionSemanticCheck executes one semantic-route check
// and records the runtime counters. It runs when the rule layer
// did not block. The sample layer reads the original non-user
// text (local matching — nothing leaves the process); the deep
// layer, when F28's deep endpoint is enabled, reads the
// desensitized non-user text parsed out of the redacted review
// copy. When that copy cannot be parsed (defensive only — it
// was valid JSON a moment ago) the deep call is skipped rather
// than fed raw text; the sample verdict still stands.
func (g *Gateway) runInjectionSemanticCheck(ctx context.Context, msgs []chatMessage, reviewBody []byte, level store.DataLevel) inject.SemanticDecision {
	cfg := g.injectionConfig()
	sampleText := inject.NonUserText(injectionMessages(msgs))
	deepText := ""
	if len(reviewBody) > 0 {
		var req chatRequest
		if err := json.Unmarshal(reviewBody, &req); err == nil {
			deepText = inject.NonUserText(injectionMessages(req.Messages))
		}
	}
	semCfg := g.semReviewConfig()
	timeout := semCfg.TimeoutMS
	if timeout <= 0 {
		timeout = semreview.DefaultTimeoutMS
	}
	var deep semreview.DeepFunc
	if semCfg.DeepEnabled {
		deep = g.semReviewDeep(semCfg)
	}
	dctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	dec := inject.SemanticReview(dctx, sampleText, deepText, level, cfg, deep)
	g.injMu.Lock()
	g.injStats.SemanticReviewed++
	switch dec.Action {
	case inject.DecisionBlock:
		g.injStats.SemanticBlocked++
	case inject.DecisionAlert:
		g.injStats.SemanticAlerted++
	}
	switch dec.Source {
	case semreview.SourceDeep, semreview.SourceTimeout, semreview.SourceDeepError:
		g.injStats.SemanticDeepCalls++
	}
	g.injMu.Unlock()
	return dec
}

// markInjectionSuspicion marks this token's session suspect when
// an injection hit came from non-user content: the agent was fed
// an instruction it may act on, so its next high-risk tool calls
// are refused until the suspicion window passes. User-typed hits
// never mark — the user is the principal instructing their own
// agent — and neither do observe-mode hits or DryRun would-be
// blocks (the operator chose watch-only there).
func (g *Gateway) markInjectionSuspicion(tok *store.APIToken, source string) {
	if g.injSuspicion == nil {
		return
	}
	if source != inject.SourceTool && source != inject.SourceAssistant {
		return
	}
	g.injSuspicion.Mark(tok.ID)
}

// markInjectionSemanticSuspicion marks the session after a
// semantic-route hit. The route only ever reviews non-user
// content, so the source is non-user by construction.
func (g *Gateway) markInjectionSemanticSuspicion(tok *store.APIToken) {
	if g.injSuspicion == nil {
		return
	}
	g.injSuspicion.Mark(tok.ID)
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

// auditInjectionSemantic writes the decision-log event for a
// semantic-route check that alerted, observed, or would have
// blocked under DryRun. It carries the deciding route (sample /
// deep), the matched sample and similarity when the sample layer
// fired, the category and severity — never request text. Blocks
// are audited as violation / model_call_blocked by the caller
// instead, so one block produces exactly one alertable event.
func (g *Gateway) auditInjectionSemantic(user *store.User, tok *store.APIToken, model string, level store.DataLevel, dec inject.SemanticDecision) {
	detail := map[string]string{
		"source":   dec.Source,
		"rule":     "semantic",
		"category": dec.Category,
		"severity": dec.Severity,
	}
	if dec.SampleID != "" {
		detail["sample_id"] = dec.SampleID
		detail["similarity"] = fmt.Sprintf("%.2f", dec.Similarity)
	}
	g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "prompt_injection", Action: "prompt_injection_" + dec.Action, Model: model,
		DataLevel: level.String(), Decision: dec.Action, Detail: detail})
}

// injectionSemanticDetail is the one-line semantic-route outcome
// recorded on the model_call event. A skip (layer or route off,
// top-secret) or an empty non-user payload leaves no trace —
// there was nothing to review.
func injectionSemanticDetail(dec inject.SemanticDecision) string {
	if dec.Action == inject.DecisionSkip {
		return ""
	}
	if dec.Reason == "no_content" {
		return ""
	}
	return dec.Source + ":" + dec.Action
}

// joinInjectionDetail appends a second route's outcome to the
// model_call detail line, so a call both routes looked at reads
// as "rule:alert+sample:alert" instead of the second route
// silently overwriting the first.
func joinInjectionDetail(existing, next string) string {
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	return existing + "+" + next
}
