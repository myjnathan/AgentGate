// Package autorouter implements smart routing (backlog F30): the
// client sends model=auto and the gateway picks a concrete model
// from a simple (cheap) or a complex (strong) model group, based
// on how complex the request is.
//
// The design borrows 1Panel AI Gateway's smart routing (1Panel
// docs, "Smart Routing": model=auto triggers routing; the request
// is vectorized and matched against simple/complex samples; when
// samples do not clearly match, rules decide; every decision is
// written to a decision log). Two adaptations for AgentGate:
//
//   - The "embedding" is the same deterministic n-gram vectorizer
//     F28 uses (semreview.Vectorize / Cosine) — a real neural
//     embedding model would break the zero-dependency single-binary
//     positioning, and the sample library, thresholds and decision
//     log are the same either way. Vectorize is the replaceable
//     function: a real embedding backend can be plugged in later.
//     The classifier augments it with character bigrams: a short
//     everyday question ("今天星期几？") has only three trigrams,
//     so one inserted character destroys two of them and the
//     trigram-only match fails; bigrams survive single insertions.
//     The augmentation is local to this package; F28's vectorizer
//     is untouched.
//   - Complexity routing is orthogonal to tiered routing and can
//     never relax it: the classifier only chooses between model
//     groups; which channels may serve a group model is still
//     decided by the F1/F17 candidate rules (data level + trust
//     type + cooldown). A tier whose group has no compliant channel
//     falls back to the other tier; if neither tier has one, the
//     request fails with the same explicit errors as a direct
//     model request (no_compliant_channel / no_channel_for_data_level).
//
// Red lines (enforced by the gateway wiring, not by this package):
//   - Complexity routing only optimizes cost inside the compliant
//     channel set; it never changes the data-level determination
//     or the channel trust boundary.
//   - The classifier is local-only: sample matching and rules run
//     in-process, no text leaves the gateway for routing.
//   - The decision (tier, source, confidence, matched sample,
//     tier fallback) is written to the audit log on every auto call.
package autorouter

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/myjnathan/agentgate/internal/semreview"
	"github.com/myjnathan/agentgate/internal/store"
)

// Complexity tiers.
const (
	TierSimple  = "simple"
	TierComplex = "complex"
)

// Decision sources, for the audit decision log.
const (
	SourceSample   = "sample"   // a library sample matched
	SourceRule     = "rule"     // rules decided (no sample matched)
	SourceDisabled = "disabled" // layer switched off
)

// Decision is one routing verdict. SampleID is set when the sample
// layer fired; Reason names the rule(s) when it did not
// ("rule:long_text,tool_calls") or "sample_match". The request text
// is never stored here.
type Decision struct {
	Tier       string
	Source     string
	Confidence float64
	SampleID   string
	Reason     string
}

// Stats are the runtime counters for the layer (like the F11 cache
// and F28 review counters): process-lifetime only, never persisted.
type Stats struct {
	Auto         int64 `json:"auto"`          // auto requests resolved
	Simple       int64 `json:"simple"`        // resolved to the simple group
	Complex      int64 `json:"complex"`       // resolved to the complex group
	Sample       int64 `json:"sample"`        // decided by sample matching
	Rule         int64 `json:"rule"`          // decided by rule fallback
	TierFallback int64 `json:"tier_fallback"` // chosen tier had no channel
}

// DefaultThreshold is the sample similarity at which a request is
// treated as matching a complexity sample. Same value as F28's
// default: the vectorizer is shared, and the seed libraries were
// tuned against it (see the classifier tests): paraphrases of a
// seed sample score 0.39 and above, ordinary business prompts score
// below 0.32.
const DefaultThreshold = 0.35

// MinSampleRunes is the shortest sample text the library accepts.
// Complexity samples include short everyday questions ("今天星期几？",
// 5 runes), so the floor is lower than F28's risk-sample floor.
const MinSampleRunes = 4

// MaxSampleRunes caps one sample so the library stays a set of short
// example phrasings, not pasted documents.
const MaxSampleRunes = 2000

// Rule thresholds for the fallback (1Panel: "when samples do not
// clearly match, rules decide"). Each is a deliberately coarse
// signal: the rules are a tiebreaker, not a second classifier.
const (
	// LongTextRunes: a request this long is a document-scale task.
	LongTextRunes = 2000
	// ManyRounds: this many messages means multi-turn work with
	// accumulated context.
	ManyRounds = 6
	// RuleConfidence is the confidence reported for rule decisions:
	// there is no sample evidence, so it is deliberately middling.
	RuleConfidence = 0.5
)

// MaxGroupModels caps one model group: enough for a priority list,
// small enough to keep the resolution loop trivial.
const MaxGroupModels = 16

// ValidTier reports whether t is a known complexity tier.
func ValidTier(t string) bool { return t == TierSimple || t == TierComplex }

// otherTier returns the tier the router falls back to when the
// chosen tier's group has no compliant channel.
func otherTier(t string) string {
	if t == TierSimple {
		return TierComplex
	}
	return TierSimple
}

// ValidateSample checks a new library entry before it is stored.
func ValidateSample(tier, text string) error {
	if !ValidTier(tier) {
		return fmt.Errorf("unknown tier %q", tier)
	}
	n := len([]rune(strings.TrimSpace(text)))
	if n < MinSampleRunes {
		return fmt.Errorf("sample text too short (%d runes, minimum %d)", n, MinSampleRunes)
	}
	if n > MaxSampleRunes {
		return fmt.Errorf("sample text too long (%d runes, maximum %d)", n, MaxSampleRunes)
	}
	return nil
}

// ValidateConfig checks a policy update before it is stored. A nil
// config is valid (it means "never configured": built-in defaults).
func ValidateConfig(cfg *store.AutoRouteConfig) error {
	if cfg == nil {
		return nil
	}
	if cfg.Threshold < 0 || cfg.Threshold > 1 {
		return fmt.Errorf("threshold %v out of range [0, 1]", cfg.Threshold)
	}
	for _, g := range []struct {
		name   string
		models []string
	}{
		{"simple_group", cfg.SimpleGroup},
		{"complex_group", cfg.ComplexGroup},
	} {
		if len(g.models) > MaxGroupModels {
			return fmt.Errorf("%s has %d models, maximum %d", g.name, len(g.models), MaxGroupModels)
		}
		for _, m := range g.models {
			if strings.TrimSpace(m) == "" {
				return fmt.Errorf("%s contains an empty model name", g.name)
			}
		}
	}
	if cfg.Enabled && len(cfg.SimpleGroup) == 0 && len(cfg.ComplexGroup) == 0 {
		return fmt.Errorf("smart routing is enabled but both model groups are empty")
	}
	for _, s := range cfg.Samples {
		if err := ValidateSample(s.Tier, s.Text); err != nil {
			return fmt.Errorf("sample %q: %w", s.ID, err)
		}
	}
	return nil
}

// DefaultConfig returns the built-in policy used when the store has
// never been configured: enabled, conservative threshold, the demo
// mock models as the two groups (mock-eco is the cheap model,
// mock-pro the strong one), seed samples.
func DefaultConfig() *store.AutoRouteConfig {
	return &store.AutoRouteConfig{
		Enabled:      true,
		Threshold:    DefaultThreshold,
		SimpleGroup:  []string{"mock-eco"},
		ComplexGroup: []string{"mock-pro"},
		Samples:      DefaultSamples(),
	}
}

// DefaultSamples is the seed complexity-sample library. Simple
// samples are everyday questions; complex samples are analysis,
// multi-step, code or long-document work. Ordinary business prompts
// must not match either side strongly — they fall through to the
// rule fallback, which the classifier tests assert.
func DefaultSamples() []store.AutoRouteSample {
	now := time.Now().UTC()
	mk := func(id, tier, text string) store.AutoRouteSample {
		return store.AutoRouteSample{ID: id, Tier: tier, Text: text, CreatedAt: now}
	}
	return []store.AutoRouteSample{
		mk("ar_seed_simple_1", TierSimple, "今天星期几？"),
		mk("ar_seed_simple_2", TierSimple, "把“请假申请”翻译成英文"),
		mk("ar_seed_simple_3", TierSimple, "帮我把这句话改得更正式一点：明天上午十点开会"),
		mk("ar_seed_simple_4", TierSimple, "星河精密的考勤时间是几点"),
		mk("ar_seed_simple_5", TierSimple, "帮我写一句生日祝福"),
		mk("ar_seed_simple_6", TierSimple, "公司食堂今天中午吃什么"),
		mk("ar_seed_complex_1", TierComplex, "分析星河精密上季度的供应商报价单，找出成本异常并给出三条降本建议"),
		mk("ar_seed_complex_2", TierComplex, "写一个 Go 程序：并发抓取 100 个 URL，限流 10，失败重试 3 次"),
		mk("ar_seed_complex_3", TierComplex, "对比这两份采购合同的付款条款，列出风险点并给出谈判建议"),
		mk("ar_seed_complex_4", TierComplex, "根据过去三年的销售数据，预测下季度各产品线收入并说明依据"),
		mk("ar_seed_complex_5", TierComplex, "设计一个支持多租户的权限模型，覆盖部门隔离与审计留痕"),
		mk("ar_seed_complex_6", TierComplex, "这份四十页的招标文件里，有哪些隐藏条款可能影响交付？"),
	}
}

// ---- classifier ----

// Classifier holds the precomputed vectors of one sample library.
// It is built per request (the library is small) so a policy change
// takes effect on the next call without a restart.
type Classifier struct {
	samples []store.AutoRouteSample
	vectors []map[string]float64
}

// NewClassifier builds a classifier from a sample list. The samples
// are copied in order; ties break toward the earlier sample so
// results are deterministic.
func NewClassifier(samples []store.AutoRouteSample) *Classifier {
	c := &Classifier{samples: append([]store.AutoRouteSample(nil), samples...)}
	for _, s := range c.samples {
		c.vectors = append(c.vectors, vectorize(s.Text))
	}
	return c
}

// vectorize is the classifier's feature vector: F28's trigram +
// word-unigram vector (semreview.Vectorize) plus character bigrams.
// The bigram layer exists because short Chinese questions have too
// few trigrams: "今天星期几？" is three trigrams, and one inserted
// character ("今天是星期几？") destroys two of them, while its
// bigrams mostly survive. The normalization mirrors
// semreview.normRunes (lowercase, strip whitespace / punctuation /
// symbols) so the two layers see the same rune stream.
func vectorize(text string) map[string]float64 {
	v := semreview.Vectorize(text)
	rs := normRunes(text)
	for i := 0; i+2 <= len(rs); i++ {
		v["b:"+string(rs[i:i+2])]++
	}
	return v
}

// normRunes lowercases and strips whitespace, punctuation and
// symbols, leaving letters, digits and CJK characters — the same
// stream semreview.Vectorize works on.
func normRunes(text string) []rune {
	var out []rune
	for _, r := range strings.ToLower(text) {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Classify decides the tier for one request. text is the request
// text (content plus tool_calls arguments); rounds is the number of
// messages in the conversation; hasToolCalls reports whether any
// message carries tool calls. Sample matching runs first (same
// vectorizer and match semantics as F28, plus a character-bigram
// layer: cosine similarity, at least three shared n-gram features,
// containment branch for near-verbatim embedding); when no sample
// reaches the threshold, the rule fallback decides: long text,
// many rounds or tool calls mean complex, otherwise simple.
func (c *Classifier) Classify(text string, rounds int, hasToolCalls bool, cfg *store.AutoRouteConfig) Decision {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if !cfg.Enabled {
		return Decision{Tier: TierSimple, Source: SourceDisabled, Reason: "disabled"}
	}
	threshold := cfg.Threshold
	if threshold == 0 {
		threshold = DefaultThreshold
	}
	if sample, tier, sim := c.match(text); sample != nil && sim >= threshold {
		return Decision{Tier: tier, Source: SourceSample, Confidence: sim,
			SampleID: sample.ID, Reason: "sample_match"}
	}
	// Rule fallback: length / tool calls / history rounds.
	var rules []string
	if len([]rune(text)) >= LongTextRunes {
		rules = append(rules, "long_text")
	}
	if rounds >= ManyRounds {
		rules = append(rules, "many_rounds")
	}
	if hasToolCalls {
		rules = append(rules, "tool_calls")
	}
	if len(rules) > 0 {
		return Decision{Tier: TierComplex, Source: SourceRule, Confidence: RuleConfidence,
			Reason: "rule:" + strings.Join(rules, ",")}
	}
	return Decision{Tier: TierSimple, Source: SourceRule, Confidence: RuleConfidence,
		Reason: "rule:default_simple"}
}

// match returns the best-matching sample, its tier and similarity,
// or (nil, "", best) when the library is empty or nothing shares at
// least three n-gram features with the request. best is the highest
// similarity over the library even when nothing reaches the
// threshold, so callers can log the closest sample for tuning. The
// score is the cosine similarity, except when a request contains a
// sample almost verbatim (at least ten shared n-grams covering 80%
// of the smaller side's n-grams), which scores by containment
// instead — a long request must not dilute an embedded sample below
// the threshold. A match only counts when at least three n-gram
// features are shared: a one- or two-feature coincidence (e.g. a
// shared company name) is not a complexity signal.
func (c *Classifier) match(text string) (*store.AutoRouteSample, string, float64) {
	v := vectorize(text)
	var best *store.AutoRouteSample
	bestTier := ""
	bestSim := 0.0
	for i, sv := range c.vectors {
		shared := sharedNGrams(v, sv)
		if shared < 3 {
			continue
		}
		sim := semreview.Cosine(v, sv)
		if shared >= 10 {
			// Containment branch (same as F28): a long request
			// must not dilute an embedded sample below the
			// threshold.
			total := ngramCount(v)
			if n := ngramCount(sv); n < total {
				total = n
			}
			if total > 0 {
				if cov := float64(shared) / float64(total); cov >= 0.8 && cov > sim {
					sim = cov
				}
			}
		}
		// Ties break toward complex: an ambiguous request is safer
		// served by the strong model than starved by the cheap one.
		if sim > bestSim || (sim == bestSim && bestTier == TierSimple && c.samples[i].Tier == TierComplex) {
			bestSim = sim
			s := c.samples[i]
			best = &s
			bestTier = s.Tier
		}
	}
	return best, bestTier, bestSim
}

// ngramCount is the total mass of the n-gram features in a vector
// (character bigrams and trigrams; word unigrams excluded).
func ngramCount(v map[string]float64) int {
	n := 0
	for k, c := range v {
		if strings.HasPrefix(k, "c:") || strings.HasPrefix(k, "b:") {
			n += int(c)
		}
	}
	return n
}

// sharedNGrams counts the n-gram features two vectors share
// (weighted by the smaller count), ignoring word unigrams.
func sharedNGrams(a, b map[string]float64) int {
	n := 0
	for k, va := range a {
		if !strings.HasPrefix(k, "c:") && !strings.HasPrefix(k, "b:") {
			continue
		}
		if vb, ok := b[k]; ok {
			if va < vb {
				n += int(va)
			} else {
				n += int(vb)
			}
		}
	}
	return n
}
