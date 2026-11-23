package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/semreview"
	"github.com/myjnathan/agentgate/internal/store"
)

// Semantic review management (backlog F28): the policy (enabled,
// threshold, timeout, deep layer) and the risk-sample library.
// Policy changes are admin/security only (roles.Policy); sample
// texts are admin-curated risk patterns, so the audit events for
// sample changes record only the sample ID, category and action —
// never the text itself.

// SetSemReviewStats wires the gateway's runtime counters into the
// API (nil = report zeroes).
func (a *API) SetSemReviewStats(fn func() semreview.Stats) { a.semReviewStats = fn }

// effectiveSemReview returns the policy in force: the stored one, or
// the built-in defaults when the admin never configured the layer.
func (a *API) effectiveSemReview() (*store.SemanticReviewConfig, bool) {
	var cfg *store.SemanticReviewConfig
	a.st.Read(func(st *store.State) {
		if st.SemanticReview != nil {
			cp := *st.SemanticReview
			cfg = &cp
		}
	})
	if cfg == nil {
		return semreview.DefaultConfig(), false
	}
	return cfg, true
}

func (a *API) getSemanticReview(w http.ResponseWriter, r *http.Request, u *store.User) {
	cfg, configured := a.effectiveSemReview()
	stats := semreview.Stats{}
	if a.semReviewStats != nil {
		stats = a.semReviewStats()
	}
	deepChannel := ""
	if cfg.DeepChannelID != "" {
		a.st.Read(func(st *store.State) {
			if ch, ok := st.Channels[cfg.DeepChannelID]; ok {
				deepChannel = ch.Name
			}
		})
	}
	samples := cfg.Samples
	if samples == nil {
		samples = []store.SemanticReviewSample{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":           configured,
		"enabled":              cfg.Enabled,
		"threshold":            cfg.Threshold,
		"timeout_ms":           cfg.TimeoutMS,
		"controversial_action": cfg.ControversialAction,
		"deep_enabled":         cfg.DeepEnabled,
		"deep_channel_id":      cfg.DeepChannelID,
		"deep_channel":         deepChannel,
		"deep_model":           cfg.DeepModel,
		"samples":              samples,
		"stats":                stats,
	})
}

func (a *API) updateSemanticReview(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Enabled             *bool    `json:"enabled"`
		Threshold           *float64 `json:"threshold"`
		TimeoutMS           *int     `json:"timeout_ms"`
		ControversialAction *string  `json:"controversial_action"`
		DeepEnabled         *bool    `json:"deep_enabled"`
		DeepChannelID       *string  `json:"deep_channel_id"`
		DeepModel           *string  `json:"deep_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	cfg, _ := a.effectiveSemReview()
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if in.Threshold != nil {
		cfg.Threshold = *in.Threshold
	}
	if in.TimeoutMS != nil {
		cfg.TimeoutMS = *in.TimeoutMS
	}
	if in.ControversialAction != nil {
		cfg.ControversialAction = *in.ControversialAction
	}
	if in.DeepEnabled != nil {
		cfg.DeepEnabled = *in.DeepEnabled
	}
	if in.DeepChannelID != nil {
		cfg.DeepChannelID = *in.DeepChannelID
	}
	if in.DeepModel != nil {
		cfg.DeepModel = *in.DeepModel
	}
	if cfg.ControversialAction == "" {
		cfg.ControversialAction = semreview.DefaultControversialAction
	}
	if err := semreview.ValidateConfig(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// A named deep channel must exist: a dangling reference would
	// fail every confidential call closed once the layer is on.
	if cfg.DeepChannelID != "" {
		found := false
		a.st.Read(func(st *store.State) {
			_, found = st.Channels[cfg.DeepChannelID]
		})
		if !found {
			writeErr(w, http.StatusBadRequest, "deep_channel_id does not name a channel")
			return
		}
	}
	a.st.WithLock(func(st *store.State) { st.SemanticReview = cfg })
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot persist semantic review policy")
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "semantic_review_policy_updated",
		Detail: map[string]string{"enabled": boolStr(cfg.Enabled), "deep_enabled": boolStr(cfg.DeepEnabled)}})
	writeJSON(w, http.StatusOK, map[string]any{"enabled": cfg.Enabled, "threshold": cfg.Threshold,
		"timeout_ms": cfg.TimeoutMS, "controversial_action": cfg.ControversialAction,
		"deep_enabled": cfg.DeepEnabled, "deep_channel_id": cfg.DeepChannelID, "deep_model": cfg.DeepModel})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (a *API) addSemanticSample(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Category string `json:"category"`
		Text     string `json:"text"`
		Action   string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := semreview.ValidateSample(in.Category, in.Text, in.Action); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, _ := a.effectiveSemReview()
	s := store.SemanticReviewSample{ID: store.NewID("sr"), Category: in.Category,
		Text: in.Text, Action: in.Action, CreatedAt: time.Now().UTC()}
	cfg.Samples = append(cfg.Samples, s)
	a.st.WithLock(func(st *store.State) { st.SemanticReview = cfg })
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot persist semantic review sample")
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "semantic_review_sample_added",
		Detail: map[string]string{"sample_id": s.ID, "category": s.Category, "action": s.Action}})
	writeJSON(w, http.StatusCreated, s)
}

func (a *API) deleteSemanticSample(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	cfg, _ := a.effectiveSemReview()
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
	a.st.WithLock(func(st *store.State) { st.SemanticReview = cfg })
	if err := a.st.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot persist semantic review sample")
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "semantic_review_sample_deleted",
		Detail: map[string]string{"sample_id": removed.ID, "category": removed.Category}})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
