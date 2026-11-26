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
	"github.com/myjnathan/agentgate/internal/autorouter"
	"github.com/myjnathan/agentgate/internal/billing"
	"github.com/myjnathan/agentgate/internal/cache"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/onboard"
	"github.com/myjnathan/agentgate/internal/otel"
	"github.com/myjnathan/agentgate/internal/pluginpol"
	"github.com/myjnathan/agentgate/internal/ratelimit"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/semreview"
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
	// LaunchPhase is the rollout phase (backlog F34: observe | warn |
	// block). When set it wins over DryRun: observe means DryRun,
	// warn/block mean enforced. Empty means derive from DryRun
	// (true -> observe, false -> block) for older callers.
	LaunchPhase string
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
	// MaxTransientRetries is the same-key retry budget for transient
	// failures (429/5xx/network error) per key (0 = default 2, so 3
	// attempts per key before the channel is abandoned — backlog
	// F33, Bifrost's failure-classified retries). Auth failures
	// (401/403) never consume this budget: they rotate to the next
	// key at once. Each fallback channel gets its own full budget.
	MaxTransientRetries int
	// RetryBackoff is the base wait before a transient retry
	// (0 = default 100ms); the wait doubles per retry, capped at 2s,
	// plus up to 50% jitter.
	RetryBackoff time.Duration
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
	// semMu guards semStats: the runtime counters for the semantic
	// review layer (F28), process-lifetime only, never persisted.
	semMu    sync.Mutex
	semStats semreview.Stats
	// autoMu guards autoStats: the runtime counters for smart
	// routing (F30), process-lifetime only, never persisted.
	autoMu    sync.Mutex
	autoStats autorouter.Stats
	// phaseMu guards phase: the rollout phase (backlog F34), changed
	// at runtime by the management API. The zero value ("") means
	// block — the pre-F34 behaviour.
	phaseMu sync.RWMutex
	phase   string
	// wordsMu guards bannedWords/customWords: the gateway's word
	// lists. They start as copies of Policy.BannedWords /
	// Policy.CustomWords and are replaced at runtime when an
	// industry template is applied (F34); the detector is rebuilt
	// per request (detector()), so a runtime change takes effect
	// on the next call without a restart.
	wordsMu     sync.RWMutex
	bannedWords []string
	customWords []string
	// redactMu guards redactOverride: the runtime redaction tiers
	// (F34 conservative preset redacts both tiers). Nil means the
	// Policy.RedactForTier map applies.
	redactMu       sync.RWMutex
	redactOverride map[store.Tier]bool
}

func New(st *store.Store, au *auth.Service, box *secretbox.Box, al *audit.Log, pol Policy) *Gateway {
	phase := pol.LaunchPhase
	if !onboard.ValidPhase(phase) {
		if pol.DryRun {
			phase = onboard.PhaseObserve
		} else {
			phase = onboard.PhaseBlock
		}
	}
	return &Gateway{st: st, auth: au, box: box, audit: al, policy: pol,
		client: &http.Client{Timeout: 10 * time.Minute}, limiter: ratelimit.New(),
		cache: cache.New(pol.CacheTTL, 0), keyNext: map[string]int{},
		budgetAlerted: map[string]string{}, phase: phase,
		bannedWords: append([]string(nil), pol.BannedWords...),
		customWords: append([]string(nil), pol.CustomWords...)}
}

// LaunchPhase returns the current rollout phase (F34): observe |
// warn | block. Unset means block (the pre-F34 behaviour).
func (g *Gateway) LaunchPhase() string {
	g.phaseMu.RLock()
	defer g.phaseMu.RUnlock()
	if g.phase == "" {
		return onboard.PhaseBlock
	}
	return g.phase
}

// SetLaunchPhase switches the rollout phase at runtime (F34). An
// invalid value is ignored — the phase only ever changes to one of
// the three known phases, never to a typo.
func (g *Gateway) SetLaunchPhase(p string) {
	if !onboard.ValidPhase(p) {
		return
	}
	g.phaseMu.Lock()
	defer g.phaseMu.Unlock()
	g.phase = p
}

// dryRun reports whether the gateway is in observe phase: record
// decisions but never block or redact.
func (g *Gateway) dryRun() bool { return g.LaunchPhase() == onboard.PhaseObserve }

// warnPhase reports whether the gateway is in warn phase: the BYOA
// tier rule is waived (see ChatCompletions) but everything else
// still enforces.
func (g *Gateway) warnPhase() bool { return g.LaunchPhase() == onboard.PhaseWarn }

// Words returns copies of the gateway's current banned/custom word
// lists (F34): the startup lists plus any template words applied
// since. The slices are copies, so callers cannot mutate the
// gateway's lists.
func (g *Gateway) Words() (banned, custom []string) {
	g.wordsMu.RLock()
	defer g.wordsMu.RUnlock()
	return append([]string(nil), g.bannedWords...), append([]string(nil), g.customWords...)
}

// SetWords replaces the gateway's word lists at runtime (F34): the
// detector is rebuilt per request, so the next call already uses
// the new lists. Callers pass full lists (the API unions template
// words onto the current lists before calling).
func (g *Gateway) SetWords(banned, custom []string) {
	g.wordsMu.Lock()
	defer g.wordsMu.Unlock()
	g.bannedWords = append([]string(nil), banned...)
	g.customWords = append([]string(nil), custom...)
}

// words is the detector's word source: the runtime lists.
func (g *Gateway) words() (banned, custom []string) { return g.Words() }

// redactForTier reports whether the tier's findings are redacted
// (F34): the runtime override wins when set (the conservative
// preset redacts both tiers), otherwise Policy.RedactForTier.
func (g *Gateway) redactForTier(t store.Tier) bool {
	g.redactMu.RLock()
	defer g.redactMu.RUnlock()
	if g.redactOverride != nil {
		return g.redactOverride[t]
	}
	return g.policy.RedactForTier[t]
}

// SetRedactForTier replaces the redaction tiers at runtime (F34).
// A nil map clears the override and restores Policy.RedactForTier.
func (g *Gateway) SetRedactForTier(m map[store.Tier]bool) {
	g.redactMu.Lock()
	defer g.redactMu.Unlock()
	if m == nil {
		g.redactOverride = nil
		return
	}
	cp := make(map[store.Tier]bool, len(m))
	for k, v := range m {
		cp[k] = v
	}
	g.redactOverride = cp
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
	banned, custom := g.words()
	return guard.NewDetector(banned, custom)
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
	// Smart routing (F30): "auto" is a virtual model name. It is
	// listed only when the layer is enabled, and only when no real
	// channel already serves a model literally named "auto".
	if g.autoRouteConfig().Enabled && !g.anyChannelForModel("auto") {
		data = append(data, map[string]any{"id": "auto", "object": "model", "owned_by": "agentgate"})
	}
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
	// F38 slice 2: a terminal under a plugin disposition
	// (downgrade / block_sensitive) is treated as BYOA for
	// authorization — the stored token is unchanged, and the next
	// login re-issues at the right tier. The cut is about the
	// terminal, not the request, so neither the warn phase nor an
	// F24 exception grant waives it (see the tier block below).
	pluginAction := g.st.PluginActionFor(user.ID, tok.AgentType)
	effTier := tok.Tier
	if pluginpol.DowngradesTier(pluginAction) {
		effTier = store.TierBYOA
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

	// Banned words: hard block. This is a safety layer, so it blocks
	// in every phase except observe — the warn phase waives only the
	// BYOA tier rule below, never banned content.
	if hasBanned && !g.dryRun() {
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
	// warnedTier records that the BYOA tier block was waived by the
	// warn phase (F34): the call proceeds through the normal trust
	// routing below (still redacted when the channel demands it) and
	// the decision is recorded as "warn" at the end.
	warnedTier := false

	// BYOA tier may never send confidential+ data anywhere — except
	// in warn phase, where the tier rule is the rollout gate: the
	// call goes through (trust routing and mandatory redaction
	// still apply) and is recorded as a warning, so the operator
	// can see exactly what blocking would have caught before
	// switching to block phase. A grant still waives the block in
	// block phase and is spent; in warn phase no grant is needed
	// and none is consumed. A plugin disposition (effTier BYOA via
	// the terminal, not the token) is exempt from both waivers: the
	// terminal is restricted until its next clean heartbeat.
	if effTier == store.TierBYOA && level >= store.LevelConfidential && !g.dryRun() {
		if pluginpol.DowngradesTier(pluginAction) {
			ts.decision, ts.level = "block", level.String()
			d := mergeDetail(detail, "reason", "plugin_policy")
			d["plugin_action"] = pluginAction
			g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
				Kind: "violation", Action: "model_call_blocked", Model: req.Model, DataLevel: level.String(), Decision: "block",
				Detail: d})
			writeErr(w, http.StatusForbidden, "data_level_forbidden",
				"this terminal is restricted by plugin policy ("+pluginAction+"); remove the violating plugin and wait for the next heartbeat")
			return
		}
		if grant != nil {
			// A valid grant waives the tier block for this one call;
			// the call is still desensitized when the channel
			// demands it (below), and the grant is spent before
			// forwarding.
			grantWaived = true
		} else if g.warnPhase() {
			warnedTier = true
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

	// ---- Model resolution: direct model, or smart routing (F30) ----
	// model=auto resolves to a concrete model before any channel
	// lookup: "auto" names no channel. Classification runs on the
	// request text (local-only: sample matching + rules, nothing
	// leaves the process) and picks a tier; the tier's model group
	// is walked in priority order and the first model with a
	// compliant candidate channel serves. The data level and trust
	// rules are unchanged — pickCandidates applies them per model,
	// so complexity routing can only choose inside the compliant
	// set, never widen it. The resolved model replaces "auto" in
	// the forwarded body, the trace and every later audit event;
	// the decision itself is recorded on the detail map, so the
	// model_call event carries the full decision log.
	var candidates []*store.Channel
	if req.Model == "auto" {
		cfg := g.autoRouteConfig()
		if !cfg.Enabled {
			ts.decision = "error"
			writeErr(w, http.StatusBadRequest, "model_not_available", "smart routing is disabled; request a concrete model name")
			return
		}
		hasToolCalls := false
		for _, m := range req.Messages {
			if len(m.ToolCalls) > 0 {
				hasToolCalls = true
			}
		}
		res := g.resolveAutoModel(cfg, fullText.String(), len(req.Messages), hasToolCalls, level, false)
		if res.model == "" && grant != nil {
			// Same waiver as a direct model request (F24): a valid
			// grant waives the trust rule for this one call.
			res = g.resolveAutoModel(cfg, fullText.String(), len(req.Messages), hasToolCalls, level, true)
			if res.model != "" {
				grantWaived = true
			}
		}
		if res.model == "" {
			// No tier has a compliant channel: mirror the direct
			// model's explicit errors over the union of group models.
			if grantPresented {
				ts.decision, ts.level = "block", level.String()
				g.rejectInvalidGrant(w, user, tok, req.Model, level, detail)
				return
			}
			if g.autoChannelsCoolingDown(cfg, level, false) {
				writeErr(w, http.StatusServiceUnavailable, "channels_in_cooldown",
					"all eligible model channels are temporarily cooling down after repeated failures")
				return
			}
			if g.autoChannelsBlockedByTrust(cfg, level, false) {
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
		candidates = res.candidates
		req.Model = res.model
		body = rewriteModel(body, res.model)
		ts.model = res.model
		g.recordAutoRoute(res.dec, res.servedTier, res.tierFallback)
		detail["auto_route"] = res.dec.Tier + ":" + res.dec.Source
		detail["auto_route_confidence"] = fmt.Sprintf("%.2f", res.dec.Confidence)
		detail["auto_route_requested"] = "auto"
		if res.dec.SampleID != "" {
			detail["auto_route_sample"] = res.dec.SampleID
		}
		if res.tierFallback {
			detail["auto_route_fallback"] = res.servedTier
		}
	} else {
		if !g.anyChannelForModel(req.Model) {
			ts.decision = "error"
			writeErr(w, http.StatusBadRequest, "model_not_available", "model is not available on this gateway")
			return
		}
		candidates = g.pickCandidates(req.Model, level, false)
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
	if len(allFindings) > 0 && (g.redactForTier(effTier) || mustRedact) && !g.dryRun() {
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
	if g.dryRun() && len(allFindings) > 0 {
		decision = "dry_run:" + decision
	}
	// Warn phase (F34): the BYOA tier block was waived above — the
	// headline decision is "warn" (with the reason in detail), even
	// when the call was also redacted (that is recorded separately
	// in detail["desensitized"]).
	if warnedTier {
		decision = "warn"
		detail["warn_reason"] = "byoa_tier_would_block"
	}

	// ---- Semantic review (F28) ----
	// Second layer above the rule DLP: the local sample-matching
	// layer catches paraphrased / split / injected requests the
	// rules missed, and the optional deep layer asks a self-hosted
	// guard model. The review runs on the desensitized text — when
	// the request had findings, a redacted copy is built for the
	// review, so the deep layer never sees raw PII even when the
	// forwarded body is not redacted (or in DryRun, where the
	// pipeline observes without redacting). It runs before the
	// cache, so a blocked request is never served from cache, and
	// before the grant is spent, so a grant is never consumed by a
	// call the review blocks. A grant never waives a review block:
	// like a banned word, this is a safety layer, not a
	// classification waiver.
	reviewBody := body
	if len(allFindings) > 0 {
		reviewBody, _ = redactBody(body, det)
	}
	semDec := g.runSemanticReview(r.Context(), reviewText(reviewBody, fullText.String()), level)
	if semDec.Action == semreview.DecisionBlock {
		if g.dryRun() {
			// Observe only: record the would-be block, do not block.
			g.auditSemanticReview(user, tok, req.Model, level,
				semreview.Decision{Action: "dry_run", Source: semDec.Source, SampleID: semDec.SampleID,
					Category: semDec.Category, Similarity: semDec.Similarity, Reason: semDec.Reason})
			detail["semantic_review"] = "dry_run:would_block"
		} else {
			ts.decision, ts.level = "block", level.String()
			vDetail := mergeDetail(detail, "reason", "semantic_review_blocked")
			vDetail["semantic_source"] = semDec.Source
			if semDec.SampleID != "" {
				vDetail["semantic_sample"] = semDec.SampleID
				vDetail["semantic_similarity"] = fmt.Sprintf("%.2f", semDec.Similarity)
			}
			g.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, AgentType: tok.AgentType, Tier: string(tok.Tier),
				Kind: "violation", Action: "model_call_blocked", Model: req.Model, DataLevel: level.String(), Decision: "block",
				Detail: vDetail})
			writeErr(w, http.StatusForbidden, "semantic_review_blocked", "request blocked by semantic review (suspicious intent)")
			return
		}
	} else if semDec.Action == semreview.DecisionAlert {
		g.auditSemanticReview(user, tok, req.Model, level, semDec)
		detail["semantic_review"] = "alert:" + semDec.Source
	} else if d := semanticReviewDetail(semDec); d != "" {
		detail["semantic_review"] = d
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
	cacheEligible := !req.Stream && !g.dryRun() && level <= store.LevelInternal &&
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

	// ---- Forward, with failure-classified retries (F33) ----
	// Failover may only move between channels whose MaxDataLevel is not
	// higher than the first pick's: data-level constraints beat
	// availability, so a request never "upgrades" to a more privileged
	// channel (or "downgrades" to a less privileged one) just because
	// the first pick failed. Since candidates are sorted least-
	// privileged first, the chain is the leading run of channels at the
	// first pick's level. Classification never changes this rule.
	//
	// Per channel, each key gets its own transient-retry budget
	// (same key, exponential backoff); auth failures rotate to the
	// next key at once with no backoff and no same-key retry. A
	// channel is abandoned only when its transient budget is spent
	// or its keys are all auth-dead — and only then does the F1
	// failure counter move (one count per failed call, not per wire
	// attempt, so cooldown still means N consecutive failed calls).
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
	// attempts counts wire attempts across the whole call, for the
	// audit trail numbering and the model_call detail.
	attempts := 0
	for i, ch := range chain {
		// Try this channel's keys in rotation order (F12): a 401 means
		// this key is dead, not the channel — disable the key and retry
		// the same in-flight request with the next key. A 403 rotates
		// the same way but does not disable the key (it may only lack
		// scope for this model). Other failures keep the F1 semantics
		// (channel-level failover), now with same-key retries first:
		// 429/5xx/network error is the channel or upstream being
		// unwell, not one key.
		enabled := ch.EnabledKeyCount()
		if enabled == 0 {
			// Every key on this channel is disabled (upstream rejected
			// them, or an admin did): the channel cannot serve, but it
			// is not a transport failure — report it explicitly.
			noEnabledKey = true
			continue
		}
		// failed is set when this channel had a transient failure
		// this call; failReason names the hop for the F1 audit event.
		failed := false
		failReason := ""
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
			masked := g.keyMasked(ch, keyIdx)
			upURL := strings.TrimSuffix(ch.BaseURL, "/") + "/chat/completions"
			// Transient failures retry on this same key with backoff
			// (F33); the attempt loop only ends early for a served
			// response, an auth rotation, or a spent budget.
			var r2 *http.Response
			abandoned := false
			for attempt := 0; attempt <= g.maxTransientRetries(); attempt++ {
				if attempt > 0 {
					time.Sleep(g.retryBackoff(attempt - 1))
				}
				attempts++
				start := time.Now()
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
				r2, err = g.client.Do(upReq)
				if err != nil {
					// No response started, so retrying is safe: no
					// bytes have been written to the caller yet.
					g.auditAttempt(user, tok, req.Model, level, attempts, ch, masked, classTransient, "transport_error", time.Since(start))
					failed = true
					failReason = "upstream_unavailable"
					if attempt < g.maxTransientRetries() {
						continue
					}
					abandoned = true
					r2 = nil
					break
				}
				class := classifyStatus(r2.StatusCode)
				g.auditAttempt(user, tok, req.Model, level, attempts, ch, masked, class, strconv.Itoa(r2.StatusCode), time.Since(start))
				switch class {
				case classAuth:
					// The key is the problem, not the channel: a 401
					// proves the key dead (disable it, F12); a 403
					// may only be a scope gap, so the key stays
					// enabled. Either way, never retry the same key.
					// When it was the last enabled key, return the
					// upstream auth error itself — the caller sees
					// the real auth failure, not a rewritten 502.
					if r2.StatusCode == http.StatusUnauthorized {
						g.disableKey(ch, keyIdx, "upstream_401")
					}
					if tried < enabled {
						r2.Body.Close()
						r2 = nil
					}
				case classTransient:
					failed = true
					failReason = fmt.Sprintf("upstream_%d", r2.StatusCode)
					if attempt < g.maxTransientRetries() {
						r2.Body.Close()
						r2 = nil
						continue
					}
					// Budget spent: abandon the channel for the next
					// same-level channel — the upstream is the
					// problem, not this key, so the next key on the
					// same channel would not help either.
					r2.Body.Close()
					r2 = nil
					abandoned = true
				default:
					// Success, or a client error (the request's
					// fault): the response is returned as-is and the
					// channel proved healthy, so the F1 consecutive
					// counter resets.
					if i > 0 {
						fallbackFrom = chain[0].Name
					}
					g.recordSuccess(ch.ID)
				}
				break
			}
			if r2 != nil {
				resp = r2
				channel = ch
				break
			}
			if abandoned {
				break
			}
			// Auth failure with more keys: rotate to the next key.
		}
		if resp != nil {
			break
		}
		if failed {
			// One F1 failure per abandoned channel per call (the
			// per-attempt trail is already in the audit log), and
			// one hop event when another channel follows.
			g.noteFailure(ch)
			if i+1 < len(chain) {
				g.auditFallback(user, tok, req.Model, level, ch, chain[i+1], failReason)
			}
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
	if attempts > 1 {
		// Retried calls carry their wire-attempt count on the call
		// event itself; the per-attempt trail is the forward_attempt
		// events (F33).
		detail["attempts"] = strconv.Itoa(attempts)
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
