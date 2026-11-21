// Package alert delivers security alerts to external webhooks
// (backlog F5): enterprise WeChat / DingTalk / Feishu group robots
// and generic JSON webhooks.
//
// Alerts are built only from audit events, which never contain request
// content or matched sensitive values — only categories (e.g.
// banned_word), actors, data levels and decisions — so an alert can
// never leak the plaintext that triggered it.
package alert

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
)

// Kinds of webhook target.
const (
	KindGeneric  = "generic"
	KindWeCom    = "wecom"
	KindDingTalk = "dingtalk"
	KindFeishu   = "feishu"
)

// ValidKind reports whether kind names a supported webhook type.
func ValidKind(kind string) bool {
	switch kind {
	case KindGeneric, KindWeCom, KindDingTalk, KindFeishu:
		return true
	}
	return false
}

// Alert is a desensitised security alert derived from one audit event.
type Alert struct {
	Severity  string            `json:"severity"` // critical | warning
	Title     string            `json:"title"`
	Kind      string            `json:"event_kind"`
	Action    string            `json:"action"`
	Actor     string            `json:"actor"`
	AgentType string            `json:"agent_type,omitempty"`
	Tier      string            `json:"tier,omitempty"`
	Model     string            `json:"model,omitempty"`
	Channel   string            `json:"channel,omitempty"`
	DataLevel string            `json:"data_level,omitempty"`
	Decision  string            `json:"decision,omitempty"`
	Detail    map[string]string `json:"detail,omitempty"`
	Seq       int64             `json:"event_seq"`
	Time      time.Time         `json:"time"`
}

// FromEvent converts an audit event into an alert. The second return
// value is false for events that must not alert:
//
//   - ordinary traffic (model_call allow/warn/redact, admin, auth logins)
//   - rate_limit: a soft control that fires at volume; alerting on it
//     would drown real security signals
//   - Kind "alert" itself: delivery reports must never re-trigger
//     delivery, or a failing webhook would alert about its own failure
//     forever
func FromEvent(ev audit.Event) (Alert, bool) {
	a := Alert{Kind: ev.Kind, Action: ev.Action, Actor: ev.Actor,
		AgentType: ev.AgentType, Tier: ev.Tier, Model: ev.Model,
		Channel: ev.Channel, DataLevel: ev.DataLevel, Decision: ev.Decision,
		Detail: ev.Detail, Seq: ev.Seq, Time: ev.Time}
	switch {
	case ev.Kind == "violation" && ev.Decision == "block":
		a.Severity = "critical"
		a.Title = "请求被防护策略拦截"
	case ev.Kind == "channel" && ev.Action == "channel_cooldown":
		a.Severity = "warning"
		a.Title = "模型渠道进入熔断冷却"
	case ev.Kind == "channel" && ev.Action == "channel_key_disabled":
		a.Severity = "warning"
		a.Title = "渠道密钥失效已自动摘除"
	case ev.Kind == "auth" && ev.Action == "account_locked":
		a.Severity = "critical"
		a.Title = "账号因多次登录失败被锁定"
	default:
		return Alert{}, false
	}
	return a, true
}

// TestAlert builds the alert the admin "send test" button delivers.
func TestAlert(actor string) Alert {
	return Alert{Severity: "warning", Title: "测试告警",
		Kind: "alert", Action: "alert_test", Actor: actor,
		Detail: map[string]string{"note": "AgentGate 告警通道测试"},
		Seq:    0,
		Time:   time.Now().UTC()}
}

// markdown renders the alert as a short markdown body shared by the
// WeCom and DingTalk formats. Findings are category names only
// (banned_word, cn_id_card, ...), never matched values.
func (a Alert) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "**[AgentGate 告警] %s**\n", a.Title)
	fmt.Fprintf(&b, "> 事件：%s / %s\n", a.Kind, a.Action)
	if a.Actor != "" {
		fmt.Fprintf(&b, "> 操作者：%s\n", a.Actor)
	}
	if a.AgentType != "" {
		fmt.Fprintf(&b, "> Agent：%s\n", a.AgentType)
	}
	if a.Tier != "" {
		fmt.Fprintf(&b, "> 等级：%s\n", a.Tier)
	}
	if a.Model != "" {
		fmt.Fprintf(&b, "> 模型：%s\n", a.Model)
	}
	if a.Channel != "" {
		fmt.Fprintf(&b, "> 渠道：%s\n", a.Channel)
	}
	if a.DataLevel != "" {
		fmt.Fprintf(&b, "> 数据密级：%s\n", a.DataLevel)
	}
	if a.Decision != "" {
		fmt.Fprintf(&b, "> 处置：%s\n", a.Decision)
	}
	if len(a.Detail) > 0 {
		keys := make([]string, 0, len(a.Detail))
		for k := range a.Detail {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "> %s：%s\n", k, a.Detail[k])
		}
	}
	fmt.Fprintf(&b, "> 时间：%s", a.Time.Format("2006-01-02 15:04:05 UTC"))
	return b.String()
}

// plainText renders the alert as plain lines for Feishu text messages.
func (a Alert) plainText() string {
	s := a.markdown()
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "> ", "")
	return s
}

// truncateBytes cuts s to at most n bytes at a UTF-8 rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !isUTF8Boundary(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

func isUTF8Boundary(s string) bool {
	if len(s) == 0 {
		return true
	}
	c := s[len(s)-1]
	// A trailing byte in 0x80..0xBF means we cut inside a rune.
	return c < 0x80 || c >= 0xC0
}

// BuildPayload renders the alert for a webhook kind. The returned
// body never contains request content: it is built from the alert
// fields only.
func BuildPayload(kind string, a Alert) ([]byte, error) {
	switch kind {
	case KindGeneric:
		body := map[string]any{
			"source": "agentgate", "severity": a.Severity, "title": a.Title,
			"event_kind": a.Kind, "action": a.Action, "actor": a.Actor,
			"agent_type": a.AgentType, "tier": a.Tier, "model": a.Model,
			"channel": a.Channel, "data_level": a.DataLevel,
			"decision": a.Decision, "detail": a.Detail,
			"event_seq": a.Seq, "time": a.Time.Format(time.RFC3339),
		}
		return json.Marshal(body)
	case KindWeCom:
		// Official doc: {"msgtype":"markdown","markdown":{"content":...}},
		// content at most 4096 bytes.
		return json.Marshal(map[string]any{"msgtype": "markdown",
			"markdown": map[string]any{"content": truncateBytes(a.markdown(), 4000)}})
	case KindDingTalk:
		// {"msgtype":"markdown","markdown":{"title":...,"text":...}}.
		return json.Marshal(map[string]any{"msgtype": "markdown",
			"markdown": map[string]any{"title": "[AgentGate 告警] " + a.Title,
				"text": truncateBytes(a.markdown(), 4000)}})
	case KindFeishu:
		// Custom bot text message.
		return json.Marshal(map[string]any{"msg_type": "text",
			"content": map[string]any{"text": truncateBytes(a.plainText(), 4000)}})
	}
	return nil, fmt.Errorf("unknown webhook kind %q", kind)
}

// SignQuery computes the shared WeCom / DingTalk / Feishu robot
// signature: sign = base64(HMAC-SHA256(secret, "<ts>\n<secret>")).
// The secret appears twice by design (HMAC key and signed string).
// Test vector (computed independently in Python, 2026-11-21):
// secret "SECtestsecret", ts 1700000000000 ->
// "7LVwF0dAF3/+MRRulbpE4y72Ogzykc6bS2nG4I99T4s=".
func SignQuery(secret string, tsMillis int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(tsMillis, 10) + "\n" + secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// SignedURL appends timestamp & sign query parameters for platform
// robots. Existing query parameters (e.g. WeCom ?key=..., DingTalk
// ?access_token=...) are preserved.
func SignedURL(rawURL, secret string, now time.Time) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	ts := now.UnixMilli()
	q := u.Query()
	q.Set("timestamp", strconv.FormatInt(ts, 10))
	q.Set("sign", SignQuery(secret, ts))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// HeaderSignature computes the generic-webhook HMAC header value:
// "sha256=<hex HMAC-SHA256(secret, body)>".
// Test vector (Python, 2026-11-21): secret "webhook-secret",
// body {"source":"agentgate"} ->
// "sha256=4bdcea28bcc27212c9edd7876db751cdbfbc627cf6cab01b974c0dd466fd7be1".
func HeaderSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
