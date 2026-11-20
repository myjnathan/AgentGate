package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

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
