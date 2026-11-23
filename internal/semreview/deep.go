package semreview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Deep-layer verdicts, following Qwen3Guard's three safety labels
// (Qwen team, Apache-2.0): a guard model answers in plain text with
// a "Safety:" line and a "Categories:" line. Higress's qwen3guard
// plugin and 1Panel AI Gateway v1.6.0 both integrate the model this
// way — the model is served self-hosted (vLLM / SGLang) behind an
// OpenAI-compatible POST /v1/chat/completions endpoint, so the
// gateway needs no vendor-specific protocol.
const (
	VerdictSafe          = "safe"
	VerdictControversial = "controversial"
	VerdictUnsafe        = "unsafe"
)

// DeepVerdict is one guard-model answer.
type DeepVerdict struct {
	Verdict    string
	Categories []string
}

// DeepEndpoint is where a guard model is served: a channel's base
// URL plus the guard model name and one API key, resolved by the
// gateway (which also checks the channel's trust type first).
type DeepEndpoint struct {
	BaseURL string
	Model   string
	APIKey  string
}

// DeepClient asks a Qwen3Guard-style guard model for a verdict over
// OpenAI-compatible chat completions. It sends only the (already
// desensitized) text and reads only the verdict — the text is never
// logged, and a non-200 response is reported as a status only, so
// no upstream error body can leak through.
type DeepClient struct {
	http *http.Client
}

// NewDeepClient builds a client around hc (nil = a default client
// with a 30s ceiling; the caller's context carries the review
// timeout, so the ceiling is only a backstop).
func NewDeepClient(hc *http.Client) *DeepClient {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &DeepClient{http: hc}
}

// Review asks the guard model to judge text. The request is a plain
// chat completion with the text as the user message — Qwen3Guard-Gen
// needs no system prompt; it answers with "Safety: <label>" and
// "Categories: <...>". An empty verdict with a nil error means the
// model answered but no Safety line could be parsed.
func (c *DeepClient) Review(ctx context.Context, ep DeepEndpoint, text string) (DeepVerdict, error) {
	payload, _ := json.Marshal(map[string]any{
		"model":       ep.Model,
		"temperature": 0,
		"max_tokens":  256,
		"messages": []map[string]string{
			{"role": "user", "content": text},
		},
	})
	url := strings.TrimSuffix(ep.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return DeepVerdict{}, fmt.Errorf("build deep review request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if ep.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+ep.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return DeepVerdict{}, fmt.Errorf("deep review call failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return DeepVerdict{}, fmt.Errorf("deep review endpoint returned %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Choices) == 0 {
		return DeepVerdict{}, fmt.Errorf("deep review response not parseable")
	}
	return ParseGuardAnswer(out.Choices[0].Message.Content)
}

// ParseGuardAnswer reads a Qwen3Guard-style plain-text answer:
//
//	Safety: Unsafe
//	Categories: Jailbreak
//
// The label is case-insensitive and may carry trailing text. Only
// the first Safety line decides; Categories is informational. A
// missing or unknown label is an error (the caller treats it as a
// deep-layer failure, i.e. the conservative fallback).
func ParseGuardAnswer(answer string) (DeepVerdict, error) {
	var v DeepVerdict
	for _, line := range strings.Split(answer, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "safety:"):
			label := strings.TrimSpace(line[len("safety:"):])
			fields := strings.Fields(label)
			if len(fields) == 0 {
				return DeepVerdict{}, fmt.Errorf("empty safety label")
			}
			switch strings.ToLower(fields[0]) {
			case "safe":
				v.Verdict = VerdictSafe
			case "controversial":
				v.Verdict = VerdictControversial
			case "unsafe":
				v.Verdict = VerdictUnsafe
			default:
				return DeepVerdict{}, fmt.Errorf("unknown safety label %q", label)
			}
		case strings.HasPrefix(lower, "categories:"):
			rest := strings.TrimSpace(line[len("categories:"):])
			if rest != "" && !strings.EqualFold(rest, "none") {
				for _, c := range strings.Split(rest, ",") {
					if c = strings.TrimSpace(c); c != "" {
						v.Categories = append(v.Categories, c)
					}
				}
			}
		}
	}
	if v.Verdict == "" {
		return DeepVerdict{}, fmt.Errorf("no Safety line in guard answer")
	}
	return v, nil
}
