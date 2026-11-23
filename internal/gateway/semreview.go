package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/semreview"
	"github.com/myjnathan/agentgate/internal/store"
)

// Semantic review (backlog F28) wiring: the gateway runs the review
// after the rule DLP and after redaction, before the cache lookup
// and before forwarding — so a blocked request never reaches a
// channel and never lands in the cache, and a granted exception is
// never spent on a call the review blocks.
//
// The review text is the desensitized request (red line ②): the
// gateway re-redacts the body into a review copy, so the deep layer
// never sees raw PII even when the forwarded body is not redacted
// (e.g. a BYOA internal call with findings, or DryRun mode where
// the pipeline observes without redacting).

// SemReviewStats returns the runtime counters for the layer.
func (g *Gateway) SemReviewStats() semreview.Stats {
	g.semMu.Lock()
	defer g.semMu.Unlock()
	return g.semStats
}

// semReviewConfig reads the stored policy, falling back to the
// built-in defaults when the admin never configured the layer.
func (g *Gateway) semReviewConfig() *store.SemanticReviewConfig {
	var cfg *store.SemanticReviewConfig
	g.st.Read(func(st *store.State) {
		if st.SemanticReview != nil {
			cp := *st.SemanticReview
			cfg = &cp
		}
	})
	if cfg == nil {
		return semreview.DefaultConfig()
	}
	return cfg
}

// semReviewDeep resolves the configured guard-model channel into a
// DeepFunc for the reviewer: the channel must exist, be enabled and
// not cooling down, its trust type must be compliant for the data
// level (red line ① — the review channel itself follows tiered
// routing), and one of its enabled keys is used for the call. Any
// failure here is a deep-layer error, so the reviewer applies the
// conservative fallback (confidential+ blocks, lower levels alert
// and allow).
func (g *Gateway) semReviewDeep(cfg *store.SemanticReviewConfig) semreview.DeepFunc {
	client := semreview.NewDeepClient(g.client)
	return func(ctx context.Context, text string, level store.DataLevel) (semreview.DeepVerdict, error) {
		var ch *store.Channel
		g.st.Read(func(st *store.State) {
			if c, ok := st.Channels[cfg.DeepChannelID]; ok {
				cp := *c
				ch = &cp
			}
		})
		if ch == nil {
			return semreview.DeepVerdict{}, errors.New("deep review channel not found")
		}
		if ch.Disabled {
			return semreview.DeepVerdict{}, errors.New("deep review channel disabled")
		}
		if ch.Health(time.Now()) == "cooling_down" {
			return semreview.DeepVerdict{}, errors.New("deep review channel cooling down")
		}
		if !trustAllowed(level, ch.TrustTypeOrDefault()) {
			return semreview.DeepVerdict{}, fmt.Errorf("deep review channel trust type %s not compliant for %s data",
				ch.TrustTypeOrDefault(), level.String())
		}
		keyIdx, enc, ok := g.nextKey(ch)
		if !ok {
			return semreview.DeepVerdict{}, errors.New("deep review channel has no enabled key")
		}
		apiKey, err := g.box.Decrypt(enc)
		if err != nil {
			return semreview.DeepVerdict{}, errors.New("deep review channel key undecryptable")
		}
		_ = keyIdx
		return client.Review(ctx, semreview.DeepEndpoint{
			BaseURL: ch.BaseURL, Model: cfg.DeepModel, APIKey: apiKey}, text)
	}
}

// runSemanticReview executes one review and records the runtime
// counters. It returns the decision; the caller decides what to do
// with it (block / alert-and-continue / allow). The deep call runs
// under the policy timeout.
func (g *Gateway) runSemanticReview(ctx context.Context, text string, level store.DataLevel) semreview.Decision {
	cfg := g.semReviewConfig()
	var deep semreview.DeepFunc
	timeout := cfg.TimeoutMS
	if timeout <= 0 {
		timeout = semreview.DefaultTimeoutMS
	}
	if cfg.DeepEnabled {
		deep = g.semReviewDeep(cfg)
	}
	dctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	dec := semreview.Reviewer{}.Review(dctx, text, level, cfg, deep)
	g.semMu.Lock()
	g.semStats.Reviewed++
	switch dec.Source {
	case semreview.SourceSample:
		if dec.Action == semreview.DecisionBlock {
			g.semStats.SampleBlocked++
		} else if dec.Action == semreview.DecisionAlert {
			g.semStats.SampleAlerted++
		}
	case semreview.SourceDeep:
		g.semStats.DeepCalls++
		if dec.Action == semreview.DecisionBlock {
			g.semStats.DeepBlocked++
		} else if dec.Action == semreview.DecisionAlert {
			g.semStats.DeepAlerted++
		}
	case semreview.SourceTimeout:
		g.semStats.DeepTimeouts++
	case semreview.SourceDeepError:
		g.semStats.DeepErrors++
	}
	g.semMu.Unlock()
	return dec
}

// reviewText extracts the scannable request text (content plus
// tool_calls arguments) from a request body, so the review sees
// exactly what the pipeline saw — after redaction. It falls back to
// the pre-redaction text when the body cannot be parsed (it was
// valid JSON a moment ago, so this is defensive only).
func reviewText(body []byte, fallback string) string {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return fallback
	}
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.scanText())
		b.WriteByte('\n')
	}
	return b.String()
}

// auditSemanticReview writes the decision-log event for a review
// that alerted (or would have blocked under DryRun). It carries the
// matched sample ID, category and similarity — never request text.
// Blocks are audited as violation/model_call_blocked by the caller
// instead, so one block produces exactly one alertable event.
func (g *Gateway) auditSemanticReview(user *store.User, tok *store.APIToken, model string, level store.DataLevel, dec semreview.Decision) {
	detail := map[string]string{
		"source": dec.Source,
		"reason": dec.Reason,
	}
	if dec.SampleID != "" {
		detail["sample_id"] = dec.SampleID
		detail["category"] = dec.Category
		detail["similarity"] = fmt.Sprintf("%.2f", dec.Similarity)
	}
	g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "semantic_review", Action: "semantic_review_" + dec.Action, Model: model,
		DataLevel: level.String(), Decision: dec.Action, Detail: detail})
}

// semanticReviewDetail is the one-line review outcome recorded on
// the model_call event, so every reviewed call is traceable in the
// audit trail even when the review allowed it.
func semanticReviewDetail(dec semreview.Decision) string {
	if dec.Source == semreview.SourceDisabled || dec.Source == semreview.SourceTopSecret {
		return ""
	}
	return dec.Source + ":" + dec.Action
}
