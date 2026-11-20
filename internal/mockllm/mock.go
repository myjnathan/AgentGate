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
	"time"
)

type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
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

func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mock/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []map[string]any{
			{"id": "mock-eco", "object": "model"},
			{"id": "mock-pro", "object": "model"},
		}})
	})
	mux.HandleFunc("/mock/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		last := ""
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				last = textOf(req.Messages[i].Content)
				break
			}
		}
		reply := "[mock-model] 我收到了你的消息：" + last
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
