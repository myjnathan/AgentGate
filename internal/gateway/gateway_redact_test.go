package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/seed"
)

// TestRedactBodyToolCalls verifies F10: tool_calls arguments are
// redacted exactly like message content, share placeholders with
// content for the same value, and survive the re-marshal intact.
func TestRedactBodyToolCalls(t *testing.T) {
	det := guard.NewDetector(seed.DemoBannedWords, seed.DemoCustomWords)
	body, _ := json.Marshal(map[string]any{
		"model": "mock-eco",
		"messages": []map[string]any{
			{"role": "user", "content": "帮我联系 13812345678"},
			{"role": "assistant", "content": nil, "tool_call_id": "", "tool_calls": []map[string]any{
				{"id": "call_1", "type": "function", "function": map[string]any{
					"name":      "send_sms",
					"arguments": `{"phone":"13812345678","text":"会议改到三点"}`,
				}},
			}},
		},
	})
	out, mapping := redactBody(body, det)
	if len(mapping) != 1 {
		t.Fatalf("expected 1 mapping entry (same phone in content and args), got %v", mapping)
	}
	if mapping["<PII_1>"] != "13812345678" {
		t.Fatalf("expected <PII_1> -> phone, got %v", mapping)
	}
	var req chatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(req.Messages))
	}
	if !strings.Contains(contentText(req.Messages[0].Content), "<PII_1>") {
		t.Fatalf("content not redacted: %s", req.Messages[0].Content)
	}
	tc := req.Messages[1].ToolCalls
	if len(tc) != 1 || tc[0].ID != "call_1" || tc[0].Function.Name != "send_sms" {
		t.Fatalf("tool call lost or mangled in re-marshal: %+v", tc)
	}
	if !strings.Contains(tc[0].Function.Arguments, "<PII_1>") {
		t.Fatalf("tool_calls arguments not redacted: %s", tc[0].Function.Arguments)
	}
	if strings.Contains(tc[0].Function.Arguments, "13812345678") {
		t.Fatalf("original phone leaked into forwarded arguments: %s", tc[0].Function.Arguments)
	}
	if !strings.Contains(tc[0].Function.Arguments, "会议改到三点") {
		t.Fatalf("unrelated argument text lost: %s", tc[0].Function.Arguments)
	}
}

// TestRedactBodySharedPlaceholdersNoCollision covers the case that
// motivated the shared Redactor: content holds phone+email, args hold
// only the email. Per-text numbering would map the email to <PII_1>
// in the args and <PII_2> in the content — a collision in the merged
// mapping. With one Redactor the email keeps one placeholder everywhere.
func TestRedactBodySharedPlaceholdersNoCollision(t *testing.T) {
	det := guard.NewDetector(seed.DemoBannedWords, seed.DemoCustomWords)
	body, _ := json.Marshal(map[string]any{
		"model": "mock-eco",
		"messages": []map[string]any{
			{"role": "user", "content": "手机 13812345678，邮箱 li@xinghe.example"},
			{"role": "assistant", "tool_calls": []map[string]any{
				{"id": "call_2", "type": "function", "function": map[string]any{
					"name":      "send_mail",
					"arguments": `{"to":"li@xinghe.example","subject":"报销"}`,
				}},
			}},
		},
	})
	out, mapping := redactBody(body, det)
	if len(mapping) != 2 {
		t.Fatalf("expected 2 mapping entries, got %v", mapping)
	}
	var req chatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	args := req.Messages[1].ToolCalls[0].Function.Arguments
	if !strings.Contains(args, "<PII_2>") {
		t.Fatalf("email in args must reuse the content placeholder <PII_2>, got args %s mapping %v", args, mapping)
	}
	if strings.Contains(args, "li@xinghe.example") {
		t.Fatalf("original email leaked into forwarded arguments: %s", args)
	}
	restored := guard.Restore(contentText(req.Messages[0].Content), mapping)
	if !strings.Contains(restored, "13812345678") || !strings.Contains(restored, "li@xinghe.example") {
		t.Fatalf("restore must recover both values, got %q", restored)
	}
}

// TestRedactBodyCanonicalPlaceholders verifies F32 end to end at the
// gateway layer: the same phone written with dashes in the content
// and without dashes in a tool_calls argument is one logical value,
// so it gets one placeholder in both places (the model can corefer),
// and the response restores both occurrences to the first-seen form.
func TestRedactBodyCanonicalPlaceholders(t *testing.T) {
	det := guard.NewDetector(seed.DemoBannedWords, seed.DemoCustomWords)
	body, _ := json.Marshal(map[string]any{
		"model": "mock-eco",
		"messages": []map[string]any{
			{"role": "user", "content": "主号是 138-0000-0000，帮我记下来"},
			{"role": "assistant", "tool_calls": []map[string]any{
				{"id": "call_4", "type": "function", "function": map[string]any{
					"name":      "send_sms",
					"arguments": `{"phone":"13800000000","text":"会议改到三点"}`,
				}},
			}},
		},
	})
	out, mapping := redactBody(body, det)
	if len(mapping) != 1 {
		t.Fatalf("expected 1 mapping entry (one phone, two surface forms), got %v", mapping)
	}
	if mapping["<PII_1>"] != "138-0000-0000" {
		t.Fatalf("mapping must keep the first-seen form, got %v", mapping)
	}
	var req chatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contentText(req.Messages[0].Content), "<PII_1>") {
		t.Fatalf("content not redacted to the shared placeholder: %s", req.Messages[0].Content)
	}
	args := req.Messages[1].ToolCalls[0].Function.Arguments
	if !strings.Contains(args, "<PII_1>") {
		t.Fatalf("args must reuse the content placeholder <PII_1>, got %s", args)
	}
	if strings.Contains(string(out), "13800000000") || strings.Contains(string(out), "138-0000-0000") {
		t.Fatalf("original phone leaked into forwarded body: %s", out)
	}
	restored := guard.Restore(contentText(req.Messages[0].Content), mapping)
	if restored != "主号是 138-0000-0000，帮我记下来" {
		t.Fatalf("restore mismatch: %q", restored)
	}
	restoredArgs := guard.Restore(args, mapping)
	if !strings.Contains(restoredArgs, "138-0000-0000") {
		t.Fatalf("args restore must return the first-seen form, got %s", restoredArgs)
	}
}

// TestRedactBodyNoFindingsLeavesBodyUntouched ensures the fast path:
// no findings means the original bytes (including unknown message
// fields) are forwarded byte-for-byte.
func TestRedactBodyNoFindingsLeavesBodyUntouched(t *testing.T) {
	det := guard.NewDetector(seed.DemoBannedWords, seed.DemoCustomWords)
	body, _ := json.Marshal(map[string]any{
		"model": "mock-eco",
		"messages": []map[string]any{
			{"role": "user", "content": "你好"},
			{"role": "assistant", "tool_calls": []map[string]any{
				{"id": "call_3", "type": "function", "function": map[string]any{
					"name":      "get_time",
					"arguments": `{}`,
				}},
			}},
		},
	})
	out, mapping := redactBody(body, det)
	if len(mapping) != 0 {
		t.Fatalf("expected empty mapping, got %v", mapping)
	}
	if string(out) != string(body) {
		t.Fatalf("body without findings must pass through unchanged")
	}
}
