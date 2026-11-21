package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestStreamingRedactedPlaceholderRestored verifies F10 end to end:
// a managed streaming call whose prompt was desensitized must return
// the original value in the SSE stream, never a placeholder.
func TestStreamingRedactedPlaceholderRestored(t *testing.T) {
	e := setup(t)
	body, _ := json.Marshal(map[string]any{
		"model":    "mock-eco",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "我的手机号是13812345678，帮我记下来"}},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.mgdTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "PII_1") {
		t.Fatalf("stream leaked placeholder: %q", out)
	}
	if !strings.Contains(out, "13812345678") {
		t.Fatalf("stream must restore the original phone: %q", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("stream must end with data: [DONE], got tail %q", out[max(0, len(out)-80):])
	}
}

func toolCallMessages(phoneOrWord string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model": "mock-eco",
		"messages": []map[string]any{
			{"role": "user", "content": "帮我发个短信"},
			{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
				{"id": "call_1", "type": "function", "function": map[string]any{
					"name":      "send_sms",
					"arguments": `{"phone":"` + phoneOrWord + `","text":"会议改到三点"}`,
				}},
			}},
		},
	})
	return body
}

// TestStreamingToolCallArgsRestored verifies F10: a phone number inside
// tool_calls arguments is desensitized before forwarding and restored
// in the streamed reply (the mock echoes the arguments it received).
func TestStreamingToolCallArgsRestored(t *testing.T) {
	e := setup(t)
	body := toolCallMessages("13812345678")
	bodyMap := map[string]any{}
	_ = json.Unmarshal(body, &bodyMap)
	bodyMap["stream"] = true
	body, _ = json.Marshal(bodyMap)
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.mgdTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "PII_1") {
		t.Fatalf("stream leaked placeholder from tool_calls args: %q", out)
	}
	if !strings.Contains(out, "13812345678") {
		t.Fatalf("stream must restore the phone from tool_calls args: %q", out)
	}
}

// TestToolCallBannedWordBlocked verifies F10: a banned word smuggled
// in tool_calls arguments is blocked exactly like one in content.
func TestToolCallBannedWordBlocked(t *testing.T) {
	e := setup(t)
	body, _ := json.Marshal(map[string]any{
		"model": "mock-eco",
		"messages": []map[string]any{
			{"role": "user", "content": "帮我发个短信"},
			{"role": "assistant", "tool_calls": []map[string]any{
				{"id": "call_1", "type": "function", "function": map[string]any{
					"name":      "send_sms",
					"arguments": `{"text":"this contains test_sensitive word"}`,
				}},
			}},
		},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.mgdTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != 400 || !strings.Contains(buf.String(), "request_rejected") {
		t.Fatalf("expected 400 request_rejected for banned word in tool_calls, got %d %s", resp.StatusCode, buf.String())
	}
}

// TestToolCallConfidentialBlockedForBYOA verifies F10: confidential
// content in tool_calls arguments classifies the request, so a BYOA
// token is refused before anything reaches an upstream.
func TestToolCallConfidentialBlockedForBYOA(t *testing.T) {
	e := setup(t)
	body, _ := json.Marshal(map[string]any{
		"model": "mock-pro",
		"messages": []map[string]any{
			{"role": "user", "content": "帮我查项目"},
			{"role": "assistant", "tool_calls": []map[string]any{
				{"id": "call_1", "type": "function", "function": map[string]any{
					"name":      "project_status",
					"arguments": `{"project":"XH-200"}`,
				}},
			}},
		},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.byoaTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != 403 || !strings.Contains(buf.String(), "data_level_forbidden") {
		t.Fatalf("expected 403 data_level_forbidden for confidential tool_calls args, got %d %s", resp.StatusCode, buf.String())
	}
}

// TestToolCallConfidentialRedactedForManaged verifies F10: a managed
// token may send confidential tool_calls arguments, but only
// desensitized — the upstream receives the placeholder and the client
// gets the original back in the response.
func TestToolCallConfidentialRedactedForManaged(t *testing.T) {
	e := setup(t)
	body, _ := json.Marshal(map[string]any{
		"model": "mock-pro",
		"messages": []map[string]any{
			{"role": "user", "content": "帮我查项目"},
			{"role": "assistant", "tool_calls": []map[string]any{
				{"id": "call_1", "type": "function", "function": map[string]any{
					"name":      "project_status",
					"arguments": `{"project":"XH-200"}`,
				}},
			}},
		},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.mgdTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d %s", resp.StatusCode, buf.String())
	}
	if !strings.Contains(buf.String(), "XH-200") {
		t.Fatalf("expected restored XH-200 from tool_calls args, got %s", buf.String())
	}
	if strings.Contains(buf.String(), "CUSTOM_1") {
		t.Fatalf("response leaked placeholder: %s", buf.String())
	}
}

// TestStreamingCallPassesThroughSSE verifies that with stream=true the
// gateway proxies the upstream SSE stream intact: chunk data lines are
// delivered and the stream terminates with data: [DONE].
func TestStreamingCallPassesThroughSSE(t *testing.T) {
	e := setup(t)
	body, _ := json.Marshal(map[string]any{
		"model":    "mock-eco",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "你好，流式测试"}},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.byoaTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for streaming call, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected SSE content type, got %q", ct)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// The mock upstream emits two data chunks (content, then finish+usage)
	// followed by the terminal [DONE] marker; all must pass through.
	dataLines := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data: ") {
			dataLines++
		}
	}
	if dataLines < 3 {
		t.Fatalf("expected at least 3 data: lines (2 chunks + [DONE]), got %d in %q", dataLines, out)
	}
	if !strings.Contains(out, `"chat.completion.chunk"`) {
		t.Fatalf("stream should contain completion chunks, got %q", out)
	}
	if !strings.Contains(out, "我收到了你的消息") || !strings.Contains(out, "流式测试") {
		t.Fatalf("stream should carry the mock reply echoing the prompt, got %q", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("stream must end with data: [DONE], got tail %q", out[max(0, len(out)-80):])
	}
}
