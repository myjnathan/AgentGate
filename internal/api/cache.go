package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/cache"
	"github.com/myjnathan/agentgate/internal/store"
)

// Response-cache management (backlog F11). The cache itself lives
// in the gateway; these endpoints only expose its status and the
// admin controls (toggle, clear). All three require admin/security.

// SetCache wires the gateway's response cache into the API.
func (a *API) SetCache(c *cache.Cache) { a.cache = c }

func (a *API) getCache(w http.ResponseWriter, r *http.Request, u *store.User) {
	if a.cache == nil {
		writeErr(w, http.StatusServiceUnavailable, "cache not configured")
		return
	}
	writeJSON(w, http.StatusOK, a.cache.Stats())
}

func (a *API) setCache(w http.ResponseWriter, r *http.Request, u *store.User) {
	if a.cache == nil {
		writeErr(w, http.StatusServiceUnavailable, "cache not configured")
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "enabled is required")
		return
	}
	a.cache.SetEnabled(*in.Enabled)
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "cache_toggled",
		Detail: map[string]string{"enabled": strconv.FormatBool(*in.Enabled)}})
	writeJSON(w, http.StatusOK, a.cache.Stats())
}

func (a *API) clearCache(w http.ResponseWriter, r *http.Request, u *store.User) {
	if a.cache == nil {
		writeErr(w, http.StatusServiceUnavailable, "cache not configured")
		return
	}
	cleared := a.cache.Clear()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "cache_cleared",
		Detail: map[string]string{"cleared": strconv.Itoa(cleared)}})
	writeJSON(w, http.StatusOK, map[string]any{"cleared": cleared})
}
