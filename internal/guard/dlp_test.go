package guard

import (
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/store"
)

func TestAhoCorasick(t *testing.T) {
	ac := NewAhoCorasick([]string{"XH-200", "秘密项目", "abc"})
	got := ac.Find("这是秘密项目 XH-200 的资料，abc")
	if len(got) != 3 {
		t.Fatalf("expected 3 matches, got %v", got)
	}
	if got := ac.Find("nothing here"); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", got)
	}
}

func TestScanDetectsBuiltins(t *testing.T) {
	d := NewDetector([]string{"test_sensitive"}, []string{"XH-200"})
	text := "我的手机是13812345678，身份证110101199003071234，key 是 sk-abcdef0123456789abcd，项目 XH-200 绝密 test_sensitive"
	findings := d.Scan(text)
	kinds := map[string]bool{}
	for _, f := range findings {
		kinds[f.Kind] = true
	}
	for _, want := range []string{"cn_mobile", "cn_id_card", "api_key", "custom_word", "banned_word"} {
		if !kinds[want] {
			t.Errorf("missing detection %s in %v", want, kinds)
		}
	}
	if lvl := Classify(findings); lvl != store.LevelSecret {
		t.Errorf("banned word should classify secret, got %v", lvl)
	}
}

func TestRedactRestoreRoundTrip(t *testing.T) {
	d := NewDetector(nil, nil)
	text := "联系我 13812345678 或发邮件到 zhangsan@example.com"
	findings := d.Scan(text)
	red, mp := Redact(text, findings)
	if strings.Contains(red, "13812345678") || strings.Contains(red, "zhangsan@example.com") {
		t.Fatalf("redaction leaked data: %s", red)
	}
	restored := Restore(red, mp)
	if restored != text {
		t.Fatalf("restore mismatch:\n got %q\nwant %q", restored, text)
	}
}

func TestClassifyLevels(t *testing.T) {
	d := NewDetector(nil, nil)
	if lvl := Classify(d.Scan("hello world")); lvl != store.LevelPublic {
		t.Errorf("clean text should be public, got %v", lvl)
	}
	if lvl := Classify(d.Scan("call me 13812345678")); lvl != store.LevelInternal {
		t.Errorf("mobile should be internal, got %v", lvl)
	}
}
