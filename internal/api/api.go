// Package api implements the management/employee REST API (/api/...),
// separate from the agent-facing token体系 on /v1 and /mcp.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/alert"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/cache"
	"github.com/myjnathan/agentgate/internal/federation"
	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/oidc"
	"github.com/myjnathan/agentgate/internal/restadapter"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/store"
)

type API struct {
	st         *store.Store
	auth       *auth.Service
	box        *secretbox.Box
	audit      *audit.Log
	dispatcher *alert.Dispatcher
	fed        *federation.Registry
	restAd     *restadapter.Adapter
	oidc       *oidc.Provider
	fk         *folderkb.Registry
	cache      *cache.Cache
}

func New(st *store.Store, au *auth.Service, box *secretbox.Box, al *audit.Log) *API {
	return &API{st: st, auth: au, box: box, audit: al}
}

// SetAlertDispatcher wires the alert dispatcher (backlog F5) used by
// the webhook test endpoint and the list status columns.
func (a *API) SetAlertDispatcher(d *alert.Dispatcher) {
	a.dispatcher = d
}

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("GET /api/auth/providers", a.oidcProviders)
	mux.HandleFunc("GET /api/auth/oidc/status", a.requireAdmin(a.oidcStatus))
	mux.HandleFunc("GET /auth/oidc/login", a.oidcLogin)
	mux.HandleFunc("GET /auth/oidc/callback", a.oidcCallback)
	mux.HandleFunc("GET /api/me", a.requireSession(a.me))
	mux.HandleFunc("POST /api/device/approve", a.requireSession(a.deviceApprove))
	mux.HandleFunc("GET /api/channels", a.requireAdmin(a.listChannels))
	mux.HandleFunc("POST /api/channels", a.requireAdmin(a.createChannel))
	mux.HandleFunc("POST /api/channels/{id}/keys", a.requireAdmin(a.addChannelKey))
	mux.HandleFunc("DELETE /api/channels/{id}/keys/{index}", a.requireAdmin(a.deleteChannelKey))
	mux.HandleFunc("POST /api/channels/{id}/keys/{index}/enable", a.requireAdmin(a.enableChannelKey))
	mux.HandleFunc("POST /api/channels/{id}/keys/{index}/disable", a.requireAdmin(a.disableChannelKey))
	mux.HandleFunc("GET /api/skills", a.requireSession(a.listSkills))
	mux.HandleFunc("POST /api/skills", a.requireSession(a.createSkill))
	mux.HandleFunc("POST /api/skills/{id}/review", a.requireAdmin(a.reviewSkill))
	mux.HandleFunc("GET /api/skills/{id}/download", a.requireTokenOrSession(a.downloadSkill))
	mux.HandleFunc("GET /api/audit", a.requireAdmin(a.queryAudit))
	mux.HandleFunc("GET /api/audit/verify", a.requireAdmin(a.verifyAudit))
	mux.HandleFunc("GET /api/audit/export", a.requireAdmin(a.exportAudit))
	mux.HandleFunc("POST /api/audit/archive", a.requireAdmin(a.archiveAudit))
	mux.HandleFunc("GET /api/stats", a.requireAdmin(a.getStats))
	mux.HandleFunc("GET /api/budgets", a.requireAdmin(a.listBudgets))
	mux.HandleFunc("POST /api/budgets", a.requireAdmin(a.createBudget))
	mux.HandleFunc("PATCH /api/budgets/{id}", a.requireAdmin(a.updateBudget))
	mux.HandleFunc("DELETE /api/budgets/{id}", a.requireAdmin(a.deleteBudget))
	mux.HandleFunc("GET /api/cache", a.requireAdmin(a.getCache))
	mux.HandleFunc("POST /api/cache", a.requireAdmin(a.setCache))
	mux.HandleFunc("POST /api/cache/clear", a.requireAdmin(a.clearCache))
	mux.HandleFunc("GET /api/tokens", a.requireAdmin(a.listTokens))
	mux.HandleFunc("PATCH /api/tokens/{id}", a.requireAdmin(a.updateTokenLimits))
	mux.HandleFunc("GET /api/alerts/webhooks", a.requireAdmin(a.listWebhooks))
	mux.HandleFunc("POST /api/alerts/webhooks", a.requireAdmin(a.createWebhook))
	mux.HandleFunc("DELETE /api/alerts/webhooks/{id}", a.requireAdmin(a.deleteWebhook))
	mux.HandleFunc("POST /api/alerts/webhooks/{id}/test", a.requireAdmin(a.testWebhook))
	mux.HandleFunc("GET /api/mcp/upstreams", a.requireAdmin(a.listUpstreams))
	mux.HandleFunc("POST /api/mcp/upstreams", a.requireAdmin(a.createUpstream))
	mux.HandleFunc("DELETE /api/mcp/upstreams/{id}", a.requireAdmin(a.deleteUpstream))
	mux.HandleFunc("POST /api/mcp/upstreams/{id}/test", a.requireAdmin(a.testUpstream))
	mux.HandleFunc("GET /api/mcp/rest-tools", a.requireAdmin(a.listRESTTools))
	mux.HandleFunc("POST /api/mcp/rest-tools", a.requireAdmin(a.createRESTTool))
	mux.HandleFunc("DELETE /api/mcp/rest-tools/{id}", a.requireAdmin(a.deleteRESTTool))
	mux.HandleFunc("POST /api/mcp/rest-tools/{id}/test", a.requireAdmin(a.testRESTTool))
	mux.HandleFunc("GET /api/prompts", a.requireSession(a.listPrompts))
	mux.HandleFunc("POST /api/prompts", a.requireSession(a.createPrompt))
	mux.HandleFunc("GET /api/prompts/{id}", a.requireSession(a.getPrompt))
	mux.HandleFunc("POST /api/prompts/{id}/versions", a.requireSession(a.addPromptVersion))
	mux.HandleFunc("POST /api/prompts/{id}/review", a.requireAdmin(a.reviewPrompt))
	mux.HandleFunc("POST /api/prompts/{id}/publish", a.requireAdmin(a.publishPrompt))
	mux.HandleFunc("DELETE /api/prompts/{id}", a.requireAdmin(a.deletePrompt))
	mux.HandleFunc("GET /api/kb/folders", a.requireAdmin(a.listFolderSources))
	mux.HandleFunc("POST /api/kb/folders", a.requireAdmin(a.createFolderSource))
	mux.HandleFunc("DELETE /api/kb/folders/{id}", a.requireAdmin(a.deleteFolderSource))
	mux.HandleFunc("POST /api/kb/folders/{id}/rescan", a.requireAdmin(a.rescanFolderSource))
	mux.HandleFunc("POST /api/setup/device/start", a.deviceStart)
	mux.HandleFunc("POST /api/setup/device/poll", a.devicePoll)
	mux.HandleFunc("POST /api/setup/device/attest", a.deviceAttest)
	mux.HandleFunc("GET /api/attestation/policies", a.requireAdmin(a.listAttestationPolicies))
	mux.HandleFunc("POST /api/attestation/policies", a.requireAdmin(a.upsertAttestationPolicy))
	mux.HandleFunc("DELETE /api/attestation/policies/{agent}", a.requireAdmin(a.deleteAttestationPolicy))
	mux.HandleFunc("GET /api/attestation/reports", a.requireAdmin(a.listAttestationReports))
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, user *store.User)

func (a *API) sessionUser(r *http.Request) (*store.User, bool) {
	h := r.Header.Get("Authorization")
	tok := strings.TrimPrefix(h, "Bearer ")
	uid, _, _, ok := a.auth.SessionUser(tok)
	if !ok {
		return nil, false
	}
	var user *store.User
	a.st.Read(func(st *store.State) {
		if u, ok := st.Users[uid]; ok {
			cp := *u
			user = &cp
		}
	})
	return user, user != nil
}

func (a *API) requireSession(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := a.sessionUser(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		fn(w, r, u)
	}
}

func (a *API) requireAdmin(fn handlerFunc) http.HandlerFunc {
	return a.requireSession(func(w http.ResponseWriter, r *http.Request, u *store.User) {
		if u.Role != "admin" && u.Role != "security" {
			writeErr(w, http.StatusForbidden, "admin role required")
			return
		}
		fn(w, r, u)
	})
}

// ---- endpoints ----

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	user, tok, err := a.auth.Login(in.Username, in.Password)
	if err != nil {
		// A lockout is audited as its own action (alertable, F5);
		// a plain bad password stays login_failed (noise, no alert).
		action := "login_failed"
		if errors.Is(err, auth.ErrAccountLocked) {
			action = "account_locked"
		}
		a.audit.Append(audit.Event{Actor: in.Username, Kind: "auth", Action: action, Decision: "block"})
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	a.audit.Append(audit.Event{Actor: user.Username, ActorID: user.ID, Kind: "auth", Action: "login", Decision: "allow"})
	writeJSON(w, http.StatusOK, map[string]any{
		"session": tok, "username": user.Username, "display_name": user.DisplayName,
		"department": user.Department, "role": user.Role,
	})
}

func (a *API) me(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"username": u.Username, "display_name": u.DisplayName,
		"department": u.Department, "role": u.Role,
	})
}

func (a *API) deviceStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentType string `json:"agent_type"`
		Tier      string `json:"tier"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	tier := store.TierBYOA
	if in.Tier == "managed" {
		// Managed tier requires admin-issued enrollment in production;
		// night-one accepts the request but the approving user sees the
		// requested tier in the UI. Record it in audit either way.
		tier = store.TierManaged
	}
	if in.AgentType == "" {
		in.AgentType = "other"
	}
	dc := a.auth.StartDeviceCode(in.AgentType, tier)
	writeJSON(w, http.StatusOK, map[string]any{
		"code": dc.Code, "secret": dc.Secret, "expires_at": dc.ExpiresAt,
		"instructions": "Open the AgentGate web UI, sign in, and approve this code under Devices.",
	})
}

func (a *API) devicePoll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code   string `json:"code"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	status, token := a.auth.PollDeviceCode(in.Code, in.Secret)
	resp := map[string]any{"status": status}
	if token != "" {
		resp["token"] = token
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) deviceApprove(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Code       string `json:"code"`
		DeviceName string `json:"device_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if in.DeviceName == "" {
		in.DeviceName = "agent device"
	}
	// Approval issues the token; a managed request that fails
	// attestation is downgraded to BYOA (backlog F14) rather than
	// refused. The downgrade is its own audit event so the F5 alert
	// pipeline can warn on it.
	var requestedTier store.Tier
	a.st.Read(func(st *store.State) {
		if dc, ok := st.DeviceCodes[in.Code]; ok {
			requestedTier = dc.Tier
		}
	})
	_, issuedTier, err := a.auth.ApproveDeviceCode(in.Code, u, in.DeviceName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	verdict := ""
	a.st.Read(func(st *store.State) {
		if dc, ok := st.DeviceCodes[in.Code]; ok {
			verdict = dc.AttestVerdict
		}
	})
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "auth", Action: "device_approved", Decision: "allow",
		Detail: map[string]string{"code": in.Code, "requested_tier": string(requestedTier),
			"issued_tier": string(issuedTier), "attest_verdict": verdict}})
	if issuedTier != requestedTier {
		reasons := ""
		a.st.Read(func(st *store.State) {
			if dc, ok := st.DeviceCodes[in.Code]; ok {
				reasons = strings.Join(dc.AttestReasons, ",")
			}
		})
		a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "attestation", Action: "attest_downgrade", Decision: "allow",
			Detail: map[string]string{"code": in.Code, "requested_tier": string(requestedTier),
				"issued_tier": string(issuedTier), "reasons": reasons}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "approved",
		"issued_tier": string(issuedTier), "attest_verdict": verdict})
}

// channelView is the admin-facing channel shape: the stored fields
// plus runtime health (healthy | cooling_down | disabled). The
// encrypted key is never exposed.
// channelKeyView is one key of a channel as the console sees it: its
// stable index, masked value and disabled state. The plaintext key is
// never returned.
type channelKeyView struct {
	Index          int        `json:"index"`
	Masked         string     `json:"masked"`
	Disabled       bool       `json:"disabled"`
	DisabledReason string     `json:"disabled_reason,omitempty"`
	DisabledAt     *time.Time `json:"disabled_at,omitempty"`
}

type channelView struct {
	ID                  string                      `json:"id"`
	Name                string                      `json:"name"`
	BaseURL             string                      `json:"base_url"`
	KeyMasked           string                      `json:"key_masked"`
	Keys                []channelKeyView            `json:"keys,omitempty"`
	Models              []string                    `json:"models"`
	Pricing             map[string]store.ModelPrice `json:"pricing,omitempty"`
	MaxDataLevel        store.DataLevel             `json:"max_data_level"`
	TrustType           store.TrustType             `json:"trust_type"`
	Weight              int                         `json:"weight"`
	Disabled            bool                        `json:"disabled"`
	Health              string                      `json:"health"`
	ConsecutiveFailures int                         `json:"consecutive_failures"`
	CooldownUntil       *time.Time                  `json:"cooldown_until,omitempty"`
}

func channelKeysView(ch *store.Channel) []channelKeyView {
	out := make([]channelKeyView, 0, len(ch.Keys))
	for i, k := range ch.Keys {
		v := channelKeyView{Index: i, Masked: k.Masked, Disabled: k.Disabled, DisabledReason: k.DisabledReason}
		if !k.DisabledAt.IsZero() {
			t := k.DisabledAt
			v.DisabledAt = &t
		}
		out = append(out, v)
	}
	return out
}

func (a *API) listChannels(w http.ResponseWriter, r *http.Request, u *store.User) {
	var out []channelView
	now := time.Now()
	a.st.Read(func(st *store.State) {
		for _, ch := range st.Channels {
			v := channelView{ID: ch.ID, Name: ch.Name, BaseURL: ch.BaseURL,
				KeyMasked: ch.PrimaryMasked(), Keys: channelKeysView(ch),
				Models: ch.Models, Pricing: ch.Pricing, MaxDataLevel: ch.MaxDataLevel,
				TrustType: ch.TrustTypeOrDefault(),
				Weight:    ch.Weight, Disabled: ch.Disabled, Health: ch.Health(now),
				ConsecutiveFailures: ch.ConsecutiveFailures}
			if ch.Health(now) == "cooling_down" {
				t := ch.CooldownUntil
				v.CooldownUntil = &t
			}
			out = append(out, v)
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{"channels": out})
}

func (a *API) createChannel(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Name         string                      `json:"name"`
		BaseURL      string                      `json:"base_url"`
		APIKey       string                      `json:"api_key"`
		APIKeys      []string                    `json:"api_keys"`
		Models       []string                    `json:"models"`
		Pricing      map[string]store.ModelPrice `json:"pricing"`
		MaxDataLevel string                      `json:"max_data_level"`
		TrustType    string                      `json:"trust_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.BaseURL == "" {
		writeErr(w, http.StatusBadRequest, "name and base_url required")
		return
	}
	// F13: per-model prices (USD per million tokens). A negative
	// price is a mistake, not a discount; more than a handful of
	// entries is a malformed payload, not a price list.
	if len(in.Pricing) > 32 {
		writeErr(w, http.StatusBadRequest, "too many pricing entries (max 32)")
		return
	}
	for model, p := range in.Pricing {
		if model == "" || p.InputPerMillion < 0 || p.OutputPerMillion < 0 {
			writeErr(w, http.StatusBadRequest, "pricing entries need a model name and non-negative prices")
			return
		}
	}
	if !strings.HasPrefix(in.BaseURL, "https://") && !strings.HasPrefix(in.BaseURL, "http://127.0.0.1") && !strings.HasPrefix(in.BaseURL, "http://localhost") {
		writeErr(w, http.StatusBadRequest, "base_url must be https (or loopback http for testing)")
		return
	}
	lvl, ok := store.ParseDataLevel(in.MaxDataLevel)
	if !ok {
		lvl = store.LevelInternal
	}
	// F17: trust type is explicit at creation; empty means signed cloud
	// (the previous default). An unknown value is rejected — a channel
	// must never silently fall back to a weaker trust assumption.
	tt := store.TrustSignedCloud
	if in.TrustType != "" {
		v, ok := store.ParseTrustType(in.TrustType)
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid trust_type (signed_cloud|dedicated_cloud|local_model)")
			return
		}
		tt = v
	}
	// F12: extra keys share the channel (round-robin, per-key disable).
	// The primary key is in.APIKey; when it is empty and extras were
	// given, the first extra becomes the primary so index 0 is never a
	// blank key while usable keys exist.
	plains := append([]string{in.APIKey}, in.APIKeys...)
	if plains[0] == "" && len(plains) > 1 {
		plains = plains[1:]
	}
	if len(plains) > store.MaxChannelKeys {
		writeErr(w, http.StatusBadRequest, "too many keys (max 10 per channel)")
		return
	}
	seen := map[string]bool{}
	for _, p := range plains {
		// A single empty primary key stays allowed (legacy: a channel
		// created without a key); an empty entry among several keys is
		// a mistake, not a key.
		if p == "" && len(plains) > 1 {
			writeErr(w, http.StatusBadRequest, "empty api key")
			return
		}
		if seen[p] {
			writeErr(w, http.StatusBadRequest, "duplicate api key")
			return
		}
		seen[p] = true
	}
	keys := make([]store.ChannelKey, 0, len(plains))
	for _, p := range plains {
		enc, err := a.box.Encrypt(p)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "encrypt failed")
			return
		}
		keys = append(keys, store.ChannelKey{Enc: enc, Masked: secretbox.Mask(p)})
	}
	ch := &store.Channel{ID: store.NewID("ch"), Name: in.Name, BaseURL: in.BaseURL,
		Keys: keys, Models: in.Models, Pricing: in.Pricing, MaxDataLevel: lvl, TrustType: tt, Weight: 1}
	a.st.WithLock(func(st *store.State) { st.Channels[ch.ID] = ch })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "channel_created",
		Detail: map[string]string{"channel": ch.Name, "max_data_level": lvl.String(), "trust_type": string(tt)}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": ch.ID, "key_masked": ch.PrimaryMasked(), "key_count": len(keys)})
}

// channelKeyParams resolves a channel and a key index from the URL.
// Keys are addressed by stable position (0 = primary), like new-api's
// per-index status list; deleting a key shifts the ones after it.
func (a *API) channelKeyParams(r *http.Request) (*store.Channel, int, bool) {
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || idx < 0 {
		return nil, 0, false
	}
	var ch *store.Channel
	a.st.Read(func(st *store.State) { ch = st.Channels[r.PathValue("id")] })
	if ch == nil || idx >= len(ch.Keys) {
		return nil, 0, false
	}
	return ch, idx, true
}

func (a *API) addChannelKey(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.APIKey == "" {
		writeErr(w, http.StatusBadRequest, "api_key required")
		return
	}
	var ch *store.Channel
	a.st.Read(func(st *store.State) { ch = st.Channels[r.PathValue("id")] })
	if ch == nil {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	if len(ch.Keys) >= store.MaxChannelKeys {
		writeErr(w, http.StatusBadRequest, "too many keys (max 10 per channel)")
		return
	}
	// Duplicate check decrypts the stored keys in memory (never logged
	// or returned) — masked forms alone cannot prove sameness.
	for _, k := range ch.Keys {
		if plain, err := a.box.Decrypt(k.Enc); err == nil && plain == in.APIKey {
			writeErr(w, http.StatusBadRequest, "duplicate api key")
			return
		}
	}
	enc, err := a.box.Encrypt(in.APIKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encrypt failed")
		return
	}
	idx := len(ch.Keys)
	a.st.WithLock(func(st *store.State) {
		if c, ok := st.Channels[ch.ID]; ok {
			c.Keys = append(c.Keys, store.ChannelKey{Enc: enc, Masked: secretbox.Mask(in.APIKey)})
		}
	})
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "channel_key_added",
		Detail: map[string]string{"channel": ch.Name, "key_index": strconv.Itoa(idx), "key_masked": secretbox.Mask(in.APIKey)}})
	writeJSON(w, http.StatusCreated, map[string]any{"index": idx, "key_masked": secretbox.Mask(in.APIKey)})
}

func (a *API) deleteChannelKey(w http.ResponseWriter, r *http.Request, u *store.User) {
	ch, idx, ok := a.channelKeyParams(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "channel or key not found")
		return
	}
	if len(ch.Keys) <= 1 {
		writeErr(w, http.StatusBadRequest, "cannot delete the last key of a channel")
		return
	}
	masked := ch.Keys[idx].Masked
	a.st.WithLock(func(st *store.State) {
		if c, ok := st.Channels[ch.ID]; ok {
			c.Keys = append(c.Keys[:idx], c.Keys[idx+1:]...)
		}
	})
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "channel_key_deleted",
		Detail: map[string]string{"channel": ch.Name, "key_index": strconv.Itoa(idx), "key_masked": masked}})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": idx})
}

func (a *API) setChannelKeyDisabled(w http.ResponseWriter, r *http.Request, u *store.User, disabled bool, action, reason string) {
	ch, idx, ok := a.channelKeyParams(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "channel or key not found")
		return
	}
	a.st.WithLock(func(st *store.State) {
		if c, ok := st.Channels[ch.ID]; ok {
			c.Keys[idx].Disabled = disabled
			if disabled {
				c.Keys[idx].DisabledReason = reason
				c.Keys[idx].DisabledAt = time.Now()
			} else {
				c.Keys[idx].DisabledReason = ""
				c.Keys[idx].DisabledAt = time.Time{}
			}
		}
	})
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: action,
		Detail: map[string]string{"channel": ch.Name, "key_index": strconv.Itoa(idx), "key_masked": ch.Keys[idx].Masked, "reason": reason}})
	writeJSON(w, http.StatusOK, map[string]any{"index": idx, "disabled": disabled})
}

func (a *API) enableChannelKey(w http.ResponseWriter, r *http.Request, u *store.User) {
	a.setChannelKeyDisabled(w, r, u, false, "channel_key_enabled", "")
}

func (a *API) disableChannelKey(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Reason == "" {
		in.Reason = "disabled_by_admin"
	}
	a.setChannelKeyDisabled(w, r, u, true, "channel_key_disabled", in.Reason)
}

func (a *API) listSkills(w http.ResponseWriter, r *http.Request, u *store.User) {
	var out []*store.Skill
	a.st.Read(func(st *store.State) {
		for _, sk := range st.Skills {
			if sk.Status != "published" && u.Role != "admin" && sk.AuthorID != u.ID {
				continue
			}
			if sk.Scope != "*" && u.Role != "admin" {
				if !strings.Contains(","+sk.Scope+",", ","+u.Department+",") {
					continue
				}
			}
			cp := *sk
			out = append(out, &cp)
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

func (a *API) createSkill(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Content string `json:"content"`
		Scope   string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.Content == "" {
		writeErr(w, http.StatusBadRequest, "name and content required")
		return
	}
	if in.Scope == "" {
		in.Scope = u.Department
	}
	sum := sha256.Sum256([]byte(in.Content))
	sk := &store.Skill{ID: store.NewID("sk"), Name: in.Name, Version: in.Version, Content: in.Content,
		Scope: in.Scope, Status: "in_review", AuthorID: u.ID, Checksum: hex.EncodeToString(sum[:]), UpdatedAt: time.Now()}
	a.st.WithLock(func(st *store.State) { st.Skills[sk.ID] = sk })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "skill_submitted",
		Detail: map[string]string{"skill": sk.Name, "version": sk.Version}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": sk.ID, "status": sk.Status, "checksum": sk.Checksum})
}

func (a *API) reviewSkill(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var in struct {
		Approve bool `json:"approve"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var status string
	a.st.WithLock(func(st *store.State) {
		if sk, ok := st.Skills[id]; ok {
			if in.Approve {
				sk.Status = "published"
			} else {
				sk.Status = "rejected"
			}
			status = sk.Status
		}
	})
	if status == "" {
		writeErr(w, http.StatusNotFound, "skill not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "skill_reviewed",
		Detail: map[string]string{"skill_id": id, "result": status}})
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

func (a *API) requireTokenOrSession(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u, ok := a.sessionUser(r); ok {
			fn(w, r, u)
			return
		}
		h := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, user, ok := a.auth.ResolveToken(h); ok {
			fn(w, r, user)
			return
		}
		writeErr(w, http.StatusUnauthorized, "unauthorized")
	}
}

func (a *API) downloadSkill(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var sk *store.Skill
	a.st.Read(func(st *store.State) {
		if s, ok := st.Skills[id]; ok && s.Status == "published" {
			cp := *s
			sk = &cp
		}
	})
	if sk == nil {
		writeErr(w, http.StatusNotFound, "skill not found or not published")
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("X-Skill-Checksum", sk.Checksum)
	_, _ = w.Write([]byte(sk.Content))
}

func (a *API) queryAudit(w http.ResponseWriter, r *http.Request, u *store.User) {
	events := a.audit.Query(r.URL.Query().Get("kind"), r.URL.Query().Get("actor"), 200)
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (a *API) verifyAudit(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, map[string]any{"chain_valid": a.audit.Verify()})
}
