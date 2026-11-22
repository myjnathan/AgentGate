package alert_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/alert"
	"github.com/myjnathan/agentgate/internal/audit"
)

func TestFromEventClassification(t *testing.T) {
	cases := []struct {
		name  string
		ev    audit.Event
		alert bool
		sev   string
	}{
		{"violation block alerts", audit.Event{Kind: "violation", Action: "model_call_blocked", Decision: "block"}, true, "critical"},
		{"mcp tool denial alerts", audit.Event{Kind: "violation", Action: "tool_call_denied", Decision: "block"}, true, "critical"},
		{"violation warn does not alert", audit.Event{Kind: "violation", Action: "model_call_blocked", Decision: "warn"}, false, ""},
		{"channel cooldown alerts", audit.Event{Kind: "channel", Action: "channel_cooldown"}, true, "warning"},
		{"channel key disabled alerts", audit.Event{Kind: "channel", Action: "channel_key_disabled"}, true, "warning"},
		{"channel fallback does not alert", audit.Event{Kind: "channel", Action: "channel_fallback"}, false, ""},
		{"budget threshold warns", audit.Event{Kind: "budget", Action: "budget_threshold"}, true, "warning"},
		{"attestation downgrade warns", audit.Event{Kind: "attestation", Action: "attest_downgrade"}, true, "warning"},
		{"attestation submit does not alert", audit.Event{Kind: "attestation", Action: "attest_submit", Decision: "warn"}, false, ""},
		{"account lockout alerts", audit.Event{Kind: "auth", Action: "account_locked", Decision: "block"}, true, "critical"},
		{"plain login failure does not alert", audit.Event{Kind: "auth", Action: "login_failed", Decision: "block"}, false, ""},
		{"rate limit does not alert", audit.Event{Kind: "rate_limit", Action: "rate_limited", Decision: "block"}, false, ""},
		{"model call does not alert", audit.Event{Kind: "model_call", Action: "chat_completions", Decision: "allow"}, false, ""},
		{"alert events never re-alert", audit.Event{Kind: "alert", Action: "alert_delivery_failed"}, false, ""},
		{"admin events do not alert", audit.Event{Kind: "admin", Action: "webhook_created"}, false, ""},
	}
	for _, c := range cases {
		a, ok := alert.FromEvent(c.ev)
		if ok != c.alert {
			t.Errorf("%s: alert=%v, want %v", c.name, ok, c.alert)
		}
		if ok && a.Severity != c.sev {
			t.Errorf("%s: severity=%q, want %q", c.name, a.Severity, c.sev)
		}
	}
}

func TestGenericPayloadHasNoSensitiveContent(t *testing.T) {
	a := alert.Alert{Severity: "critical", Title: "请求被防护策略拦截",
		Kind: "violation", Action: "model_call_blocked", Actor: "lixiaofeng",
		AgentType: "claude-code", Tier: "byoa", Model: "mock-eco",
		DataLevel: "confidential", Decision: "block",
		Detail: map[string]string{"findings": "banned:banned_word"},
		Seq:    7,
		Time:   time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)}
	body, err := alert.BuildPayload(alert.KindGeneric, a)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v["source"] != "agentgate" || v["severity"] != "critical" || v["event_seq"] != float64(7) {
		t.Fatalf("unexpected payload fields: %s", body)
	}
	if strings.Contains(string(body), "test_sensitive") {
		t.Fatalf("payload must not contain matched sensitive text: %s", body)
	}
	if _, ok := v["detail"]; !ok {
		t.Fatalf("detail missing: %s", body)
	}
}

func TestPlatformPayloadShapes(t *testing.T) {
	a := alert.Alert{Severity: "warning", Title: "模型渠道进入熔断冷却",
		Kind: "channel", Action: "channel_cooldown", Actor: "system",
		Channel: "ch_cloud", Detail: map[string]string{"consecutive_failures": "3"},
		Time: time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)}

	wecom, err := alert.BuildPayload(alert.KindWeCom, a)
	if err != nil {
		t.Fatal(err)
	}
	var w map[string]any
	_ = json.Unmarshal(wecom, &w)
	if w["msgtype"] != "markdown" {
		t.Fatalf("wecom msgtype: %s", wecom)
	}
	if !strings.Contains(string(wecom), "熔断冷却") {
		t.Fatalf("wecom content missing title: %s", wecom)
	}

	ding, err := alert.BuildPayload(alert.KindDingTalk, a)
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	_ = json.Unmarshal(ding, &d)
	md, _ := d["markdown"].(map[string]any)
	if d["msgtype"] != "markdown" || md["title"] == nil || md["text"] == nil {
		t.Fatalf("dingtalk shape: %s", ding)
	}

	fei, err := alert.BuildPayload(alert.KindFeishu, a)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	_ = json.Unmarshal(fei, &f)
	if f["msg_type"] != "text" {
		t.Fatalf("feishu shape: %s", fei)
	}
	if strings.Contains(string(fei), "**") {
		t.Fatalf("feishu text must be plain, got markdown: %s", fei)
	}

	if _, err := alert.BuildPayload("unknown-kind", a); err == nil {
		t.Fatal("unknown kind must fail")
	}
}

func TestSignQueryVector(t *testing.T) {
	// Independent Python vector (2026-11-21): secret "SECtestsecret",
	// ts 1700000000000 -> "7LVwF0dAF3/+MRRulbpE4y72Ogzykc6bS2nG4I99T4s=".
	got := alert.SignQuery("SECtestsecret", 1700000000000)
	if want := "7LVwF0dAF3/+MRRulbpE4y72Ogzykc6bS2nG4I99T4s="; got != want {
		t.Fatalf("SignQuery = %q, want %q", got, want)
	}
}

func TestSignedURLPreservesQuery(t *testing.T) {
	now := time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)
	u, err := alert.SignedURL("https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc123", "s3cr3t", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "key=abc123") || !strings.Contains(u, "timestamp=1795226400000") || !strings.Contains(u, "sign=") {
		t.Fatalf("signed URL lost parameters: %s", u)
	}
	if strings.Contains(u, "s3cr3t") {
		t.Fatalf("signed URL must not embed the secret: %s", u)
	}
}

func TestHeaderSignatureVector(t *testing.T) {
	// Independent Python vector (2026-11-21): secret "webhook-secret",
	// body {"source":"agentgate"} -> sha256:4bdcea28bcc27212c9edd7876db751cdbfbc627cf6cab01b974c0dd466fd7be1.
	got := alert.HeaderSignature("webhook-secret", []byte(`{"source":"agentgate"}`))
	want := "sha256=4bdcea28bcc27212c9edd7876db751cdbfbc627cf6cab01b974c0dd466fd7be1"
	if got != want {
		t.Fatalf("HeaderSignature = %q, want %q", got, want)
	}
}

func TestMarkdownTruncation(t *testing.T) {
	detail := map[string]string{}
	for i := 0; i < 40; i++ {
		detail["finding_"+string(rune('a'+i%26))+strings.Repeat("x", 8)] =
			strings.Repeat("敏感词类别很长很长很长很长很长很长", 4)
	}
	a := alert.Alert{Severity: "critical", Title: "请求被防护策略拦截",
		Kind: "violation", Action: "model_call_blocked", Detail: detail,
		Time: time.Now().UTC()}
	body, err := alert.BuildPayload(alert.KindWeCom, a)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Markdown struct {
			Content string `json:"content"`
		} `json:"markdown"`
	}
	_ = json.Unmarshal(body, &v)
	if len(v.Markdown.Content) > 4096 {
		t.Fatalf("wecom content %d bytes, exceeds 4096 limit", len(v.Markdown.Content))
	}
	if !strings.HasSuffix(v.Markdown.Content, "…") {
		t.Fatalf("expected truncation marker, got tail %q", v.Markdown.Content[len(v.Markdown.Content)-20:])
	}
}
