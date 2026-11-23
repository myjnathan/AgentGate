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

// F29: deformed sensitive content must still be detected after
// normalization — split by spaces, zero-width characters, fullwidth
// forms or mixed case. The finding span covers the original text,
// separators included, so redaction removes the whole evasion.
func TestScanNormalizedWords(t *testing.T) {
	d := NewDetector([]string{"test_sensitive", "机密", "secret"}, []string{"XH-200"})
	cases := []struct {
		text string
		kind string
		want string // expected original span
	}{
		{"包含 test sensitive 内容", "banned_word", "test sensitive"},
		{"包含 test\u200bsensitive 内容", "banned_word", "test\u200bsensitive"},
		{"包含 ｔｅｓｔ_sensitive 内容", "banned_word", "ｔｅｓｔ_sensitive"},
		{"机 密文件请勿外传", "banned_word", "机 密"},
		{"机\u200b密文件请勿外传", "banned_word", "机\u200b密"},
		{"this is SeCrEt stuff", "banned_word", "SeCrEt"},
		{"s e c r e t stuff", "banned_word", "s e c r e t"},
		{"XH 200 的进度如何", "custom_word", "XH 200"},
		{"ＸＨ－２００ 的进度如何", "custom_word", "ＸＨ－２００"},
		{"X\u200bH-200 的进度如何", "custom_word", "X\u200bH-200"},
	}
	for _, c := range cases {
		findings := d.Scan(c.text)
		var got *Finding
		for i := range findings {
			if findings[i].Kind == c.kind && findings[i].Value == c.want {
				got = &findings[i]
				break
			}
		}
		if got == nil {
			t.Errorf("Scan(%q): missing %s finding with span %q (got %v)", c.text, c.kind, c.want, findings)
			continue
		}
		if c.text[got.Start:got.End] != c.want {
			t.Errorf("Scan(%q): span [%d,%d) = %q, want %q", c.text, got.Start, got.End, c.text[got.Start:got.End], c.want)
		}
	}
}

// F29: structured patterns deformed the same way must match too.
func TestScanNormalizedPatterns(t *testing.T) {
	d := NewDetector(nil, nil)
	cases := []struct {
		text string
		kind string
		want string
	}{
		{"电话 138 0000 0000 请联系", "cn_mobile", "138 0000 0000"},
		{"电话 138-0000-0000 请联系", "cn_mobile", "138-0000-0000"},
		{"电话１３８００００００００请联系", "cn_mobile", "１３８００００００００"},
		{"key 是 ｓｋ-abcdef0123456789abcd", "api_key", "ｓｋ-abcdef0123456789abcd"},
		{"邮箱 ａ＠ｂ．ｃｏｍ 请查收", "email", "ａ＠ｂ．ｃｏｍ"},
		{"密码：abc12345", "password_assignment", "密码：abc12345"},
		{"密码：ａｂｃ１２３４５", "password_assignment", "密码：ａｂｃ１２３４５"},
	}
	for _, c := range cases {
		findings := d.Scan(c.text)
		var got *Finding
		for i := range findings {
			if findings[i].Kind == c.kind && findings[i].Value == c.want {
				got = &findings[i]
				break
			}
		}
		if got == nil {
			t.Errorf("Scan(%q): missing %s finding with span %q (got %v)", c.text, c.kind, c.want, findings)
			continue
		}
		if c.text[got.Start:got.End] != c.want {
			t.Errorf("Scan(%q): span [%d,%d) = %q, want %q", c.text, got.Start, got.End, c.text[got.Start:got.End], c.want)
		}
	}
}

// F29 redline: normalization only adds findings. Text the original
// pass already catches must produce exactly the same findings —
// same spans, no duplicates from the normalized pass.
func TestScanNormalizedPreservesOriginal(t *testing.T) {
	d := NewDetector([]string{"test_sensitive"}, []string{"XH-200"})
	text := "call 13812345678 about XH-200 test_sensitive"
	findings := d.Scan(text)
	if len(findings) != 3 {
		t.Fatalf("expected exactly 3 findings, got %v", findings)
	}
	want := map[string][2]int{
		"cn_mobile":   {5, 16},
		"custom_word": {23, 29},
		"banned_word": {30, 44},
	}
	for _, f := range findings {
		span, ok := want[f.Kind]
		if !ok {
			t.Errorf("unexpected finding %v", f)
			continue
		}
		if f.Start != span[0] || f.End != span[1] {
			t.Errorf("%s span = [%d,%d), want [%d,%d)", f.Kind, f.Start, f.End, span[0], span[1])
		}
	}
	if lvl := Classify(findings); lvl != store.LevelSecret {
		t.Errorf("expected secret, got %v", lvl)
	}
	if got := d.Scan("nothing sensitive here"); len(got) != 0 {
		t.Errorf("clean text must stay clean, got %v", got)
	}
}

// F29: redaction of a normalized-only finding replaces the whole
// original span (separators included) and restores it exactly.
func TestRedactNormalizedRoundTrip(t *testing.T) {
	d := NewDetector(nil, nil)
	text := "联系我 138 0000 0000 或 １３８００００００００"
	findings := d.Scan(text)
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %v", findings)
	}
	red, mp := Redact(text, findings)
	if strings.Contains(red, "138") || strings.Contains(red, "１３８") {
		t.Fatalf("redaction leaked deformed phone: %s", red)
	}
	if strings.Count(red, "<PII_") != 2 {
		t.Fatalf("expected 2 placeholders, got %q", red)
	}
	if restored := Restore(red, mp); restored != text {
		t.Fatalf("restore mismatch:\n got %q\nwant %q", restored, text)
	}
}
