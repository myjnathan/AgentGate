// Package api — plugin policy (F38, slice 1).
//
// GET/POST /api/plugin/policies and GET/PUT/DELETE
// /api/plugin/policies/{id} manage the standalone PluginPolicy
// objects: mode (allowlist/denylist), scope (global/department/
// group), triple rules (ID + version constraint + content hash +
// source), disposition and the unknown-plugin alarm. Only admins
// may manage them; the evaluation itself runs at login approval
// and on every heartbeat, so a policy change takes effect on the
// next approval or heartbeat without a restart.
package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/pluginpol"
	"github.com/myjnathan/agentgate/internal/store"
)

// listPluginPolicies handles GET /api/plugin/policies.
func (a *API) listPluginPolicies(w http.ResponseWriter, r *http.Request, u *store.User) {
	var out []*pluginpol.PluginPolicy
	a.st.Read(func(st *store.State) {
		for _, p := range st.PluginPolicies {
			cp := *p
			out = append(out, &cp)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if out == nil {
		out = []*pluginpol.PluginPolicy{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out})
}

// getPluginPolicy handles GET /api/plugin/policies/{id}.
func (a *API) getPluginPolicy(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := strings.TrimSpace(r.PathValue("id"))
	var found *pluginpol.PluginPolicy
	a.st.Read(func(st *store.State) {
		if p, ok := st.PluginPolicies[id]; ok && p != nil {
			cp := *p
			found = &cp
		}
	})
	if found == nil {
		writeErr(w, http.StatusNotFound, "no such plugin policy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": found})
}

// createPluginPolicy handles POST /api/plugin/policies. The ID is
// server-assigned unless the client supplies one; a supplied ID
// that already exists is a 409.
func (a *API) createPluginPolicy(w http.ResponseWriter, r *http.Request, u *store.User) {
	var p pluginpol.PluginPolicy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	now := time.Now()
	if p.ID == "" {
		p.ID = store.NewID("plg")
	}
	p.CreatedAt = now
	p.UpdatedAt = now
	if err := p.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var conflict bool
	a.st.WithLock(func(st *store.State) {
		if _, ok := st.PluginPolicies[p.ID]; ok {
			conflict = true
			return
		}
		cp := p
		st.PluginPolicies[p.ID] = &cp
	})
	if conflict {
		writeErr(w, http.StatusConflict, "plugin policy already exists")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID,
		Kind: "admin", Action: "plugin_policy_create", Decision: "allow",
		Detail: map[string]string{"policy_id": p.ID, "name": p.Name, "mode": p.Mode}})
	writeJSON(w, http.StatusCreated, map[string]any{"policy": p})
}

// updatePluginPolicy handles PUT /api/plugin/policies/{id}: a full
// replacement (the path ID wins); CreatedAt is preserved.
func (a *API) updatePluginPolicy(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := strings.TrimSpace(r.PathValue("id"))
	var p pluginpol.PluginPolicy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	p.ID = id
	var created time.Time
	var found bool
	a.st.Read(func(st *store.State) {
		if old, ok := st.PluginPolicies[id]; ok && old != nil {
			created = old.CreatedAt
			found = true
		}
	})
	if !found {
		writeErr(w, http.StatusNotFound, "no such plugin policy")
		return
	}
	p.CreatedAt = created
	p.UpdatedAt = time.Now()
	if err := p.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.st.WithLock(func(st *store.State) {
		cp := p
		st.PluginPolicies[id] = &cp
	})
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID,
		Kind: "admin", Action: "plugin_policy_update", Decision: "allow",
		Detail: map[string]string{"policy_id": p.ID, "name": p.Name, "mode": p.Mode}})
	writeJSON(w, http.StatusOK, map[string]any{"policy": p})
}

// listPluginAssets handles GET /api/plugin/assets (F38, slice 2):
// the network-wide plugin inventory aggregated from the latest
// heartbeat of every terminal. Filters: status (violating | unknown
// | unverified | compliant), department (only installs in that
// department are counted), q (substring on the plugin ID). The rows
// reconcile with the heartbeat evidence by construction — the
// aggregation reads the same Terminal records the heartbeats wrote.
func (a *API) listPluginAssets(w http.ResponseWriter, r *http.Request, u *store.User) {
	q := r.URL.Query()
	status := q.Get("status")
	switch status {
	case "", pluginpol.StatusViolating, pluginpol.StatusUnknown,
		pluginpol.StatusUnverified, pluginpol.StatusCompliant:
	default:
		writeErr(w, http.StatusBadRequest, "invalid status (violating|unknown|unverified|compliant)")
		return
	}
	dept := q.Get("department")
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))
	now := time.Now()
	var rows []pluginpol.AssetRow
	a.st.Read(func(st *store.State) {
		terms := make([]pluginpol.AssetTerminal, 0, len(st.Terminals))
		for _, t := range st.Terminals {
			if dept != "" && !strings.EqualFold(t.Department, dept) {
				continue
			}
			terms = append(terms, pluginpol.AssetTerminal{
				ID: t.ID, Username: t.Username, Department: t.Department,
				AgentType: t.AgentType, Details: t.PluginDetails, Names: t.Plugins,
			})
		}
		rows = pluginpol.Aggregate(terms, st.PluginPolicies, now)
	})
	if rows == nil {
		rows = []pluginpol.AssetRow{}
	}
	if status != "" || needle != "" {
		filtered := rows[:0]
		for _, row := range rows {
			if status != "" && row.Status != status {
				continue
			}
			if needle != "" && !strings.Contains(strings.ToLower(row.ID), needle) {
				continue
			}
			filtered = append(filtered, row)
		}
		rows = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": rows, "total": len(rows)})
}

// deletePluginPolicy handles DELETE /api/plugin/policies/{id}.
func (a *API) deletePluginPolicy(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := strings.TrimSpace(r.PathValue("id"))
	var ok bool
	var name string
	a.st.WithLock(func(st *store.State) {
		if p, found := st.PluginPolicies[id]; found && p != nil {
			name = p.Name
			delete(st.PluginPolicies, id)
			ok = true
		}
	})
	if !ok {
		writeErr(w, http.StatusNotFound, "no such plugin policy")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID,
		Kind: "admin", Action: "plugin_policy_delete", Decision: "allow",
		Detail: map[string]string{"policy_id": id, "name": name}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}
