// Package api — prompt templates (F8).
//
// A prompt template is a versioned text template in the capability
// hub, following the same pipeline as skills (submit → review →
// publish) and Portkey's version semantics: an update only creates a
// new immutable version, and a separate publish step marks one
// version as the production version. Agents only ever receive the
// published version, via /mcp prompts/get.
//
// GET /api/prompts lists templates visible to the caller
// (admin/security see all; others see published templates in their
// department scope, plus their own drafts). POST creates a template
// (version 1, status in_review). POST /api/prompts/{id}/versions
// appends a new version (admin or author; the published version keeps
// serving until an admin publishes the new one). POST
// /api/prompts/{id}/review approves (publishes the current version on
// first publish) or rejects. POST /api/prompts/{id}/publish publishes
// (or rolls back to) a specific version. DELETE removes a template.
// Template content is admin-authored text, never a secret.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// promptVersionView is the admin-facing version shape: content and
// declared arguments only.
type promptVersionView struct {
	Version   int                    `json:"version"`
	Content   string                 `json:"content"`
	Arguments []store.PromptArgument `json:"arguments"`
	CreatedAt time.Time              `json:"created_at"`
	CreatedBy string                 `json:"created_by"`
}

// promptView is the admin-facing template shape.
type promptView struct {
	ID               string              `json:"id"`
	Name             string              `json:"name"`
	Description      string              `json:"description"`
	Scope            string              `json:"scope"`
	MaxDataLevel     store.DataLevel     `json:"max_data_level"`
	MinTier          store.Tier          `json:"min_tier"`
	Status           string              `json:"status"`
	CurrentVersion   int                 `json:"current_version"`
	PublishedVersion int                 `json:"published_version"`
	Versions         []promptVersionView `json:"versions"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

func toPromptView(t *store.PromptTemplate) promptView {
	v := promptView{ID: t.ID, Name: t.Name, Description: t.Description,
		Scope: t.Scope, MaxDataLevel: t.MaxDataLevel, MinTier: t.MinTier,
		Status: t.Status, CurrentVersion: t.CurrentVersion,
		PublishedVersion: t.PublishedVersion,
		CreatedAt:        t.CreatedAt, UpdatedAt: t.UpdatedAt}
	for _, ver := range t.Versions {
		v.Versions = append(v.Versions, promptVersionView{
			Version: ver.Version, Content: ver.Content,
			Arguments: ver.Arguments, CreatedAt: ver.CreatedAt,
			CreatedBy: ver.CreatedBy})
	}
	return v
}

// visiblePrompt mirrors the skills list filter: non-admins see only
// published templates in their department scope, plus their own
// drafts (so an author can follow their submission through review).
func visiblePrompt(u *store.User, t *store.PromptTemplate) bool {
	if t.Status != "published" && u.Role != "admin" && u.Role != "security" && t.AuthorID != u.ID {
		return false
	}
	if t.Scope != "*" && u.Role != "admin" && u.Role != "security" {
		if !t.InScope(u.Department) {
			return false
		}
	}
	return true
}

func (a *API) listPrompts(w http.ResponseWriter, r *http.Request, u *store.User) {
	var out []promptView
	a.st.Read(func(st *store.State) {
		for _, t := range st.Prompts {
			if !visiblePrompt(u, t) {
				continue
			}
			out = append(out, toPromptView(t))
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{"prompts": out})
}

func (a *API) getPrompt(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var tpl *store.PromptTemplate
	a.st.Read(func(st *store.State) {
		if t, ok := st.Prompts[id]; ok {
			cp := *t
			tpl = &cp
		}
	})
	if tpl == nil || !visiblePrompt(u, tpl) {
		writeErr(w, http.StatusNotFound, "prompt not found")
		return
	}
	writeJSON(w, http.StatusOK, toPromptView(tpl))
}

type promptInput struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	Scope        string                 `json:"scope"`
	MaxDataLevel string                 `json:"max_data_level"`
	MinTier      string                 `json:"min_tier"`
	Content      string                 `json:"content"`
	Arguments    []store.PromptArgument `json:"arguments"`
}

func (a *API) createPrompt(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in promptInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.Content == "" {
		writeErr(w, http.StatusBadRequest, "name and content required")
		return
	}
	if !store.ValidPromptName(in.Name) {
		writeErr(w, http.StatusBadRequest, "name must be lowercase snake_case (e.g. weekly_report)")
		return
	}
	if err := store.ValidateTemplateVersion(in.Content, in.Arguments); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
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
	if in.Scope == "" {
		in.Scope = u.Department
	}
	duplicate := false
	a.st.Read(func(st *store.State) {
		for _, t := range st.Prompts {
			if t.Name == in.Name {
				duplicate = true
			}
		}
	})
	if duplicate {
		writeErr(w, http.StatusConflict, "a prompt template with this name already exists")
		return
	}
	now := time.Now()
	tpl := &store.PromptTemplate{ID: store.NewID("pt"), Name: in.Name,
		Description: in.Description, Scope: in.Scope, MaxDataLevel: lvl,
		MinTier: tier, Status: "in_review", CurrentVersion: 1,
		Versions: []store.PromptVersion{{Version: 1, Content: in.Content,
			Arguments: in.Arguments, CreatedAt: now, CreatedBy: u.ID}},
		AuthorID: u.ID, CreatedAt: now, UpdatedAt: now}
	a.st.WithLock(func(st *store.State) { st.Prompts[tpl.ID] = tpl })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "prompt_created",
		Detail: map[string]string{"prompt": tpl.Name, "version": "1"}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": tpl.ID, "status": tpl.Status})
}

// addPromptVersion appends a new immutable version (admin or author
// only). The published version keeps serving until an admin
// explicitly publishes the new one — an update is never a silent
// production change.
func (a *API) addPromptVersion(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var in struct {
		Content   string                 `json:"content"`
		Arguments []store.PromptArgument `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Content == "" {
		writeErr(w, http.StatusBadRequest, "content required")
		return
	}
	if err := store.ValidateTemplateVersion(in.Content, in.Arguments); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var version int
	var found, allowed bool
	a.st.WithLock(func(st *store.State) {
		t, ok := st.Prompts[id]
		if !ok {
			return
		}
		found = true
		if u.Role != "admin" && u.Role != "security" && t.AuthorID != u.ID {
			return
		}
		allowed = true
		t.CurrentVersion++
		version = t.CurrentVersion
		t.Versions = append(t.Versions, store.PromptVersion{
			Version: version, Content: in.Content, Arguments: in.Arguments,
			CreatedAt: time.Now(), CreatedBy: u.ID})
		t.UpdatedAt = time.Now()
	})
	if !found {
		writeErr(w, http.StatusNotFound, "prompt not found")
		return
	}
	if !allowed {
		writeErr(w, http.StatusForbidden, "only the author or an admin may add a version")
		return
	}
	_ = a.st.Save()
	var name string
	a.st.Read(func(st *store.State) { name = st.Prompts[id].Name })
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "prompt_version_added",
		Detail: map[string]string{"prompt": name, "version": strconv.Itoa(version)}})
	writeJSON(w, http.StatusCreated, map[string]any{"version": version})
}

// reviewPrompt approves (status published; on first publish the
// current version becomes the published version) or rejects a
// template. Rejecting a published template withdraws it from serving:
// prompts/get requires status published.
func (a *API) reviewPrompt(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var in struct {
		Approve bool `json:"approve"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var status, name string
	var published int
	a.st.WithLock(func(st *store.State) {
		t, ok := st.Prompts[id]
		if !ok {
			return
		}
		name = t.Name
		if in.Approve {
			t.Status = "published"
			if t.PublishedVersion == 0 {
				t.PublishedVersion = t.CurrentVersion
			}
		} else {
			t.Status = "rejected"
		}
		t.UpdatedAt = time.Now()
		status = t.Status
		published = t.PublishedVersion
	})
	if status == "" {
		writeErr(w, http.StatusNotFound, "prompt not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "prompt_reviewed",
		Detail: map[string]string{"prompt": name, "result": status}})
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "published_version": published})
}

// publishPrompt publishes (or rolls back to) a specific version: the
// version must exist, and publishing is always an explicit admin act.
func (a *API) publishPrompt(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var in struct {
		Version int `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Version < 1 {
		writeErr(w, http.StatusBadRequest, "version required")
		return
	}
	var name string
	var found bool
	a.st.WithLock(func(st *store.State) {
		t, ok := st.Prompts[id]
		if !ok {
			return
		}
		found = true
		if t.Version(in.Version) == nil {
			return
		}
		name = t.Name
		t.PublishedVersion = in.Version
		t.Status = "published"
		t.UpdatedAt = time.Now()
	})
	if !found {
		writeErr(w, http.StatusNotFound, "prompt not found")
		return
	}
	if name == "" {
		writeErr(w, http.StatusBadRequest, "version does not exist")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "prompt_published",
		Detail: map[string]string{"prompt": name, "version": strconv.Itoa(in.Version)}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "published", "published_version": in.Version})
}

func (a *API) deletePrompt(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var name string
	found := false
	a.st.WithLock(func(st *store.State) {
		if t, ok := st.Prompts[id]; ok {
			name = t.Name
			delete(st.Prompts, id)
			found = true
		}
	})
	if !found {
		writeErr(w, http.StatusNotFound, "prompt not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "prompt_deleted",
		Detail: map[string]string{"prompt": name}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}
