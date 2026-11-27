package api

import (
	"encoding/json"
	"net/http"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/inject"
	"github.com/myjnathan/agentgate/internal/store"
)

// Prompt-injection policy management (backlog F39, slice 1): the
// rule layer's enable switch and mode (observe / alert / block).
// Policy changes are admin/security only (roles.Policy); policy
// changes are audited without any request text (the layer never
// stores request text anywhere).

// SetInjectionStats wires the gateway's runtime counters into the
// API (nil = report zeroes).
func (a *API) SetInjectionStats(fn func() inject.Stats) { a.injectionStats = fn }

// effectiveInjection returns the policy in force: the stored one,
// or the built-in defaults when the admin never configured the
// layer.
func (a *API) effectiveInjection() (*store.InjectionConfig, bool) {
	var cfg *store.InjectionConfig
	a.st.Read(func(st *store.State) {
		if st.Injection != nil {
			cp := *st.Injection
			cfg = &cp
		}
	})
	if cfg == nil {
		return inject.DefaultConfig(), false
	}
	return cfg, true
}

func (a *API) getInjection(w http.ResponseWriter, r *http.Request, u *store.User) {
	cfg, configured := a.effectiveInjection()
	stats := inject.Stats{}
	if a.injectionStats != nil {
		stats = a.injectionStats()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": configured,
		"enabled":    cfg.Enabled,
		"mode":       cfg.Mode,
		"stats":      stats,
	})
}

func (a *API) updateInjection(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Enabled *bool   `json:"enabled"`
		Mode    *string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	cfg, _ := a.effectiveInjection()
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if in.Mode != nil {
		cfg.Mode = *in.Mode
	}
	if cfg.Mode == "" {
		cfg.Mode = inject.DefaultMode
	}
	if err := inject.ValidateConfig(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.st.WithLock(func(st *store.State) { st.Injection = cfg })
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot persist injection policy")
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "injection_policy_updated",
		Detail: map[string]string{"enabled": boolStr(cfg.Enabled), "mode": cfg.Mode}})
	writeJSON(w, http.StatusOK, map[string]any{"enabled": cfg.Enabled, "mode": cfg.Mode})
}
