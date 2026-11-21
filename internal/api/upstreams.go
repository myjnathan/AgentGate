// Package api — federated upstream MCP servers (F6).
//
// GET /api/mcp/upstreams lists registered upstreams (admin/security
// only); POST registers one; DELETE removes one; POST
// /api/mcp/upstreams/{id}/test lists the upstream's tools live to
// verify connectivity. Upstream bearer tokens are encrypted at rest
// with the master key and never returned; the list only reports
// whether one is configured.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/federation"
	"github.com/myjnathan/agentgate/internal/store"
)

// upstreamView is the admin-facing upstream shape: metadata only,
// never the credential.
type upstreamView struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	URL          string          `json:"url"`
	HasToken     bool            `json:"has_token"`
	MaxDataLevel store.DataLevel `json:"max_data_level"`
	MinTier      store.Tier      `json:"min_tier"`
	Enabled      bool            `json:"enabled"`
	CreatedAt    time.Time       `json:"created_at"`
}

func toUpstreamView(up *store.UpstreamServer) upstreamView {
	return upstreamView{ID: up.ID, Name: up.Name, URL: up.URL, HasToken: up.HasToken,
		MaxDataLevel: up.MaxDataLevel, MinTier: up.MinTier, Enabled: up.Enabled, CreatedAt: up.CreatedAt}
}

// validNamespace: lowercase letters, digits and hyphens, starting
// with a letter — the namespace becomes the "<name>.<tool>" prefix.
var validNamespace = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func (a *API) listUpstreams(w http.ResponseWriter, r *http.Request, u *store.User) {
	var out []upstreamView
	a.st.Read(func(st *store.State) {
		for _, up := range st.Upstreams {
			out = append(out, toUpstreamView(up))
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{"upstreams": out})
}

func (a *API) createUpstream(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Name         string `json:"name"`
		URL          string `json:"url"`
		Token        string `json:"token"`
		MaxDataLevel string `json:"max_data_level"`
		MinTier      string `json:"min_tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.URL == "" {
		writeErr(w, http.StatusBadRequest, "name and url required")
		return
	}
	if !validNamespace.MatchString(in.Name) {
		writeErr(w, http.StatusBadRequest, "name must be a lowercase namespace (e.g. hr)")
		return
	}
	if !strings.HasPrefix(in.URL, "https://") && !strings.HasPrefix(in.URL, "http://127.0.0.1") && !strings.HasPrefix(in.URL, "http://localhost") {
		writeErr(w, http.StatusBadRequest, "url must be https (or loopback http for testing)")
		return
	}
	lvl, ok := store.ParseDataLevel(in.MaxDataLevel)
	if in.MaxDataLevel != "" && !ok {
		writeErr(w, http.StatusBadRequest, "max_data_level must be public, internal, confidential or secret")
		return
	}
	if in.MaxDataLevel == "" {
		lvl = store.LevelInternal
	}
	tier := store.TierBYOA
	if in.MinTier != "" {
		if in.MinTier != "byoa" && in.MinTier != "managed" {
			writeErr(w, http.StatusBadRequest, "min_tier must be byoa or managed")
			return
		}
		tier = store.Tier(in.MinTier)
	}
	duplicate := false
	a.st.Read(func(st *store.State) {
		for _, up := range st.Upstreams {
			if up.Name == in.Name {
				duplicate = true
			}
		}
	})
	if duplicate {
		writeErr(w, http.StatusConflict, "an upstream with this name already exists")
		return
	}
	enc := ""
	if in.Token != "" {
		var err error
		enc, err = a.box.Encrypt(in.Token)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "encrypt failed")
			return
		}
	}
	up := &store.UpstreamServer{ID: store.NewID("up"), Name: in.Name, URL: in.URL,
		AuthTokenEnc: enc, HasToken: enc != "", MaxDataLevel: lvl, MinTier: tier,
		Enabled: true, CreatedAt: time.Now()}
	a.st.WithLock(func(st *store.State) { st.Upstreams[up.ID] = up })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "upstream_created",
		Detail: map[string]string{"upstream": up.Name, "max_data_level": lvl.String(), "min_tier": string(tier)}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": up.ID, "has_token": up.HasToken})
}

func (a *API) deleteUpstream(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var name string
	found := false
	a.st.WithLock(func(st *store.State) {
		if up, ok := st.Upstreams[id]; ok {
			name = up.Name
			delete(st.Upstreams, id)
			found = true
		}
	})
	if !found {
		writeErr(w, http.StatusNotFound, "upstream not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "upstream_deleted",
		Detail: map[string]string{"upstream": name}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func (a *API) testUpstream(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var up *store.UpstreamServer
	a.st.Read(func(st *store.State) {
		if x, ok := st.Upstreams[id]; ok {
			cp := *x
			up = &cp
		}
	})
	if up == nil {
		writeErr(w, http.StatusNotFound, "upstream not found")
		return
	}
	if a.fed == nil {
		writeErr(w, http.StatusServiceUnavailable, "federation not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tools, err := a.fed.ListToolsFresh(ctx, up)
	detail := map[string]string{"upstream": up.Name,
		"result": map[bool]string{true: "ok", false: "failed"}[err == nil]}
	if err != nil {
		detail["error"] = "upstream_error"
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "upstream_tested",
		Detail: detail})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "upstream_error"})
		return
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, up.Name+"."+t.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tools": names})
}

// SetFederation wires the federation registry used by the upstream
// test endpoint.
func (a *API) SetFederation(f *federation.Registry) {
	a.fed = f
}
