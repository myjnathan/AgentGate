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
	"sync"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/billing"
	"github.com/myjnathan/agentgate/internal/cache"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/otel"
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
	// CacheTTL is how long an exact-match response stays cached (0 =
	// default 1h, see cache.DefaultTTL). The cache itself is
	// in-memory and resets on restart, like channel health (F1).
	CacheTTL time.Duration
	// Tracer is the OpenTelemetry exporter (F16); nil or disabled
	// means no tracing. Spans carry metadata only — never prompt or
	// response text (see docs/opentelemetry.md).
	Tracer *otel.Tracer
}

type Gateway struct {
	st      *store.Store
	auth    *auth.Service
	box     *secretbox.Box
	audit   *audit.Log
	policy  Policy
	client  *http.Client
	limiter *ratelimit.Limiter
	cache   *cache.Cache
	// keyMu guards keyNext; every method that also touches the store
	// takes keyMu first and the store lock second, so the lock order
	// is always keyMu -> store and can never invert.
	keyMu   sync.Mutex
	keyNext map[string]int // channel ID -> next key index to try
	// budgetMu guards budgetAlerted: one threshold warning per
	// department per month (F13), so a busy department does not spam
	// one alert per call once it crosses its threshold.
	budgetMu      sync.Mutex
	budgetAlerted map[string]string // department -> "2006-01" month warned
}

func New(st *store.Store, au *auth.Service, box *secretbox.Box, al *audit.Log, pol Policy) *Gateway {
	return &Gateway{st: st, auth: au, box: box, audit: al, policy: pol,
		client: &http.Client{Timeout: 10 * time.Minute}, limiter: ratelimit.New(),
		cache: cache.New(pol.CacheTTL, 0), keyNext: map[string]int{},
		budgetAlerted: map[string]string{}}
}

// Cache exposes the exact-match response cache (F11) so the server
// can wire it to the management API for status, toggling and
// clearing.
func (g *Gateway) Cache() *cache.Cache { return g.cache }

// SetTracer wires the OpenTelemetry exporter after construction
// (used by tests; the server passes it through Policy).
func (g *Gateway) SetTracer(tr *otel.Tracer) { g.policy.Tracer = tr }

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
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []chatMessage `json:"messages"`
	raw      map[string]json.RawMessage
}

// chatMessage is one OpenAI chat message. ToolCalls carries assistant
// tool invocations: their function arguments are request payload, not
// metadata, so they go through the same DLP/classification/redaction
// pipeline as content (F10).
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// scanText returns everything in the message the protection pipeline
// must see: content plus tool_calls function names and arguments.
func (m chatMessage) scanText() string {
	var b strings.Builder
	b.WriteString(contentText(m.Content))
	for _, tc := range m.ToolCalls {
		b.WriteString("\n")
		b.WriteString(tc.Function.Name)
		b.WriteString("\n")
		b.WriteString(tc.Function.Arguments)
	}
	return b.String()
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

// statusRecorder captures the HTTP status of a gateway response for
// the deferred span end, without changing what the client sees. It
// forwards Flush so SSE streaming keeps working (streamCopy asserts
// http.Flusher).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// traceState carries the per-call facts the deferred span end
// reports. Fields are filled as the handler learns them; the span
// itself is metadata only (model, channel, level, decision,
// tokens), never request or response text.
type traceState struct {
	decision string
	model    string
	channel  string
	level    string
	tokens   int64
	cache    string
	sidecar  string
}

// startTrace begins the gateway span for a call (a no-op span when
// the tracer is nil or disabled) and returns it with the state the
// deferred end reports. The caller's traceparent, if valid, is
// joined so one trace covers the agent, the gateway and the model.
func (g *Gateway) startTrace(r *http.Request, tok *store.APIToken, user *store.User) (*otel.Span, *traceState) {
	ts := &traceState{decision: "allow"}
	if g.policy.Tracer == nil {
		return nil, ts
	}
	parent, _ := otel.ParseTraceParent(r.Header.Get("traceparent"))
	span := g.policy.Tracer.Start("gateway.chat_completions", parent,
		otel.StringAttr("agentgate.actor_id", user.ID),
		otel.StringAttr("agentgate.agent_type", tok.AgentType),
		otel.StringAttr("agentgate.tier", string(tok.Tier)),
		otel.StringAttr("agentgate.department", user.Department))
	return span, ts
}

// endTrace finishes the span and records the call counters. The
// status comes from the recorder, the other facts from ts.
func (g *Gateway) endTrace(span *otel.Span, ts *traceState, rec *statusRecorder) {
	if span == nil {
		return
	}
	status := 1
	msg := ""
	if rec.status >= 400 {
		status = 2
		msg = http.StatusText(rec.status)
	}
	attrs := []otel.Attr{
		otel.StringAttr("agentgate.decision", ts.decision),
		otel.StringAttr("agentgate.model", ts.model),
		otel.StringAttr("agentgate.channel", ts.channel),
		otel.StringAttr("agentgate.data_level", ts.level),
		otel.IntAttr("agentgate.tokens_total", ts.tokens),
	}
	if ts.cache != "" {
		attrs = append(attrs, otel.StringAttr("agentgate.cache", ts.cache))
	}
	if ts.sidecar != "" {
		attrs = append(attrs, otel.StringAttr("agentgate.sidecar", ts.sidecar))
	}
	span.SetAttrs(attrs...)
	span.End(status, msg)
	if tr := g.policy.Tracer; tr != nil {
		tr.AddCounter("agentgate.gateway.calls", 1, map[string]string{"decision": ts.decision})
		if ts.tokens > 0 {
			tr.AddCounter("agentgate.gateway.tokens", ts.tokens, map[string]string{"channel": ts.channel})
		}
	}
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
// order is deterministic. F17: a channel is a candidate only if its
// trust type is compliant for the data level — secret data only ever
// reaches a local model, never a cloud of any kind.
// F24: ignoreTrust drops the trust filter — used only when a valid
// exception grant waives the trust rule for this one call.
func (g *Gateway) pickCandidates(model string, level store.DataLevel, ignoreTrust bool) []*store.Channel {
	now := time.Now()
	var out []*store.Channel
	g.st.Read(func(st *store.State) {
		for _, ch := range st.Channels {
			if ch.Disabled || ch.MaxDataLevel < level {
				continue
			}
			if !ignoreTrust && !trustAllowed(level, ch.TrustTypeOrDefault()) {
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
// sufficient data level exists but is currently cooling down. Only
// trust-compliant channels count, unless ignoreTrust is set (an
// exception grant waives the trust rule for this call): a cooling
// cloud is not "eligible" for secret data, so it must not turn a
// trust block into a 503.
func (g *Gateway) channelsCoolingDown(model string, level store.DataLevel, ignoreTrust bool) bool {
	cooling := false
	now := time.Now()
	g.st.Read(func(st *store.State) {
		for _, ch := range st.Channels {
			if ch.Disabled || ch.MaxDataLevel < level {
				continue
			}
			if !ignoreTrust && !trustAllowed(level, ch.TrustTypeOrDefault()) {
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

// channelsBlockedByTrust reports whether any enabled channel serves
// model at a sufficient data level but is ruled out by its trust type
// for that level (e.g. secret data and only cloud channels serve the
// model). It distinguishes "no compliant channel" from "no channel at
// all" so the gateway can block with an explicit reason. With
// ignoreTrust the trust rule is waived for this call, so nothing is
// trust-blocked by definition.
func (g *Gateway) channelsBlockedByTrust(model string, level store.DataLevel, ignoreTrust bool) bool {
	blocked := false
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
			if serves && !ignoreTrust && !trustAllowed(level, ch.TrustTypeOrDefault()) {
				blocked = true
			}
		}
	})
	return blocked
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
	// OpenTelemetry (F16): one server span per call, finished by a
	// deferred end that reads the recorder status and the trace
	// state filled in below. The recorder forwards Flush so SSE
	// streaming is unaffected.
	rec := &statusRecorder{ResponseWriter: w}
	w = rec
	span, ts := g.startTrace(r, tok, user)
	defer g.endTrace(span, ts, rec)
	// Per-token rate limit (F3): RPM counts the request now, TPM is
	// checked against tokens recorded from completed responses.
	// A denied request is audited but never reaches the pipeline or
	// an upstream channel.
	rpm, tpm := g.rateLimits(tok)
	if allowed, kind, retryAfter := g.limiter.Allow(tok.ID, rpm, tpm, time.Now()); !allowed {
		ts.decision = "block"
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
		ts.decision = "error"
		writeErr(w, http.StatusBadRequest, "bad_request", "cannot read body")
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		ts.decision = "error"
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid chat completion request")
		return
	}
	ts.model = req.Model
	// Quota check (monthly token cap on the token).
	if tok.MonthlyCap > 0 && tok.UsedTokens >= tok.MonthlyCap {
		ts.decision = "block"
		writeErr(w, http.StatusTooManyRequests, "quota_exceeded", "monthly token quota exhausted")
		return
	}

	// Department budget check (F13): a hard monthly USD cap per
	// department. Like LiteLLM's max_budget, the call already in
	// flight is never the one blocked — the check runs before the
	// pipeline, against spend so far this calendar month (UTC).
	if spent, cap, exceeded := g.departmentBudgetExceeded(user); exceeded {
		ts.decision = "block"
		g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "violation", Action: "model_call_blocked", Model: req.Model, Decision: "block",
			Detail: map[string]string{"reason": "department_budget_exceeded", "department": user.Department,
				"spent_usd":       strconv.FormatFloat(spent, 'f', 6, 64),
				"monthly_cap_usd": strconv.FormatFloat(cap, 'f', 6, 64)}})
		writeErr(w, http.StatusTooManyRequests, "department_budget_exceeded",
			fmt.Sprintf("department %q monthly budget of $%.2f exceeded (spent $%.2f)", user.Department, cap, spent))
		return
	}

	// ---- Protection pipeline ----
	det := g.detector()
	var allFindings []guard.Finding
	var fullText strings.Builder
	for _, m := range req.Messages {
		// Content and tool_calls arguments are both request payload:
		// a phone number or project code smuggled in a function
		// argument must classify and block exactly like one in
		// visible content (F10).
		t := m.scanText()
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
	// Endpoint sidecar signal (F15): a request that arrived through
	// ag-guard carries X-AgentGate-Sidecar. Record it on every audit
	// event for this call (violation and model_call both reuse this
	// detail map) so operators can see endpoint coverage in the
	// audit trail and in GET /api/sidecar/stats.
	sidecar := r.Header.Get("X-AgentGate-Sidecar")
	if sidecar != "" {
		detail["sidecar"] = sidecar
		ts.sidecar = sidecar
	}

	// Banned words: hard block.
	if hasBanned && !g.policy.DryRun {
		ts.decision, ts.level = "block", level.String()
		g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
			Kind: "violation", Action: "model_call_blocked", Model: req.Model, DataLevel: level.String(), Decision: "block", Detail: detail})
		writeErr(w, http.StatusBadRequest, "request_rejected", "request blocked by protection policy (banned content)")
		return
	}

	// Exception grant (F24): validated after the banned-word block
	// on purpose — a grant never waives banned content. It is only
	// consulted (and only consumed) where the policy below would
	// otherwise block this call.
	grant, grantPresented := g.findGrant(r, user.ID, level)
	grantWaived := false

	// BYOA tier may never send confidential+ data anywhere.
	if tok.Tier == store.TierBYOA && level >= store.LevelConfidential && !g.policy.DryRun {
		if grant != nil {
			// A valid grant waives the tier block for this one call;
			// the call is still desensitized when the channel
			// demands it (below), and the grant is spent before
			// forwarding.
			grantWaived = true
		} else {
			if grantPresented {
				ts.decision, ts.level = "block", level.String()
				g.rejectInvalidGrant(w, user, tok, req.Model, level, detail)
				return
			}
			ts.decision, ts.level = "block", level.String()
			g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
				Kind: "violation", Action: "model_call_blocked", Model: req.Model, DataLevel: level.String(), Decision: "block",
				Detail: mergeDetail(detail, "reason", "byoa_tier_cannot_send_confidential")})
			writeErr(w, http.StatusForbidden, "data_level_forbidden",
				"self-provided agents may not send confidential data; use a managed agent or remove the sensitive content")
			return
		}
	}

	if !g.anyChannelForModel(req.Model) {
		ts.decision = "error"
		writeErr(w, http.StatusBadRequest, "model_not_available", "model is not available on this gateway")
		return
	}
	candidates := g.pickCandidates(req.Model, level, false)
	if len(candidates) == 0 && grant != nil {
		// No compliant channel for this level, but a valid grant
		// waives the trust rule for this one call: fall back to the
		// least-privileged channel that may receive the level at
		// all. Compliant routing is always preferred — the waiver
		// only fires when the policy would otherwise block.
		if repicked := g.pickCandidates(req.Model, level, true); len(repicked) > 0 {
			candidates = repicked
			grantWaived = true
		}
	}
	if len(candidates) == 0 {
		// Model exists but no channel is allowed to receive this data
		// level — or every eligible channel is cooling down.
		if grantPresented {
			// The caller presented a grant that does not cover this
			// call (unknown, expired, used, another employee's, or
			// for a lower level): fail closed with an explicit
			// reason instead of a misleading policy block.
			ts.decision, ts.level = "block", level.String()
			g.rejectInvalidGrant(w, user, tok, req.Model, level, detail)
			return
		}
		if g.channelsCoolingDown(req.Model, level, false) {
			writeErr(w, http.StatusServiceUnavailable, "channels_in_cooldown",
				"all eligible model channels are temporarily cooling down after repeated failures")
			return
		}
		// F17: channels exist that could serve this model at this level,
		// but none has a compliant trust type (e.g. secret data and no
		// local-model channel). Block with an explicit reason instead of
		// the generic no-channel error — the operator can see exactly
		// which kind of channel is missing.
		if g.channelsBlockedByTrust(req.Model, level, false) {
			ts.decision, ts.level = "block", level.String()
			g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
				Kind: "violation", Action: "model_call_blocked", Model: req.Model, DataLevel: level.String(), Decision: "block",
				Detail: mergeDetail(detail, "reason", "no_compliant_channel")})
			writeErr(w, http.StatusForbidden, "no_compliant_channel", noCompliantChannelMessage(level))
			return
		}
		ts.decision, ts.level = "block", level.String()
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
	// F17: a confidential request routed to a signed cloud is always
	// desensitized, even when the caller's tier is not configured to
	// redact — the trust type demands it. Dedicated-cloud and local-model
	// channels receive the original text.
	mustRedact := trustRequiresRedaction(level, candidates[0].TrustTypeOrDefault())
	var mapping map[string]string
	if len(allFindings) > 0 && (g.policy.RedactForTier[tok.Tier] || mustRedact) && !g.policy.DryRun {
		body, mapping = redactBody(body, det)
		if decision == "allow" {
			decision = "redact"
		}
		if mustRedact {
			// The decision stays "reroute" (the existing convention for
			// confidential calls); the desensitized hop itself is
			// recorded here so operators can see it in the audit log.
			detail["desensitized"] = "signed_cloud"
		}
	} else if len(allFindings) > 0 && decision == "allow" {
		decision = "warn"
	}
	if g.policy.DryRun && len(allFindings) > 0 {
		decision = "dry_run:" + decision
	}

	// ---- Exact-match response cache (F11) ----
	// Only clean, low-level, non-streaming calls are cacheable: the
	// request must carry no DLP findings at all (a finding means the
	// request text is sensitive even when this tier would not redact
	// it), the data level must be public/internal, and DryRun observes
	// without serving stored answers. Streaming is never cached —
	// the response is sent while it is being generated.
	// A hit returns the stored body verbatim and consumes no upstream
	// tokens: it is audited as Kind "cache_hit", not "model_call",
	// so usage stats (F2) keep measuring upstream usage only.
	cacheEligible := !req.Stream && !g.policy.DryRun && level <= store.LevelInternal &&
		len(allFindings) == 0 && g.cache.Enabled()
	if cacheEligible {
		key := cache.Key(candidates[0].ID, level.String(), body)
		if status, cachedBody, ok := g.cache.Get(key); ok {
			cacheDetail := map[string]string{"cache": "hit"}
			if sidecar != "" {
				cacheDetail["sidecar"] = sidecar
			}
			ts.decision, ts.channel, ts.level, ts.cache = decision, candidates[0].Name, level.String(), "hit"
			g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
				Kind: "cache_hit", Action: "chat_completions", Model: req.Model, Channel: candidates[0].Name,
				DataLevel: level.String(), Decision: decision, Detail: cacheDetail})
			w.Header().Set("X-AgentGate-Cache", "HIT")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(cachedBody)
			return
		}
	}

	// Exception grant (F24): spend it now that the gateway has
	// committed to forwarding the waived call. If another request
	// spent the same token first, block instead of double-spending;
	// a grant is never consumed for a call that needed no waiver.
	if grantWaived {
		if !g.consumeGrant(grant, user, tok, req.Model, level, detail) {
			ts.decision, ts.level = "block", level.String()
			g.rejectInvalidGrant(w, user, tok, req.Model, level, detail)
			return
		}
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
	// noEnabledKey tracks whether every candidate channel was skipped
	// because all of its keys are disabled — that deserves an explicit
	// error (add a key or re-enable one), not a generic 502.
	noEnabledKey := false
	for i, ch := range chain {
		// Try this channel's keys in rotation order (F12): a 401 means
		// this key is dead, not the channel — disable the key and retry
		// the same in-flight request with the next key. Other failures
		// keep the existing F1 semantics (channel-level failover):
		// 429/5xx is the channel or upstream being unwell, not one key.
		enabled := ch.EnabledKeyCount()
		if enabled == 0 {
			// Every key on this channel is disabled (upstream rejected
			// them, or an admin did): the channel cannot serve, but it
			// is not a transport failure — report it explicitly.
			noEnabledKey = true
			continue
		}
		tried := 0
		for tried < enabled {
			keyIdx, enc, ok := g.nextKey(ch)
			if !ok {
				noEnabledKey = true
				break
			}
			tried++
			apiKey, err := g.box.Decrypt(enc)
			if err != nil {
				continue // undecryptable key: try the next one
			}
			upURL := strings.TrimSuffix(ch.BaseURL, "/") + "/chat/completions"
			upReq, err := http.NewRequestWithContext(context.WithoutCancel(r.Context()), http.MethodPost, upURL, bytes.NewReader(body))
			if err != nil {
				writeErr(w, http.StatusBadGateway, "bad_gateway", "cannot build upstream request")
				return
			}
			upReq.Header.Set("Content-Type", "application/json")
			upReq.Header.Set("Authorization", "Bearer "+apiKey)
			// Trace context (F16): the upstream joins this call's
			// trace when tracing is on; a no-op span yields "".
			if tp := span.TraceParent(); tp != "" {
				upReq.Header.Set("traceparent", tp)
			}
			if req.Stream {
				upReq.Header.Set("Accept", "text/event-stream")
			}
			r2, err := g.client.Do(upReq)
			if err != nil {
				g.noteFailure(ch)
				if i+1 < len(chain) {
					g.auditFallback(user, tok, req.Model, level, ch, chain[i+1], "upstream_unavailable")
				}
				break
			}
			if r2.StatusCode == http.StatusUnauthorized {
				// The key is invalid or revoked: remove just this key
				// (F12) and retry the same request with the next one.
				// When it was the last enabled key, return the
				// upstream 401 itself — the caller sees the real
				// auth failure, not a rewritten 502.
				g.disableKey(ch, keyIdx, "upstream_401")
				if tried < enabled {
					r2.Body.Close()
					continue
				}
				resp = r2
				channel = ch
				break
			}
			if retryableStatus(r2.StatusCode) {
				g.noteFailure(ch)
				if i+1 < len(chain) {
					g.auditFallback(user, tok, req.Model, level, ch, chain[i+1], fmt.Sprintf("upstream_%d", r2.StatusCode))
				}
				r2.Body.Close()
				break
			}
			resp = r2
			channel = ch
			if i > 0 {
				fallbackFrom = chain[0].Name
			}
			g.recordSuccess(ch.ID)
			break
		}
		if resp != nil {
			break
		}
	}
	if resp == nil {
		if noEnabledKey {
			writeErr(w, http.StatusBadGateway, "channel_no_enabled_key",
				"all API keys for the model channel are disabled (the upstream rejected them); add a new key or re-enable one in the console")
			return
		}
		writeErr(w, http.StatusBadGateway, "upstream_unavailable", "upstream model service unavailable")
		return
	}
	defer resp.Body.Close()

	var used usage
	if req.Stream {
		// Streaming responses restore placeholders too (F10): the
		// mapping is the same one redactBody built for this request.
		used = g.streamCopy(w, resp, mapping)
	} else {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		used = parseUsage(respBody)
		if mapping != nil {
			respBody = []byte(guard.Restore(string(respBody), mapping))
		}
		if cacheEligible {
			// The response must be clean too: a model can emit PII
			// for a clean prompt, and sensitive content is never
			// cached. The key uses the channel that actually served
			// the response, so a fallback channel's entry is found
			// again when that channel becomes the first pick.
			// The header is only set for cache-eligible requests:
			// ineligible ones (sensitive, streaming, DryRun) never
			// consulted the cache, so they carry no HIT/MISS at all.
			if resp.StatusCode == http.StatusOK && len(det.Scan(string(respBody))) == 0 {
				g.cache.Put(cache.Key(channel.ID, level.String(), body), resp.StatusCode, respBody)
			}
			w.Header().Set("X-AgentGate-Cache", "MISS")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	}
	usedTokens := used.Total
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
	// Cost (F13): priced at call time from the serving channel's
	// per-model price, so the audit trail and the stats agree even
	// if the price changes later. An unpriced model records 0 and
	// is reported as unpriced in stats.
	var cost float64
	if channel != nil {
		cost = billing.Cost(used.Prompt, used.Completion, channel.Pricing[req.Model])
	}
	if fallbackFrom != "" {
		detail["fallback_from"] = fallbackFrom
	}
	ts.decision, ts.channel, ts.level, ts.tokens = decision, channel.Name, level.String(), usedTokens
	if v := rec.Header().Get("X-AgentGate-Cache"); v != "" {
		ts.cache = strings.ToLower(v)
	}
	g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
		Kind: "model_call", Action: "chat_completions", Model: req.Model, Channel: channel.Name,
		DataLevel: level.String(), Decision: decision, Detail: detail, Tokens: usedTokens, CostUSD: cost})
	// Budget threshold warning (F13): one per department per month.
	if cost > 0 {
		g.maybeAlertBudgetThreshold(user)
	}
}

func parseUsage(body []byte) usage {
	var wrap struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(body, &wrap)
	u := wrap.Usage
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return usage{Prompt: u.PromptTokens, Completion: u.CompletionTokens, Total: u.TotalTokens}
}

// deptOf resolves an audit actor ID to a department, for spend and
// stats aggregation (same resolution as internal/api/stats.go).
func (g *Gateway) deptOf(actorID string) string {
	var dept string
	g.st.Read(func(st *store.State) {
		if u, ok := st.Users[actorID]; ok {
			dept = u.Department
		}
	})
	return dept
}

// departmentBudgetExceeded reports whether the caller's department
// has spent its monthly budget. No budget configured (or no
// department on the user) means unlimited.
func (g *Gateway) departmentBudgetExceeded(user *store.User) (spent, cap float64, exceeded bool) {
	if user.Department == "" {
		return 0, 0, false
	}
	var budget *store.DepartmentBudget
	g.st.Read(func(st *store.State) {
		for _, b := range st.Budgets {
			if b.Department == user.Department {
				cp := *b
				budget = &cp
			}
		}
	})
	if budget == nil || budget.MonthlyCapUSD <= 0 {
		return 0, 0, false
	}
	start, end := billing.MonthWindow(time.Now())
	spent = billing.DepartmentSpend(g.audit.All(), g.deptOf, user.Department, start, end)
	return spent, budget.MonthlyCapUSD, spent >= budget.MonthlyCapUSD
}

// maybeAlertBudgetThreshold fires one warning per department per
// month when spend crosses the budget's alert threshold (F13,
// Portkey's alert-threshold convention). It runs after the call is
// audited, so the spend figure includes the call that crossed it.
func (g *Gateway) maybeAlertBudgetThreshold(user *store.User) {
	if user.Department == "" {
		return
	}
	var budget *store.DepartmentBudget
	g.st.Read(func(st *store.State) {
		for _, b := range st.Budgets {
			if b.Department == user.Department {
				cp := *b
				budget = &cp
			}
		}
	})
	if budget == nil || budget.AlertThreshold <= 0 || budget.MonthlyCapUSD <= 0 {
		return
	}
	start, end := billing.MonthWindow(time.Now())
	spent := billing.DepartmentSpend(g.audit.All(), g.deptOf, user.Department, start, end)
	if spent < budget.AlertThreshold*budget.MonthlyCapUSD {
		return
	}
	month := start.Format("2006-01")
	g.budgetMu.Lock()
	if g.budgetAlerted[user.Department] == month {
		g.budgetMu.Unlock()
		return
	}
	g.budgetAlerted[user.Department] = month
	g.budgetMu.Unlock()
	g.audit.Append(audit.Event{Actor: "system", Kind: "budget", Action: "budget_threshold",
		Detail: map[string]string{"department": user.Department,
			"spent_usd":       strconv.FormatFloat(spent, 'f', 6, 64),
			"monthly_cap_usd": strconv.FormatFloat(budget.MonthlyCapUSD, 'f', 6, 64),
			"threshold":       strconv.FormatFloat(budget.AlertThreshold, 'f', 2, 64)}})
}

func redactBody(body []byte, det *guard.Detector) ([]byte, map[string]string) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return body, nil
	}
	// One Redactor for the whole request: the same value in content
	// and in a tool_calls argument gets the same placeholder, so the
	// merged mapping never collides (F10).
	redactor := guard.NewRedactor()
	changed := false
	for i, m := range req.Messages {
		t := contentText(m.Content)
		if findings := det.Scan(t); len(findings) > 0 {
			red := redactor.Redact(t, findings)
			if red != t {
				changed = true
				rawMsg, _ := json.Marshal(red)
				req.Messages[i].Content = rawMsg
			}
		}
		// Tool_calls arguments are redacted in place, exactly like
		// content; the rest of the tool call (id, name) is untouched.
		for j, tc := range m.ToolCalls {
			args := tc.Function.Arguments
			if findings := det.Scan(args); len(findings) > 0 {
				red := redactor.Redact(args, findings)
				if red != args {
					changed = true
					req.Messages[i].ToolCalls[j].Function.Arguments = red
				}
			}
		}
	}
	mapping := redactor.Mapping()
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
