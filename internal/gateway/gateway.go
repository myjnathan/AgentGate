// Package gateway is the OpenAI-compatible model gateway with the
// protection pipeline at its core:
//
//	auth -> quota -> banned-word filter -> DLP scan -> classification
//	-> data-level routing -> redaction (optional) -> forward -> audit
//
// Policy actions: allow | warn | redact | block | reroute.
// See docs/enterprise-protection-design.md §3 (layer 2).
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

// Policy configures the protection pipeline.
type Policy struct {
	BannedWords []string
	CustomWords []string // enterprise-sensitive words -> confidential
	// RedactForTier: tiers whose PII/credential findings are redacted
	// instead of blocked when the channel is allowed to receive the data.
	RedactForTier map[store.Tier]bool
	DryRun        bool // record decisions but never block/redact
}

type Gateway struct {
	st     *store.Store
	auth   *auth.Service
	box    *secretbox.Box
	audit  *audit.Log
	policy Policy
	client *http.Client
}

func New(st *store.Store, au *auth.Service, box *secretbox.Box, al *audit.Log, pol Policy) *Gateway {
	return &Gateway{st: st, auth: au, box: box, audit: al, policy: pol,
		client: &http.Client{Timeout: 10 * time.Minute}}
}

func (g *Gateway) detector() *guard.Detector {
	return guard.NewDetector(g.policy.BannedWords, g.policy.CustomWords)
}

type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	raw map[string]json.RawMessage
}

func contentText(raw json.RawMessage) string {
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

func (g *Gateway) authenticate(r *http.Request) (*store.APIToken, *store.User, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil, nil, false
	}
	return g.auth.ResolveToken(strings.TrimPrefix(h, "Bearer "))
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg, "type": "agentgate_error"},
	})
}

// Models lists models the caller's tier may use.
func (g *Gateway) Models(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := g.authenticate(r); !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
		return
	}
	var data []map[string]any
	g.st.Read(func(st *store.State) {
		seen := map[string]bool{}
		for _, ch := range st.Channels {
			if ch.Disabled {
				continue
			}
			for _, m := range ch.Models {
				if !seen[m] {
					seen[m] = true
					data = append(data, map[string]any{"id": m, "object": "model", "owned_by": "agentgate"})
				}
			}
		}
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// pickChannel selects an enabled channel serving model whose
// MaxDataLevel >= level. Prefers the least-privileged sufficient
// channel (lowest MaxDataLevel that still fits) — data goes to the
// most restricted destination that is allowed to receive it.
func (g *Gateway) pickChannel(model string, level store.DataLevel) (*store.Channel, string, bool) {
	var best *store.Channel
	var key string
	g.st.Read(func(st *store.State) {
		for _, ch := range st.Channels {
			if ch.Disabled || ch.MaxDataLevel < level {
				continue
			}
			has := false
			for _, m := range ch.Models {
				if m == model {
					has = true
				}
			}
			if !has {
				continue
			}
			if best == nil || ch.MaxDataLevel < best.MaxDataLevel {
				cp := *ch
				best = &cp
			}
		}
	})
	if best == nil {
		return nil, "", false
	}
	k, err := g.box.Decrypt(best.APIKeyEnc)
	if err != nil {
		return nil, "", false
	}
	key = k
	return best, key, true
}

// anyChannelForModel reports whether any enabled channel serves model.
func (g *Gateway) anyChannelForModel(model string) bool {
	found := false
	g.st.Read(func(st *store.State) {
		for _, ch := range st.Channels {
			if ch.Disabled {
				continue
			}
			for _, m := range ch.Models {
				if m == model {
					found = true
				}
			}
		}
	})
	return found
}

// ChatCompletions runs the protection pipeline and proxies the request.
func (g *Gateway) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	tok, user, ok := g.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "cannot read body")
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid chat completion request")
		return
	}
	// Quota check (monthly token cap on the token).
	if tok.MonthlyCap > 0 && tok.UsedTokens >= tok.MonthlyCap {
		writeErr(w, http.StatusTooManyRequests, "quota_exceeded", "monthly token quota exhausted")
		return
	}

	// ---- Protection pipeline ----
	det := g.detector()
	var allFindings []guard.Finding
	var fullText strings.Builder
	for _, m := range req.Messages {
		t := contentText(m.Content)
		fullText.WriteString(t)
		fullText.WriteByte('\n')
		allFindings = append(allFindings, det.Scan(t)...)
	}
	level := guard.Classify(allFindings)
	// Caller-declared level (e.g. set by the endpoint sidecar when the
	// content came from an encrypted file) can only raise, never lower.
	if hl := r.Header.Get("X-AgentGate-Data-Level"); hl != "" {
		if lv, ok := store.ParseDataLevel(hl); ok && lv > level {
			level = lv
		}
	}
	decision := "allow"
	kinds := map[string]bool{}
	for _, f := range allFindings {
		kinds[string(f.Category)+":"+f.Kind] = true
	}
	hasBanned := false
	for _, f := range allFindings {
		if f.Category == guard.CatBanned {
			hasBanned = true
		}
	}
	detail := map[string]string{}
	{
		var ks []string
		for k := range kinds {
			ks = append(ks, k)
		}
		detail["findings"] = strings.Join(ks, ",")
	}

	// Banned words: hard block.
	if hasBanned && !g.policy.DryRun {
		g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "violation", Action: "model_call_blocked", Model: req.Model, DataLevel: level.String(), Decision: "block", Detail: detail})
		writeErr(w, http.StatusBadRequest, "request_rejected", "request blocked by protection policy (banned content)")
		return
	}

	// BYOA tier may never send confidential+ data anywhere.
	if tok.Tier == store.TierBYOA && level >= store.LevelConfidential && !g.policy.DryRun {
		g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "violation", Action: "model_call_blocked", Model: req.Model, DataLevel: level.String(), Decision: "block",
			Detail: mergeDetail(detail, "reason", "byoa_tier_cannot_send_confidential")})
		writeErr(w, http.StatusForbidden, "data_level_forbidden",
			"self-provided agents may not send confidential data; use a managed agent or remove the sensitive content")
		return
	}

	if !g.anyChannelForModel(req.Model) {
		writeErr(w, http.StatusBadRequest, "model_not_available", "model is not available on this gateway")
		return
	}
	channel, apiKey, ok := g.pickChannel(req.Model, level)
	if !ok {
		// Model exists but no channel is allowed to receive this data level.
		g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "violation", Action: "model_call_blocked", Model: req.Model, DataLevel: level.String(), Decision: "block",
			Detail: mergeDetail(detail, "reason", "no_channel_for_data_level")})
		writeErr(w, http.StatusForbidden, "no_channel_for_data_level",
			fmt.Sprintf("no configured model channel may receive %s data", level.String()))
		return
	}
	// Note: pickChannel already chose the least-privileged channel that
	// may receive this data level; when the level forced a restricted
	// (private) channel for confidential+ data, mark the decision so
	// operators can see classification routing in the audit log.
	if level >= store.LevelConfidential {
		decision = "reroute"
	}

	// Redaction: for tiers configured to redact, replace sensitive spans
	// with placeholders before forwarding (mapping stays in the gateway;
	// response restore happens for non-streaming responses).
	var mapping map[string]string
	if len(allFindings) > 0 && g.policy.RedactForTier[tok.Tier] && !g.policy.DryRun {
		body, mapping = redactBody(body, det)
		if decision == "allow" {
			decision = "redact"
		}
	} else if len(allFindings) > 0 && decision == "allow" {
		decision = "warn"
	}
	if g.policy.DryRun && len(allFindings) > 0 {
		decision = "dry_run:" + decision
	}

	// ---- Forward ----
	upURL := strings.TrimSuffix(channel.BaseURL, "/") + "/chat/completions"
	upReq, err := http.NewRequestWithContext(context.WithoutCancel(r.Context()), http.MethodPost, upURL, bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "bad_gateway", "cannot build upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+apiKey)
	if req.Stream {
		upReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := g.client.Do(upReq)
	if err != nil {
		g.recordFailure(channel.ID)
		writeErr(w, http.StatusBadGateway, "upstream_unavailable", "upstream model service unavailable")
		return
	}
	defer resp.Body.Close()

	var usedTokens int64
	if req.Stream {
		usedTokens = g.streamCopy(w, resp)
	} else {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		usedTokens = parseUsage(respBody)
		if mapping != nil {
			respBody = []byte(guard.Restore(string(respBody), mapping))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	}
	if usedTokens > 0 {
		g.st.WithLock(func(st *store.State) {
			if t, ok := st.Tokens[tok.ID]; ok {
				t.UsedTokens += usedTokens
			}
		})
		_ = g.st.Save()
	}
	g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "model_call", Action: "chat_completions", Model: req.Model, Channel: channel.Name,
		DataLevel: level.String(), Decision: decision, Detail: detail, Tokens: usedTokens})
}

func (g *Gateway) streamCopy(w http.ResponseWriter, resp *http.Response) int64 {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	var tail []byte
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			tail = append(tail, chunk...)
			if len(tail) > 8192 {
				tail = tail[len(tail)-8192:]
			}
			_, _ = w.Write(chunk)
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	// Best-effort usage extraction from the final SSE chunk.
	if i := strings.LastIndex(string(tail), "\"usage\""); i >= 0 {
		var wrap struct {
			Usage struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		seg := string(tail)[i-1:]
		if j := strings.Index(seg, "}"); j >= 0 {
			_ = json.Unmarshal([]byte(seg[:j+2]), &wrap)
			return wrap.Usage.TotalTokens
		}
	}
	return 0
}

func parseUsage(body []byte) int64 {
	var wrap struct {
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &wrap)
	return wrap.Usage.TotalTokens
}

func redactBody(body []byte, det *guard.Detector) ([]byte, map[string]string) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return body, nil
	}
	mapping := map[string]string{}
	changed := false
	for i, m := range req.Messages {
		t := contentText(m.Content)
		findings := det.Scan(t)
		if len(findings) == 0 {
			continue
		}
		red, mp := guard.Redact(t, findings)
		for k, v := range mp {
			mapping[k] = v
		}
		if red != t {
			changed = true
			rawMsg, _ := json.Marshal(red)
			req.Messages[i].Content = rawMsg
		}
	}
	if !changed {
		return body, mapping
	}
	// Re-marshal via a generic map to preserve other fields.
	var generic map[string]json.RawMessage
	_ = json.Unmarshal(body, &generic)
	msgs, _ := json.Marshal(req.Messages)
	generic["messages"] = msgs
	out, err := json.Marshal(generic)
	if err != nil {
		return body, mapping
	}
	return out, mapping
}

func (g *Gateway) recordFailure(channelID string) {
	g.st.WithLock(func(st *store.State) {
		if ch, ok := st.Channels[channelID]; ok {
			ch.Failures++
		}
	})
}

func mergeDetail(d map[string]string, k, v string) map[string]string {
	out := map[string]string{}
	for kk, vv := range d {
		out[kk] = vv
	}
	out[k] = v
	return out
}
