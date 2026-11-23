package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/autorouter"
	"github.com/myjnathan/agentgate/internal/store"
)

// Smart routing management (backlog F30): the policy (enabled,
// threshold, simple/complex model groups) and the complexity-sample
// library. Policy changes are admin/security only (roles.Policy);
// sample texts are admin-curated example phrasings, so the audit
// events for sample changes record only the sample ID and tier —
// never the text itself. A group model no channel serves is not
// rejected (it may be a forward reference to a model added later);
// GET reports it under unresolved_models, and at runtime the tier
// simply falls back to the other group.

// SetAutoRouteStats wires the gateway's runtime counters into the
// API (nil = report zeroes).
func (a *API) SetAutoRouteStats(fn func() autorouter.Stats) { a.autoRouteStats = fn }

// effectiveAutoRoute returns the policy in force: the stored one, or
// the built-in defaults when the admin never configured the layer.
func (a *API) effectiveAutoRoute() (*store.AutoRouteConfig, bool) {
	var cfg *store.AutoRouteConfig
	a.st.Read(func(st *store.State) {
		if st.AutoRoute != nil {
			cp := *st.AutoRoute
			cfg = &cp
		}
	})
	if cfg == nil {
		return autorouter.DefaultConfig(), false
	}
	return cfg, true
}

func (a *API) getAutoRoute(w http.ResponseWriter, r *http.Request, u *store.User) {
	cfg, configured := a.effectiveAutoRoute()
	stats := autorouter.Stats{}
	if a.autoRouteStats != nil {
		stats = a.autoRouteStats()
	}
	samples := cfg.Samples
	if samples == nil {
		samples = []store.AutoRouteSample{}
	}
	simpleGroup := cfg.SimpleGroup
	if simpleGroup == nil {
		simpleGroup = []string{}
	}
	complexGroup := cfg.ComplexGroup
	if complexGroup == nil {
		complexGroup = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":        configured,
		"enabled":           cfg.Enabled,
		"threshold":         cfg.Threshold,
		"simple_group":      simpleGroup,
		"complex_group":     complexGroup,
		"samples":           samples,
		"unresolved_models": a.autoRouteUnresolved(cfg),
		"stats":             stats,
	})
}

// autoRouteUnresolved lists group models no enabled channel serves
// (a forward reference, or a typo — visible here rather than
// failing the request, since the other tier covers for it).
func (a *API) autoRouteUnresolved(cfg *store.AutoRouteConfig) []string {
	served := map[string]bool{}
	a.st.Read(func(st *store.State) {
		for _, ch := range st.Channels {
			if ch.Disabled {
				continue
			}
			for _, m := range ch.Models {
				served[m] = true
			}
		}
	})
	seen := map[string]bool{}
	var out []string
	for _, m := range append(append([]string(nil), cfg.SimpleGroup...), cfg.ComplexGroup...) {
		if !served[m] && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func (a *API) updateAutoRoute(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Enabled      *bool     `json:"enabled"`
		Threshold    *float64  `json:"threshold"`
		SimpleGroup  *[]string `json:"simple_group"`
		ComplexGroup *[]string `json:"complex_group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	cfg, _ := a.effectiveAutoRoute()
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if in.Threshold != nil {
		cfg.Threshold = *in.Threshold
	}
	if in.SimpleGroup != nil {
		cfg.SimpleGroup = *in.SimpleGroup
	}
	if in.ComplexGroup != nil {
		cfg.ComplexGroup = *in.ComplexGroup
	}
	if err := autorouter.ValidateConfig(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.st.WithLock(func(st *store.State) { st.AutoRoute = cfg })
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot persist smart routing policy")
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "auto_route_policy_updated",
		Detail: map[string]string{"enabled": boolStr(cfg.Enabled)}})
	writeJSON(w, http.StatusOK, map[string]any{"enabled": cfg.Enabled, "threshold": cfg.Threshold,
		"simple_group": cfg.SimpleGroup, "complex_group": cfg.ComplexGroup})
}

func (a *API) addAutoRouteSample(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Tier string `json:"tier"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := autorouter.ValidateSample(in.Tier, in.Text); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, _ := a.effectiveAutoRoute()
	s := store.AutoRouteSample{ID: store.NewID("ar"), Tier: in.Tier,
		Text: in.Text, CreatedAt: time.Now().UTC()}
	cfg.Samples = append(cfg.Samples, s)
	a.st.WithLock(func(st *store.State) { st.AutoRoute = cfg })
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot persist smart routing sample")
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "auto_route_sample_added",
		Detail: map[string]string{"sample_id": s.ID, "tier": s.Tier}})
	writeJSON(w, http.StatusCreated, s)
}

func (a *API) deleteAutoRouteSample(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	cfg, _ := a.effectiveAutoRoute()
	idx := -1
	for i, s := range cfg.Samples {
		if s.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "sample not found")
		return
	}
	removed := cfg.Samples[idx]
	cfg.Samples = append(cfg.Samples[:idx], cfg.Samples[idx+1:]...)
	a.st.WithLock(func(st *store.State) { st.AutoRoute = cfg })
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot persist smart routing sample")
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "auto_route_sample_deleted",
		Detail: map[string]string{"sample_id": removed.ID, "tier": removed.Tier}})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
