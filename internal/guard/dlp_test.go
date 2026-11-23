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
// original span (separators included).
//
// F32 changed the expectation: the two spans are surface forms of one
// phone number, so they share one placeholder. Restore therefore
// returns the first-seen form ("138 0000 0000") in both places —
// the value is identical, only its formatting is normalized.
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
	if len(mp) != 1 {
		t.Fatalf("same phone in two forms must share one mapping entry, got %v", mp)
	}
	if strings.Count(red, "<PII_1>") != 2 {
		t.Fatalf("expected one placeholder used twice, got %q", red)
	}
	if restored := Restore(red, mp); restored != "联系我 138 0000 0000 或 138 0000 0000" {
		t.Fatalf("restore must return the first-seen form twice:\n got %q", restored)
	}
}

// F32: one logical value, one placeholder — across surface forms.
// Same phone with/without separators, same email in different case,
// same bank card with/without spaces, same custom word with
// separators inserted: each pair shares a placeholder and mints a
// single mapping entry. Credentials stay exact-match: two keys that
// differ only in case are different secrets and must not merge.
func TestRedactorCanonicalIdentity(t *testing.T) {
	d := NewDetector(nil, []string{"XH-200"})
	cases := []struct {
		name  string
		text  string
		ph    string // the single placeholder both forms must share
		forms []string
	}{
		{"phone separators", "主号 138-0000-0000，备用 13800000000", "<PII_1>",
			[]string{"138-0000-0000", "13800000000"}},
		{"email case", "发给 Li@Xinghe.example 或 li@xinghe.example", "<PII_1>",
			[]string{"Li@Xinghe.example", "li@xinghe.example"}},
		{"bank card spaces", "卡号 6222 0000 0000 0000，另一张 6222000000000000", "<PII_1>",
			[]string{"6222 0000 0000 0000", "6222000000000000"}},
		{"custom word separators", "XH 200 与 XH-200 的进度", "<CUSTOM_1>",
			[]string{"XH 200", "XH-200"}},
		{"id card X case", "证件 11010119900307123X 与 11010119900307123x 无关", "<PII_1>",
			[]string{"11010119900307123X", "11010119900307123x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			findings := d.Scan(c.text)
			if len(findings) != 2 {
				t.Fatalf("expected 2 findings, got %v", findings)
			}
			red, mp := Redact(c.text, findings)
			if len(mp) != 1 {
				t.Fatalf("expected 1 mapping entry, got %v", mp)
			}
			if strings.Count(red, c.ph) != 2 {
				t.Fatalf("expected placeholder %s twice, got %q", c.ph, red)
			}
			for _, f := range c.forms {
				if strings.Contains(red, f) {
					t.Fatalf("redaction leaked %q: %s", f, red)
				}
			}
			// Restore returns the first-seen surface form in both
			// places and leaves no placeholder behind.
			restored := Restore(red, mp)
			if strings.Contains(restored, c.ph) {
				t.Fatalf("restore left a placeholder: %q", restored)
			}
			if got := strings.Count(restored, c.forms[0]); got != 2 {
				t.Fatalf("expected first-seen form %q twice after restore, got %q", c.forms[0], restored)
			}
		})
	}

	// Credentials are case-sensitive: two keys differing only in case
	// are different secrets and must mint different placeholders.
	t.Run("credentials stay case-sensitive", func(t *testing.T) {
		text := "key1 sk-abcdefghijklmnop key2 sk-ABCDEFGHIJKLMNOP"
		findings := d.Scan(text)
		if len(findings) != 2 {
			t.Fatalf("expected 2 findings, got %v", findings)
		}
		red, mp := Redact(text, findings)
		if len(mp) != 2 {
			t.Fatalf("different keys must not share a placeholder, got %v", mp)
		}
		if restored := Restore(red, mp); restored != text {
			t.Fatalf("credential restore must be exact:\n got %q\nwant %q", restored, text)
		}
	})

	// Numbering counts logical values, not occurrences: the second
	// distinct phone is _2 even though the first phone appeared twice.
	// (A letter between the two forms keeps them separate digit runs
	// in the F29 keyword view; two bare numbers glued by punctuation
	// alone merge into one run and are not detected — a known F29
	// edge, not a placeholder issue.)
	t.Run("numbering counts logical values", func(t *testing.T) {
		text := "138-0000-0000 与 13800000000，另 13911112222"
		red, mp := Redact(text, d.Scan(text))
		if len(mp) != 2 {
			t.Fatalf("expected 2 mapping entries, got %v", mp)
		}
		if strings.Count(red, "<PII_1>") != 2 || !strings.Contains(red, "<PII_2>") {
			t.Fatalf("expected <PII_1> twice and <PII_2> once, got %q", red)
		}
		if strings.Contains(red, "<PII_3>") {
			t.Fatalf("no third placeholder should be minted: %q", red)
		}
	})
}
