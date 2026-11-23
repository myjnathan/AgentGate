package gateway_test

import (
	"strings"
	"testing"
)

// F29: deformed sensitive content is still blocked / desensitized
// end to end — the gateway pipeline scans through the normalized
// views, so a banned word split by spaces or zero-width characters,
// a custom word split by a space, and a fullwidth phone number are
// all caught exactly like their plain forms.
func TestNormalizedBannedWordBlocked(t *testing.T) {
	e := setup(t)
	for _, content := range []string{
		"包含 test sensitive 内容",      // split by a space
		"包含 test\u200bsensitive 内容", // split by a zero-width space
		"包含 ｔｅｓｔ_sensitive 内容",      // fullwidth letters
	} {
		code, body := e.chat(t, e.byoaTok, "mock-eco", content)
		if code != 400 || !strings.Contains(body, "request_rejected") {
			t.Fatalf("expected 400 request_rejected for %q, got %d %s", content, code, body)
		}
	}
}

func TestNormalizedCustomWordBlockedForBYOA(t *testing.T) {
	e := setup(t)
	// "XH-200" split by a space still classifies as confidential, so
	// a BYOA token is refused exactly like for the plain word.
	code, body := e.chat(t, e.byoaTok, "mock-pro", "请分析 XH 200 项目的进展")
	if code != 403 || !strings.Contains(body, "data_level_forbidden") {
		t.Fatalf("expected 403 data_level_forbidden, got %d %s", code, body)
	}
}

func TestNormalizedPhoneRedactedAndRestored(t *testing.T) {
	e := setup(t)
	// Fullwidth phone number: desensitized before forwarding (the
	// mock echoes the redacted prompt) and restored in the response.
	code, body := e.chat(t, e.mgdTok, "mock-eco", "我的手机号是１３８１２３４５６７８，帮我记下来")
	if code != 200 {
		t.Fatalf("expected 200, got %d %s", code, body)
	}
	if !strings.Contains(body, "１３８１２３４５６７８") {
		t.Fatalf("expected restored fullwidth phone in response, got %s", body)
	}
	if strings.Contains(body, "<PII_") {
		t.Fatalf("placeholder leaked to client: %s", body)
	}
}

// F29 redline, end to end: the audit trail for a normalized-only hit
// records the finding kind, never the deformed (or original) text.
func TestNormalizedHitAuditedWithoutText(t *testing.T) {
	e := setup(t)
	if code, _ := e.chat(t, e.byoaTok, "mock-eco", "包含 test sensitive 内容"); code != 400 {
		t.Fatalf("expected 400, got %d", code)
	}
	found := false
	for _, ev := range e.al.All() {
		if ev.Kind == "violation" && ev.Action == "model_call_blocked" {
			found = true
			for k, v := range ev.Detail {
				if strings.Contains(v, "test") || strings.Contains(v, "sensitive") {
					t.Fatalf("audit detail %q contains matched text: %q", k, v)
				}
			}
			if ev.Detail["findings"] != "banned:banned_word" {
				t.Fatalf("findings detail = %q, want %q", ev.Detail["findings"], "banned:banned_word")
			}
		}
	}
	if !found {
		t.Fatal("no violation audit event for the blocked call")
	}
}
