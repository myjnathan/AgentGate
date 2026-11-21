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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/ratelimit"
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
	// FailThreshold is the number of consecutive retryable failures
	// after which a channel is cooled down (0 = default 3, mirroring
	// LiteLLM's allowed_fails).
	FailThreshold int
	// Cooldown is how long a cooled-down channel stays out of rotation
	// (0 = default 60s, mirroring LiteLLM's cooldown_time).
	Cooldown time.Duration
	// DefaultRPM/DefaultTPM are the per-minute limits applied to a
	// token without an explicit limit (0 = built-in tier default:
	// managed 60 RPM / 100k TPM, BYOA 20 RPM / 20k TPM — managed
	// agents are supervised, self-provided ones are held tighter).
	DefaultRPM map[store.Tier]int
	DefaultTPM map[store.Tier]int
}

type Gateway struct {
	st      *store.Store
	auth    *auth.Service
	box     *secretbox.Box
	audit   *audit.Log
	policy  Policy
	client  *http.Client
	limiter *ratelimit.Limiter
}

func New(st *store.Store, au *auth.Service, box *secretbox.Box, al *audit.Log, pol Policy) *Gateway {
	return &Gateway{st: st, auth: au, box: box, audit: al, policy: pol,
		client: &http.Client{Timeout: 10 * time.Minute}, limiter: ratelimit.New()}
}

// rateLimits resolves the effective RPM/TPM for a token: its own
// configured limit wins, otherwise the tier default.
func (g *Gateway) rateLimits(tok *store.APIToken) (rpm, tpm int) {
	rpm, tpm = tok.RPM, tok.TPM
	if rpm <= 0 {
		rpm = g.policy.DefaultRPM[tok.Tier]
		if rpm <= 0 {
			rpm = defaultRPM(tok.Tier)
		}
	}
	if tpm <= 0 {
		tpm = g.policy.DefaultTPM[tok.Tier]
		if tpm <= 0 {
			tpm = defaultTPM(tok.Tier)
		}
	}
	return rpm, tpm
}

func defaultRPM(tier store.Tier) int {
	if tier == store.TierManaged {
		return 60
	}
	return 20
}

func defaultTPM(tier store.Tier) int {
	if tier == store.TierManaged {
		return 100_000
	}
	return 20_000
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

// failThreshold returns the configured consecutive-failure threshold,
// defaulting to 3.
func (g *Gateway) failThreshold() int {
	if g.policy.FailThreshold > 0 {
		return g.policy.FailThreshold
	}
	return 3
}

// cooldown returns the configured cooldown duration, defaulting to 60s.
func (g *Gateway) cooldown() time.Duration {
	if g.policy.Cooldown > 0 {
		return g.policy.Cooldown
	}
	return 60 * time.Second
}

// pickCandidates lists enabled channels serving model at a sufficient
// data level, excluding channels currently in cooldown, ordered
// least-privileged first (lowest MaxDataLevel). Ties break by ID so the
// order is deterministic.
func (g *Gateway) pickCandidates(model string, level store.DataLevel) []*store.Channel {
	now := time.Now()
	var out []*store.Channel
	g.st.Read(func(st *store.State) {
		for _, ch := range st.Channels {
			if ch.Disabled || ch.MaxDataLevel < level {
				continue
			}
			if ch.Health(now) == "cooling_down" {
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
			cp := *ch
			out = append(out, &cp)
		}
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].MaxDataLevel != out[j].MaxDataLevel {
			return out[i].MaxDataLevel < out[j].MaxDataLevel
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// channelsCoolingDown reports whether any channel serving model at a
// sufficient data level exists but is currently cooling down.
func (g *Gateway) channelsCoolingDown(model string, level store.DataLevel) bool {
	cooling := false
	now := time.Now()
	g.st.Read(func(st *store.State) {
		for _, ch := range st.Channels {
			if ch.Disabled || ch.MaxDataLevel < level {
				continue
			}
			serves := false
			for _, m := range ch.Models {
				if m == model {
					serves = true
				}
			}
			if serves && ch.Health(now) == "cooling_down" {
				cooling = true
			}
		}
	})
	return cooling
}

// retryableStatus reports whether an upstream status is worth failing
// over for: 5xx and 429. Other 4xx (auth, not found, content policy)
// are the caller's fault and must not trigger a retry — mirroring
// LiteLLM's per-error-type retry classification.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
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
	// Per-token rate limit (F3): RPM counts the request now, TPM is
	// checked against tokens recorded from completed responses.
	// A denied request is audited but never reaches the pipeline or
	// an upstream channel.
	rpm, tpm := g.rateLimits(tok)
	if allowed, kind, retryAfter := g.limiter.Allow(tok.ID, rpm, tpm, time.Now()); !allowed {
		g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "rate_limit", Action: "rate_limited", Decision: "block",
			Detail: map[string]string{"limit": kind, "token_id": tok.ID}})
		secs := int(retryAfter.Round(time.Second).Seconds())
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeErr(w, http.StatusTooManyRequests, "rate_limit_exceeded",
			fmt.Sprintf("token %s limit exceeded, retry after %d seconds", kind, secs))
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
	candidates := g.pickCandidates(req.Model, level)
	if len(candidates) == 0 {
		// Model exists but no channel is allowed to receive this data
		// level — or every eligible channel is cooling down.
		if g.channelsCoolingDown(req.Model, level) {
			writeErr(w, http.StatusServiceUnavailable, "channels_in_cooldown",
				"all eligible model channels are temporarily cooling down after repeated failures")
			return
		}
		g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "violation", Action: "model_call_blocked", Model: req.Model, DataLevel: level.String(), Decision: "block",
			Detail: mergeDetail(detail, "reason", "no_channel_for_data_level")})
		writeErr(w, http.StatusForbidden, "no_channel_for_data_level",
			fmt.Sprintf("no configured model channel may receive %s data", level.String()))
		return
	}
	// Note: pickCandidates already chose the least-privileged channel
	// that may receive this data level; when the level forced a
	// restricted (private) channel for confidential+ data, mark the
	// decision so operators can see classification routing in the
	// audit log.
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

	// ---- Forward, with same-level failover ----
	// Failover may only move between channels whose MaxDataLevel is not
	// higher than the first pick's: data-level constraints beat
	// availability, so a request never "upgrades" to a more privileged
	// channel (or "downgrades" to a less privileged one) just because
	// the first pick failed. Since candidates are sorted least-
	// privileged first, the chain is the leading run of channels at the
	// first pick's level.
	firstLevel := candidates[0].MaxDataLevel
	var chain []*store.Channel
	for _, ch := range candidates {
		if ch.MaxDataLevel != firstLevel {
			break
		}
		chain = append(chain, ch)
	}
	var resp *http.Response
	var channel *store.Channel
	fallbackFrom := ""
	for i, ch := range chain {
		apiKey, err := g.box.Decrypt(ch.APIKeyEnc)
		if err != nil {
			continue // misconfigured channel key: never receives the request
		}
		upURL := strings.TrimSuffix(ch.BaseURL, "/") + "/chat/completions"
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
		r2, err := g.client.Do(upReq)
		if err != nil {
			g.noteFailure(ch)
			if i+1 < len(chain) {
				g.auditFallback(user, tok, req.Model, level, ch, chain[i+1], "upstream_unavailable")
			}
			continue
		}
		if retryableStatus(r2.StatusCode) {
			g.noteFailure(ch)
			if i+1 < len(chain) {
				g.auditFallback(user, tok, req.Model, level, ch, chain[i+1], fmt.Sprintf("upstream_%d", r2.StatusCode))
			}
			r2.Body.Close()
			continue
		}
		resp = r2
		channel = ch
		if i > 0 {
			fallbackFrom = chain[0].Name
		}
		g.recordSuccess(ch.ID)
		break
	}
	if resp == nil {
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
		// Tokens are recorded only after the response completes, so
		// requests already in flight are never blocked by tokens
		// they have not yet consumed (LiteLLM's enforcement model).
		g.limiter.RecordTokens(tok.ID, usedTokens, time.Now())
		g.st.WithLock(func(st *store.State) {
			if t, ok := st.Tokens[tok.ID]; ok {
				t.UsedTokens += usedTokens
			}
		})
		_ = g.st.Save()
	}
	if fallbackFrom != "" {
		detail["fallback_from"] = fallbackFrom
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

// recordFailure counts a retryable failure for a channel. When the
// consecutive count reaches the fail threshold and the channel is not
// already cooling down, it enters cooldown and the second return value
// is true.
func (g *Gateway) recordFailure(channelID string) (failures int, cooledDown bool) {
	g.st.WithLock(func(st *store.State) {
		if ch, ok := st.Channels[channelID]; ok {
			ch.Failures++
			ch.ConsecutiveFailures++
			failures = ch.ConsecutiveFailures
			if failures >= g.failThreshold() && time.Now().After(ch.CooldownUntil) {
				ch.CooldownUntil = time.Now().Add(g.cooldown())
				cooledDown = true
			}
		}
	})
	return failures, cooledDown
}

// recordSuccess resets a channel's consecutive-failure count, so a
// transient blip does not accumulate toward cooldown.
func (g *Gateway) recordSuccess(channelID string) {
	g.st.WithLock(func(st *store.State) {
		if ch, ok := st.Channels[channelID]; ok {
			ch.ConsecutiveFailures = 0
		}
	})
}

// noteFailure records a failure and audits the cooldown transition
// (no request content is logged).
func (g *Gateway) noteFailure(ch *store.Channel) {
	failures, cooledDown := g.recordFailure(ch.ID)
	if !cooledDown {
		return
	}
	g.audit.Append(audit.Event{Actor: "system", Kind: "channel", Action: "channel_cooldown",
		Channel: ch.Name, Decision: "cooldown",
		Detail: map[string]string{
			"channel_id":           ch.ID,
			"consecutive_failures": strconv.Itoa(failures),
			"cooldown_seconds":     strconv.Itoa(int(g.cooldown().Seconds())),
		}})
}

// auditFallback records a routing hop to the next same-level channel.
func (g *Gateway) auditFallback(user *store.User, tok *store.APIToken, model string, level store.DataLevel, from, to *store.Channel, reason string) {
	g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "channel", Action: "channel_fallback", Model: model, Channel: from.Name,
		DataLevel: level.String(), Decision: "reroute",
		Detail: map[string]string{"to_channel": to.Name, "reason": reason}})
}

func mergeDetail(d map[string]string, k, v string) map[string]string {
	out := map[string]string{}
	for kk, vv := range d {
		out[kk] = vv
	}
	out[k] = v
	return out
}
