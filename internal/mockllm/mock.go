// Package mockllm is a built-in mock OpenAI-compatible upstream used
// for the PoC demo and end-to-end tests, so the full gateway loop can
// be exercised without a real model API key. It echoes the last user
// message and reports fake token usage. Never enable it for real
// traffic; it is mounted only under /mock and in demo mode.
package mockllm

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		ToolCalls []struct {
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
}

func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// failTwice counts requests carrying the [mock-fail-500-twice]
// marker, so the smoke test can exercise gateway retries end to
// end: the first two matching requests fail with 500, later ones
// succeed. Test hook only — no production path sends this marker,
// and the counter is process-lifetime (the smoke server starts
// fresh each run).
var (
	failTwiceMu sync.Mutex
	failTwice   int
)

func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mock/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []map[string]any{
			{"id": "mock-eco", "object": "model"},
			{"id": "mock-pro", "object": "model"},
		}})
	})
	// Demo REST endpoint (backlog F7): a fictional HR API for 星河精密
	// that the seeded REST tool "rest.leave_balance" adapts into an
	// MCP tool. No credential is required; data is fictional.
	mux.HandleFunc("/mock/demo/hr/leave-balance", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		emp := r.URL.Query().Get("employee_id")
		if emp == "" {
			http.Error(w, "employee_id required", http.StatusBadRequest)
			return
		}
		balance := 5
		switch emp {
		case "lixiaofeng":
			balance = 12
		case "chenjing":
			balance = 8
		}
		writeJSON(w, map[string]any{"employee_id": emp, "balance_days": balance, "unit": "days"})
	})
	mux.HandleFunc("/mock/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		last := ""
		var toolArgs []string
		for _, m := range req.Messages {
			if m.Role == "user" {
				last = textOf(m.Content)
			}
			for _, tc := range m.ToolCalls {
				toolArgs = append(toolArgs, tc.Function.Arguments)
			}
		}
		// Test hook (backlog F33): a request whose last user message
		// carries [mock-fail-500-twice] fails with 500 the first two
		// times, then succeeds — the gateway's same-key retry then
		// serves the third attempt.
		if strings.Contains(last, "[mock-fail-500-twice]") {
			failTwiceMu.Lock()
			failTwice++
			n := failTwice
			failTwiceMu.Unlock()
			if n <= 2 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"mock_transient_500"}}`))
				return
			}
		}
		// The reply echoes the last user message and any tool_calls
		// arguments, so gateway tests can observe what the upstream
		// actually received (redacted or not) after restore.
		reply := "[mock-model] 我收到了你的消息：" + last
		if len(toolArgs) > 0 {
			reply += " 工具调用参数：" + strings.Join(toolArgs, "；")
		}
		usage := map[string]any{
			"prompt_tokens":     len([]rune(last)) + 8,
			"completion_tokens": len([]rune(reply)),
			"total_tokens":      len([]rune(last)) + 8 + len([]rune(reply)),
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			chunk := map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": req.Model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": reply}, "finish_reason": nil}},
			}
			b, _ := json.Marshal(chunk)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			final := map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": req.Model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				"usage":   usage,
			}
			b, _ = json.Marshal(final)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\ndata: [DONE]\n\n"))
			if fl != nil {
				fl.Flush()
			}
			return
		}
		writeJSON(w, map[string]any{
			"id": "chatcmpl-mock", "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": reply}, "finish_reason": "stop"}},
			"usage":   usage,
		})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
