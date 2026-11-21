// Package api implements the management/employee REST API (/api/...),
// separate from the agent-facing token体系 on /v1 and /mcp.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/alert"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/federation"
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
	mux.HandleFunc("GET /api/me", a.requireSession(a.me))
	mux.HandleFunc("POST /api/device/approve", a.requireSession(a.deviceApprove))
	mux.HandleFunc("GET /api/channels", a.requireAdmin(a.listChannels))
	mux.HandleFunc("POST /api/channels", a.requireAdmin(a.createChannel))
	mux.HandleFunc("GET /api/skills", a.requireSession(a.listSkills))
	mux.HandleFunc("POST /api/skills", a.requireSession(a.createSkill))
	mux.HandleFunc("POST /api/skills/{id}/review", a.requireAdmin(a.reviewSkill))
	mux.HandleFunc("GET /api/skills/{id}/download", a.requireTokenOrSession(a.downloadSkill))
	mux.HandleFunc("GET /api/audit", a.requireAdmin(a.queryAudit))
	mux.HandleFunc("GET /api/audit/verify", a.requireAdmin(a.verifyAudit))
	mux.HandleFunc("GET /api/audit/export", a.requireAdmin(a.exportAudit))
	mux.HandleFunc("POST /api/audit/archive", a.requireAdmin(a.archiveAudit))
	mux.HandleFunc("GET /api/stats", a.requireAdmin(a.getStats))
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
	mux.HandleFunc("POST /api/setup/device/start", a.deviceStart)
	mux.HandleFunc("POST /api/setup/device/poll", a.devicePoll)
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
	if _, err := a.auth.ApproveDeviceCode(in.Code, u, in.DeviceName); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "auth", Action: "device_approved", Decision: "allow",
		Detail: map[string]string{"code": in.Code}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "approved"})
}

// channelView is the admin-facing channel shape: the stored fields
// plus runtime health (healthy | cooling_down | disabled). The
// encrypted key is never exposed.
type channelView struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	BaseURL             string          `json:"base_url"`
	KeyMasked           string          `json:"key_masked"`
	Models              []string        `json:"models"`
	MaxDataLevel        store.DataLevel `json:"max_data_level"`
	Weight              int             `json:"weight"`
	Disabled            bool            `json:"disabled"`
	Health              string          `json:"health"`
	ConsecutiveFailures int             `json:"consecutive_failures"`
	CooldownUntil       *time.Time      `json:"cooldown_until,omitempty"`
}

func (a *API) listChannels(w http.ResponseWriter, r *http.Request, u *store.User) {
	var out []channelView
	now := time.Now()
	a.st.Read(func(st *store.State) {
		for _, ch := range st.Channels {
			v := channelView{ID: ch.ID, Name: ch.Name, BaseURL: ch.BaseURL,
				KeyMasked: ch.KeyMasked, Models: ch.Models, MaxDataLevel: ch.MaxDataLevel,
				Weight: ch.Weight, Disabled: ch.Disabled, Health: ch.Health(now),
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
		Name         string   `json:"name"`
		BaseURL      string   `json:"base_url"`
		APIKey       string   `json:"api_key"`
		Models       []string `json:"models"`
		MaxDataLevel string   `json:"max_data_level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.BaseURL == "" {
		writeErr(w, http.StatusBadRequest, "name and base_url required")
		return
	}
	if !strings.HasPrefix(in.BaseURL, "https://") && !strings.HasPrefix(in.BaseURL, "http://127.0.0.1") && !strings.HasPrefix(in.BaseURL, "http://localhost") {
		writeErr(w, http.StatusBadRequest, "base_url must be https (or loopback http for testing)")
		return
	}
	lvl, ok := store.ParseDataLevel(in.MaxDataLevel)
	if !ok {
		lvl = store.LevelInternal
	}
	enc, err := a.box.Encrypt(in.APIKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encrypt failed")
		return
	}
	ch := &store.Channel{ID: store.NewID("ch"), Name: in.Name, BaseURL: in.BaseURL,
		APIKeyEnc: enc, KeyMasked: secretbox.Mask(in.APIKey), Models: in.Models, MaxDataLevel: lvl, Weight: 1}
	a.st.WithLock(func(st *store.State) { st.Channels[ch.ID] = ch })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "channel_created",
		Detail: map[string]string{"channel": ch.Name, "max_data_level": lvl.String()}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": ch.ID, "key_masked": ch.KeyMasked})
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
