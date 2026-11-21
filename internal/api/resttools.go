// Package api — REST → MCP adapter tools (F7).
//
// GET /api/mcp/rest-tools lists registered REST tools (admin/security
// only); POST registers one (method + base URL + path template +
// input schema + hosted credential); DELETE removes one; POST
// /api/mcp/rest-tools/{id}/test performs a real call with
// admin-supplied sample arguments and reports the status plus a short
// preview. Credentials are encrypted at rest with the master key and
// never returned; the list only reports whether one is configured.
// The test preview goes to the admin caller only and is never written
// to the audit log.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/restadapter"
	"github.com/myjnathan/agentgate/internal/store"
)

// restToolView is the admin-facing REST tool shape: configuration
// metadata only, never the credential.
type restToolView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	BaseURL      string            `json:"base_url"`
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	Headers      map[string]string `json:"headers,omitempty"`
	HasToken     bool              `json:"has_token"`
	InputSchema  json.RawMessage   `json:"input_schema"`
	MaxDataLevel store.DataLevel   `json:"max_data_level"`
	MinTier      store.Tier        `json:"min_tier"`
	Enabled      bool              `json:"enabled"`
	CreatedAt    time.Time         `json:"created_at"`
}

func toRESTToolView(t *store.RESTTool) restToolView {
	return restToolView{ID: t.ID, Name: t.Name, Description: t.Description,
		BaseURL: t.BaseURL, Method: t.Method, Path: t.Path, Headers: t.Headers,
		HasToken: t.HasToken, InputSchema: t.InputSchema, MaxDataLevel: t.MaxDataLevel,
		MinTier: t.MinTier, Enabled: t.Enabled, CreatedAt: t.CreatedAt}
}

func (a *API) listRESTTools(w http.ResponseWriter, r *http.Request, u *store.User) {
	var out []restToolView
	a.st.Read(func(st *store.State) {
		for _, t := range st.RESTTools {
			out = append(out, toRESTToolView(t))
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{"rest_tools": out})
}

func (a *API) createRESTTool(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Name         string            `json:"name"`
		Description  string            `json:"description"`
		BaseURL      string            `json:"base_url"`
		Method       string            `json:"method"`
		Path         string            `json:"path"`
		Headers      map[string]string `json:"headers"`
		Token        string            `json:"token"`
		InputSchema  json.RawMessage   `json:"input_schema"`
		MaxDataLevel string            `json:"max_data_level"`
		MinTier      string            `json:"min_tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.BaseURL == "" || in.Path == "" {
		writeErr(w, http.StatusBadRequest, "name, base_url and path required")
		return
	}
	if !restadapter.ValidToolName(in.Name) {
		writeErr(w, http.StatusBadRequest, "name must be lowercase snake_case (e.g. leave_balance)")
		return
	}
	if !strings.HasPrefix(in.BaseURL, "https://") && !strings.HasPrefix(in.BaseURL, "http://127.0.0.1") && !strings.HasPrefix(in.BaseURL, "http://localhost") {
		writeErr(w, http.StatusBadRequest, "base_url must be https (or loopback http for testing)")
		return
	}
	if in.Method == "" {
		in.Method = "GET"
	}
	if !restadapter.ValidMethod(in.Method) {
		writeErr(w, http.StatusBadRequest, "method must be GET, POST, PUT, PATCH or DELETE")
		return
	}
	if !strings.HasPrefix(in.Path, "/") {
		writeErr(w, http.StatusBadRequest, "path must start with /")
		return
	}
	if err := restadapter.ValidateSchema(in.InputSchema); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	for k := range in.Headers {
		if strings.TrimSpace(k) == "" {
			writeErr(w, http.StatusBadRequest, "header names must not be empty")
			return
		}
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
		for _, t := range st.RESTTools {
			if t.Name == in.Name {
				duplicate = true
			}
		}
	})
	if duplicate {
		writeErr(w, http.StatusConflict, "a REST tool with this name already exists")
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
	tool := &store.RESTTool{ID: store.NewID("rt"), Name: in.Name, Description: in.Description,
		BaseURL: in.BaseURL, Method: strings.ToUpper(in.Method), Path: in.Path,
		Headers: in.Headers, AuthTokenEnc: enc, HasToken: enc != "",
		InputSchema: in.InputSchema, MaxDataLevel: lvl, MinTier: tier,
		Enabled: true, CreatedAt: time.Now()}
	a.st.WithLock(func(st *store.State) { st.RESTTools[tool.ID] = tool })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "rest_tool_created",
		Detail: map[string]string{"tool": tool.Name, "method": tool.Method,
			"max_data_level": lvl.String(), "min_tier": string(tier)}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": tool.ID, "has_token": tool.HasToken})
}

func (a *API) deleteRESTTool(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var name string
	found := false
	a.st.WithLock(func(st *store.State) {
		if t, ok := st.RESTTools[id]; ok {
			name = t.Name
			delete(st.RESTTools, id)
			found = true
		}
	})
	if !found {
		writeErr(w, http.StatusNotFound, "REST tool not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "rest_tool_deleted",
		Detail: map[string]string{"tool": name}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func (a *API) testRESTTool(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var tool *store.RESTTool
	a.st.Read(func(st *store.State) {
		if x, ok := st.RESTTools[id]; ok {
			cp := *x
			tool = &cp
		}
	})
	if tool == nil {
		writeErr(w, http.StatusNotFound, "REST tool not found")
		return
	}
	if a.restAd == nil {
		writeErr(w, http.StatusServiceUnavailable, "REST adapter not configured")
		return
	}
	var in struct {
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	text, isErr, err := a.restAd.Call(ctx, tool, in.Arguments)
	detail := map[string]string{"tool": tool.Name,
		"result": map[bool]string{true: "ok", false: "failed"}[err == nil && !isErr]}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "rest_tool_tested",
		Detail: detail})
	if err != nil {
		if errors.Is(err, restadapter.ErrValidation) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "upstream_error"})
		return
	}
	preview := text
	if len(preview) > 200 {
		preview = preview[:200]
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": !isErr, "is_error": isErr, "preview": preview})
}

// SetRESTAdapter wires the REST adapter used by the REST tool test
// endpoint.
func (a *API) SetRESTAdapter(ad *restadapter.Adapter) {
	a.restAd = ad
}
