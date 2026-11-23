package autorouter_test

import (
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/autorouter"
	"github.com/myjnathan/agentgate/internal/store"
)

func defaultCfg() *store.AutoRouteConfig { return autorouter.DefaultConfig() }

// classify is the test helper: sample library from the default
// config, one user message, no tool calls.
func classify(text string) autorouter.Decision {
	return autorouter.NewClassifier(defaultCfg().Samples).Classify(text, 1, false, defaultCfg())
}

func TestClassifySeedSamples(t *testing.T) {
	cfg := defaultCfg()
	for _, s := range cfg.Samples {
		dec := autorouter.NewClassifier(cfg.Samples).Classify(s.Text, 1, false, cfg)
		if dec.Tier != s.Tier {
			t.Errorf("seed %q: got tier %s, want %s (source %s conf %.2f)",
				s.ID, dec.Tier, s.Tier, dec.Source, dec.Confidence)
		}
		if dec.Source != autorouter.SourceSample {
			t.Errorf("seed %q: got source %s, want sample", s.ID, dec.Source)
		}
		if dec.SampleID != s.ID {
			t.Errorf("seed %q: got sample %s, want itself", s.ID, dec.SampleID)
		}
	}
}

func TestClassifyParaphrases(t *testing.T) {
	cases := []struct {
		text string
		tier string
	}{
		// Simple paraphrases: same everyday intent, different words.
		{"今天是星期几？", autorouter.TierSimple},
		{"帮我写一句新年祝福", autorouter.TierSimple},
		{"星河精密的考勤是几点开始", autorouter.TierSimple},
		{"食堂今晚吃什么", autorouter.TierSimple},
		// Complex paraphrases: same hard intent, reworded.
		{"分析一下上季度供应商报价单的成本异常，给三条降本建议", autorouter.TierComplex},
		{"帮我写个 Go 程序并发抓 100 个链接，限流 10 次失败重试 3 次", autorouter.TierComplex},
		{"对比两份采购合同的付款条件，指出风险并给谈判建议", autorouter.TierComplex},
		{"按过去三年销售数据预测下季度各产品线收入，并讲清楚依据", autorouter.TierComplex},
	}
	for _, tc := range cases {
		dec := classify(tc.text)
		if dec.Tier != tc.tier {
			t.Errorf("%q: got tier %s (source %s conf %.2f), want %s",
				tc.text, dec.Tier, dec.Source, dec.Confidence, tc.tier)
		}
		if dec.Source != autorouter.SourceSample {
			t.Errorf("%q: got source %s, want sample", tc.text, dec.Source)
		}
	}
}

func TestClassifyOrdinaryPromptsFallToRules(t *testing.T) {
	// Ordinary business prompts share a noun phrase with a sample
	// but must not match either side strongly: they fall through
	// to the rule fallback and land simple (no length / rounds /
	// tool-call signal). A heavily abbreviated paraphrase
	// ("星河精密几点上班" drops half the seed sample) also falls
	// through — and still lands on the right tier, which is the
	// point of the rule fallback as a safety net.
	for _, text := range []string{
		"帮我写请假条",
		"本周的供应商报价单汇总",
		"提醒我明天下午三点开会",
		"把这份会议纪要发给市场部",
		"星河精密几点上班",
	} {
		dec := classify(text)
		if dec.Source != autorouter.SourceRule {
			t.Errorf("%q: got source %s, want rule (conf %.2f sample %s)",
				text, dec.Source, dec.Confidence, dec.SampleID)
		}
		if dec.Tier != autorouter.TierSimple {
			t.Errorf("%q: got tier %s, want simple", text, dec.Tier)
		}
	}
}

func TestClassifyRuleFallback(t *testing.T) {
	cfg := defaultCfg()
	c := autorouter.NewClassifier(cfg.Samples)
	// Long text: a document-scale request is complex even with no
	// matching sample.
	long := strings.Repeat("这是一段关于日常工作的普通描述。", 150)
	dec := c.Classify(long, 1, false, cfg)
	if dec.Tier != autorouter.TierComplex || dec.Source != autorouter.SourceRule {
		t.Errorf("long text: got %s/%s, want complex/rule", dec.Tier, dec.Source)
	}
	if !strings.Contains(dec.Reason, "long_text") {
		t.Errorf("long text: reason %q does not name long_text", dec.Reason)
	}
	// Many rounds: multi-turn work is complex.
	dec = c.Classify("继续", 8, false, cfg)
	if dec.Tier != autorouter.TierComplex || !strings.Contains(dec.Reason, "many_rounds") {
		t.Errorf("many rounds: got %s/%s, want complex/many_rounds", dec.Tier, dec.Reason)
	}
	// Tool calls: agentic work is complex.
	dec = c.Classify("查一下", 1, true, cfg)
	if dec.Tier != autorouter.TierComplex || !strings.Contains(dec.Reason, "tool_calls") {
		t.Errorf("tool calls: got %s/%s, want complex/tool_calls", dec.Tier, dec.Reason)
	}
	// Short, single-turn, no tools: simple by default.
	dec = c.Classify("你好", 1, false, cfg)
	if dec.Tier != autorouter.TierSimple || dec.Source != autorouter.SourceRule {
		t.Errorf("greeting: got %s/%s, want simple/rule", dec.Tier, dec.Source)
	}
	// Rule confidence is deliberately middling: no sample evidence.
	if dec.Confidence != autorouter.RuleConfidence {
		t.Errorf("rule confidence: got %.2f, want %.2f", dec.Confidence, autorouter.RuleConfidence)
	}
}

func TestClassifyDisabled(t *testing.T) {
	cfg := defaultCfg()
	cfg.Enabled = false
	dec := autorouter.NewClassifier(cfg.Samples).Classify("分析供应商报价单并给出降本建议", 1, false, cfg)
	if dec.Source != autorouter.SourceDisabled {
		t.Errorf("disabled: got source %s, want disabled", dec.Source)
	}
}

func TestClassifyEmptyLibrary(t *testing.T) {
	// No samples at all: everything is decided by rules.
	cfg := defaultCfg()
	cfg.Samples = nil
	c := autorouter.NewClassifier(cfg.Samples)
	if dec := c.Classify("帮我写请假条", 1, false, cfg); dec.Tier != autorouter.TierSimple {
		t.Errorf("empty library simple prompt: got %s", dec.Tier)
	}
	if dec := c.Classify("分析三年销售数据并预测下季度收入", 1, false, cfg); dec.Tier != autorouter.TierSimple {
		// No sample to match and no rule signal: simple is the
		// honest answer — the rules only fire on length, rounds
		// or tool calls, not on vocabulary.
		t.Errorf("empty library complex prompt: got %s, want simple (rules only)", dec.Tier)
	}
}

func TestClassifyThresholdRespected(t *testing.T) {
	// A near-threshold request must flip tiers when the threshold
	// moves: the paraphrase "帮我写一句新年祝福" matches a simple
	// sample just above the default threshold.
	text := "帮我写一句新年祝福"
	dec := classify(text)
	if dec.Source != autorouter.SourceSample {
		t.Fatalf("%q: got source %s, want sample (test setup broken)", text, dec.Source)
	}
	cfg := defaultCfg()
	cfg.Threshold = 0.9
	dec = autorouter.NewClassifier(cfg.Samples).Classify(text, 1, false, cfg)
	if dec.Source != autorouter.SourceRule {
		t.Errorf("threshold 0.9: got source %s, want rule", dec.Source)
	}
}

func TestValidateSample(t *testing.T) {
	for _, tc := range []struct {
		tier, text string
		wantErr    bool
	}{
		{"simple", "今天星期几？", false},
		{"complex", "分析报价单", false},
		{"medium", "今天星期几？", true},
		{"simple", "你好", true}, // too short
		{"simple", strings.Repeat("长", 2001), true},
	} {
		err := autorouter.ValidateSample(tc.tier, tc.text)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateSample(%q, %q): err=%v, wantErr=%v", tc.tier, tc.text, err, tc.wantErr)
		}
	}
}

func TestValidateConfig(t *testing.T) {
	cfg := defaultCfg()
	if err := autorouter.ValidateConfig(cfg); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	if err := autorouter.ValidateConfig(nil); err != nil {
		t.Errorf("nil config: got %v, want nil", err)
	}
	bad := *cfg
	bad.Threshold = 1.5
	if err := autorouter.ValidateConfig(&bad); err == nil {
		t.Error("threshold 1.5: want error")
	}
	bad = *cfg
	bad.SimpleGroup = []string{""}
	if err := autorouter.ValidateConfig(&bad); err == nil {
		t.Error("empty model name: want error")
	}
	bad = *cfg
	bad.SimpleGroup = nil
	bad.ComplexGroup = nil
	if err := autorouter.ValidateConfig(&bad); err == nil {
		t.Error("both groups empty while enabled: want error")
	}
	bad = *cfg
	bad.Enabled = false
	bad.SimpleGroup = nil
	bad.ComplexGroup = nil
	if err := autorouter.ValidateConfig(&bad); err != nil {
		t.Errorf("disabled with empty groups: got %v, want nil", err)
	}
	bad = *cfg
	bad.Samples = append(append([]store.AutoRouteSample(nil), cfg.Samples...),
		store.AutoRouteSample{ID: "x", Tier: "medium", Text: "今天星期几？"})
	if err := autorouter.ValidateConfig(&bad); err == nil {
		t.Error("bad sample tier: want error")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := autorouter.DefaultConfig()
	if !cfg.Enabled || cfg.Threshold != autorouter.DefaultThreshold {
		t.Errorf("defaults: enabled=%v threshold=%v", cfg.Enabled, cfg.Threshold)
	}
	if len(cfg.SimpleGroup) != 1 || cfg.SimpleGroup[0] != "mock-eco" {
		t.Errorf("simple group: %v", cfg.SimpleGroup)
	}
	if len(cfg.ComplexGroup) != 1 || cfg.ComplexGroup[0] != "mock-pro" {
		t.Errorf("complex group: %v", cfg.ComplexGroup)
	}
	if len(cfg.Samples) != 12 {
		t.Errorf("seed samples: got %d, want 12", len(cfg.Samples))
	}
}
