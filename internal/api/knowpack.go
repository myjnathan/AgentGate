// Package api — curated knowledge packages (F22).
//
// A knowledge package is a curated, versioned synthesis over folder
// documents: an administrator or department expert selects sources
// (the recipe), writes the synthesis (purpose / evidence usage /
// boundaries + conclusions, every conclusion backed by a citation),
// and submits it for review. Versions are append-only like prompt
// templates (F8); a separate publish step marks one version as the
// production version, and agents only ever read the published
// version via the MCP kb_pack_list / kb_pack_get tools.
//
// GET /api/kb/packages lists packages visible to the caller
// (admin/security see all; others see published packages in their
// department scope, plus their own drafts). POST creates a package
// (version 1, status in_review). POST /api/kb/packages/{id}/versions
// appends a new version (admin or author). POST
// /api/kb/packages/{id}/review approves (publishes the current
// version on first publish) or rejects. POST
// /api/kb/packages/{id}/publish publishes (or rolls back to) a
// specific version after re-validating its recipe against the live
// folder index and its references. DELETE removes a package.
// Every mutation is an audit event (the change receipt); the audit
// detail carries names, versions and levels only — never synthesis
// text, which may itself summarise classified documents.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/knowpack"
	"github.com/myjnathan/agentgate/internal/store"
)

// errFolderIndexUnavailable is returned when a package operation
// needs the folder index and it is not wired.
var errFolderIndexUnavailable = errors.New("folder knowledge base not available")

// packageVersionView is the admin-facing version shape: recipe,
// synthesis and the citations with their tree-filled back-reference.
type packageVersionView struct {
	Version    int                     `json:"version"`
	Content    string                  `json:"content"`
	Citations  []store.PackageCitation `json:"citations"`
	Sources    []store.PackageSource   `json:"sources"`
	References []string                `json:"references,omitempty"`
	ChangeNote string                  `json:"change_note,omitempty"`
	Level      string                  `json:"level"`
	CreatedAt  string                  `json:"created_at"`
	CreatedBy  string                  `json:"created_by"`
}

// knowpackView is the admin-facing package shape. Stale is true when
// the current version's recipe no longer matches the live index
// (a source document vanished) or one of its references would be
// withheld at read time (withdrawn or republished higher); the
// curator revises the recipe with a new version.
type knowpackView struct {
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	Description      string               `json:"description"`
	Scope            string               `json:"scope"`
	Status           string               `json:"status"`
	Level            string               `json:"level"`
	CurrentVersion   int                  `json:"current_version"`
	PublishedVersion int                  `json:"published_version"`
	Stale            bool                 `json:"stale"`
	Versions         []packageVersionView `json:"versions"`
	CreatedAt        string               `json:"created_at"`
	UpdatedAt        string               `json:"updated_at"`
}

func toKnowpackView(p *store.KnowledgePackage, stale bool) knowpackView {
	v := knowpackView{ID: p.ID, Name: p.Name, Description: p.Description,
		Scope: p.Scope, Status: p.Status, Level: p.Level(),
		CurrentVersion: p.CurrentVersion, PublishedVersion: p.PublishedVersion,
		Stale:     stale,
		CreatedAt: p.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: p.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")}
	for _, ver := range p.Versions {
		v.Versions = append(v.Versions, packageVersionView{
			Version: ver.Version, Content: ver.Content,
			Citations: ver.Citations, Sources: ver.Sources,
			References: ver.References, ChangeNote: ver.ChangeNote,
			Level:     ver.Level,
			CreatedAt: ver.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			CreatedBy: ver.CreatedBy})
	}
	return v
}

// packageStale checks the package's current version against the live
// index and store. It is best-effort: when the folder index is not
// wired (unit tests that only exercise other endpoints) the package
// is reported not stale.
func (a *API) packageStale(p *store.KnowledgePackage) bool {
	ver := p.Version(p.CurrentVersion)
	if ver == nil {
		return false
	}
	var pkgs map[string]*store.KnowledgePackage
	a.st.Read(func(st *store.State) { pkgs = st.KnowledgePackages })
	if len(knowpack.CheckReferences(ver, pkgs)) > 0 {
		return true
	}
	if a.fk == nil {
		return false
	}
	return len(knowpack.CheckSources(ver, a.fk.Snapshots())) > 0
}

// visiblePackage mirrors the prompts list filter: non-admins see only
// published packages in their department scope, plus their own
// drafts (so an author can follow a submission through review).
func visiblePackage(u *store.User, p *store.KnowledgePackage) bool {
	if p.Status != "published" && u.Role != "admin" && u.Role != "security" && p.AuthorID != u.ID {
		return false
	}
	if p.Scope != "*" && u.Role != "admin" && u.Role != "security" {
		if !p.InScope(u.Department) {
			return false
		}
	}
	return true
}

func (a *API) listKnowledgePackages(w http.ResponseWriter, r *http.Request, u *store.User) {
	var out []knowpackView
	a.st.Read(func(st *store.State) {
		for _, p := range st.KnowledgePackages {
			if !visiblePackage(u, p) {
				continue
			}
			out = append(out, toKnowpackView(p, a.packageStale(p)))
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{"packages": out})
}

func (a *API) getKnowledgePackage(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var pkg *store.KnowledgePackage
	a.st.Read(func(st *store.State) {
		if p, ok := st.KnowledgePackages[id]; ok {
			cp := *p
			pkg = &cp
		}
	})
	if pkg == nil || !visiblePackage(u, pkg) {
		writeErr(w, http.StatusNotFound, "package not found")
		return
	}
	writeJSON(w, http.StatusOK, toKnowpackView(pkg, a.packageStale(pkg)))
}

type packageInput struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	Scope       string                  `json:"scope"`
	Content     string                  `json:"content"`
	Citations   []store.PackageCitation `json:"citations"`
	Sources     []store.PackageSource   `json:"sources"`
	References  []string                `json:"references"`
	ChangeNote  string                  `json:"change_note"`
}

// resolvePackageVersion validates a version's shape, then resolves it
// against the live folder index: sources must exist, references must
// be published and downward-only, and every citation is filled in
// from the section tree. It returns the computed level and the filled
// citations.
func (a *API) resolvePackageVersion(selfID string, in packageInput) (store.DataLevel, []store.PackageCitation, error) {
	if err := store.ValidatePackageVersion(in.Content, in.Citations, in.Sources, in.References, in.ChangeNote); err != nil {
		return 0, nil, err
	}
	if a.fk == nil {
		return 0, nil, errFolderIndexUnavailable
	}
	var pkgs map[string]*store.KnowledgePackage
	a.st.Read(func(st *store.State) { pkgs = st.KnowledgePackages })
	return knowpack.ResolveVersion(selfID, in.Sources, in.References, in.Citations, a.fk.Snapshots(), pkgs)
}

func (a *API) createKnowledgePackage(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in packageInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.Content == "" {
		writeErr(w, http.StatusBadRequest, "name and content required")
		return
	}
	if !store.ValidPromptName(in.Name) {
		writeErr(w, http.StatusBadRequest, "name must be lowercase snake_case (e.g. onboarding_essentials)")
		return
	}
	if in.Scope == "" {
		in.Scope = u.Department
	}
	duplicate := false
	a.st.Read(func(st *store.State) {
		for _, p := range st.KnowledgePackages {
			if p.Name == in.Name {
				duplicate = true
			}
		}
	})
	if duplicate {
		writeErr(w, http.StatusConflict, "a knowledge package with this name already exists")
		return
	}
	lvl, filled, err := a.resolvePackageVersion("", in)
	if err != nil {
		if err == errFolderIndexUnavailable {
			writeErr(w, http.StatusServiceUnavailable, "folder knowledge base not available")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now()
	pkg := &store.KnowledgePackage{ID: store.NewID("kp"), Name: in.Name,
		Description: in.Description, Scope: in.Scope, Status: "in_review",
		CurrentVersion: 1,
		Versions: []store.PackageVersion{{Version: 1, Content: in.Content,
			Citations: filled, Sources: in.Sources, References: in.References,
			ChangeNote: in.ChangeNote, Level: lvl.String(),
			CreatedAt: now, CreatedBy: u.ID}},
		AuthorID: u.ID, CreatedAt: now, UpdatedAt: now}
	a.st.WithLock(func(st *store.State) { st.KnowledgePackages[pkg.ID] = pkg })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "package_created",
		Detail: map[string]string{"package": pkg.Name, "version": "1", "level": lvl.String()}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": pkg.ID, "status": pkg.Status, "level": lvl.String()})
}

// addKnowledgePackageVersion appends a new immutable version (admin
// or author only). The published version keeps serving until an admin
// explicitly publishes the new one — an update is never a silent
// production change.
func (a *API) addKnowledgePackageVersion(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var in packageInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Content == "" {
		writeErr(w, http.StatusBadRequest, "content required")
		return
	}
	var found, allowed bool
	a.st.Read(func(st *store.State) {
		p, ok := st.KnowledgePackages[id]
		if !ok {
			return
		}
		found = true
		if u.Role == "admin" || u.Role == "security" || p.AuthorID == u.ID {
			allowed = true
		}
	})
	if !found {
		writeErr(w, http.StatusNotFound, "package not found")
		return
	}
	if !allowed {
		writeErr(w, http.StatusForbidden, "only the author or an admin may add a version")
		return
	}
	lvl, filled, err := a.resolvePackageVersion(id, in)
	if err != nil {
		if err == errFolderIndexUnavailable {
			writeErr(w, http.StatusServiceUnavailable, "folder knowledge base not available")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var version int
	a.st.WithLock(func(st *store.State) {
		p := st.KnowledgePackages[id]
		p.CurrentVersion++
		version = p.CurrentVersion
		p.Versions = append(p.Versions, store.PackageVersion{
			Version: version, Content: in.Content, Citations: filled,
			Sources: in.Sources, References: in.References,
			ChangeNote: in.ChangeNote, Level: lvl.String(),
			CreatedAt: time.Now(), CreatedBy: u.ID})
		p.UpdatedAt = time.Now()
	})
	_ = a.st.Save()
	var name string
	a.st.Read(func(st *store.State) { name = st.KnowledgePackages[id].Name })
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "package_version_added",
		Detail: map[string]string{"package": name, "version": strconv.Itoa(version), "level": lvl.String()}})
	writeJSON(w, http.StatusCreated, map[string]any{"version": version, "level": lvl.String()})
}

// reviewKnowledgePackage approves (status published; on first publish
// the current version becomes the published version) or rejects a
// package. Rejecting a published package withdraws it from serving:
// kb_pack_list / kb_pack_get require status published.
func (a *API) reviewKnowledgePackage(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var in struct {
		Approve bool `json:"approve"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var status, name string
	var published int
	var conflict string
	if in.Approve {
		// First publish goes through the same re-validation as the
		// publish endpoint: a recipe whose source vanished or whose
		// reference was withdrawn after the version was drafted must
		// not silently become the production version.
		var pkg *store.KnowledgePackage
		a.st.Read(func(st *store.State) {
			if p, ok := st.KnowledgePackages[id]; ok {
				cp := *p
				pkg = &cp
			}
		})
		if pkg != nil && pkg.PublishedVersion == 0 {
			if ver := pkg.Version(pkg.CurrentVersion); ver != nil {
				if a.fk == nil {
					writeErr(w, http.StatusServiceUnavailable, "folder knowledge base not available")
					return
				}
				var pkgs map[string]*store.KnowledgePackage
				a.st.Read(func(st *store.State) { pkgs = st.KnowledgePackages })
				if _, _, err := knowpack.ResolveVersion(id, ver.Sources, ver.References, stripFilled(ver.Citations), a.fk.Snapshots(), pkgs); err != nil {
					conflict = "version no longer validates: " + err.Error()
				}
			}
		}
	}
	if conflict != "" {
		writeErr(w, http.StatusConflict, conflict)
		return
	}
	a.st.WithLock(func(st *store.State) {
		p, ok := st.KnowledgePackages[id]
		if !ok {
			return
		}
		name = p.Name
		if in.Approve {
			p.Status = "published"
			if p.PublishedVersion == 0 {
				p.PublishedVersion = p.CurrentVersion
			}
		} else {
			p.Status = "rejected"
		}
		p.UpdatedAt = time.Now()
		status = p.Status
		published = p.PublishedVersion
	})
	if status == "" {
		writeErr(w, http.StatusNotFound, "package not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "package_reviewed",
		Detail: map[string]string{"package": name, "result": status}})
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "published_version": published})
}

// publishKnowledgePackage publishes (or rolls back to) a specific
// version. Before publishing, the version's recipe is re-validated
// against the live index: a source document that vanished, a
// reference that was withdrawn, or a reference republished higher
// all refuse the publish with 409, so a stale recipe can never
// silently become the production version.
func (a *API) publishKnowledgePackage(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var in struct {
		Version int `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Version < 1 {
		writeErr(w, http.StatusBadRequest, "version required")
		return
	}
	var pkg *store.KnowledgePackage
	a.st.Read(func(st *store.State) {
		if p, ok := st.KnowledgePackages[id]; ok {
			cp := *p
			pkg = &cp
		}
	})
	if pkg == nil {
		writeErr(w, http.StatusNotFound, "package not found")
		return
	}
	ver := pkg.Version(in.Version)
	if ver == nil {
		writeErr(w, http.StatusBadRequest, "version does not exist")
		return
	}
	if a.fk == nil {
		writeErr(w, http.StatusServiceUnavailable, "folder knowledge base not available")
		return
	}
	var pkgs map[string]*store.KnowledgePackage
	a.st.Read(func(st *store.State) { pkgs = st.KnowledgePackages })
	if _, _, err := knowpack.ResolveVersion(id, ver.Sources, ver.References, stripFilled(ver.Citations), a.fk.Snapshots(), pkgs); err != nil {
		writeErr(w, http.StatusConflict, "version no longer validates: "+err.Error())
		return
	}
	a.st.WithLock(func(st *store.State) {
		p := st.KnowledgePackages[id]
		p.PublishedVersion = in.Version
		p.Status = "published"
		p.UpdatedAt = time.Now()
	})
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "package_published",
		Detail: map[string]string{"package": pkg.Name, "version": strconv.Itoa(in.Version)}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "published", "published_version": in.Version})
}

// stripFilled drops the tree-filled fields of stored citations so
// ResolveVersion can re-fill them from the live tree (a citation
// whose section vanished fails the re-validation instead of being
// served from stale text).
func stripFilled(cits []store.PackageCitation) []store.PackageCitation {
	out := make([]store.PackageCitation, 0, len(cits))
	for _, c := range cits {
		out = append(out, store.PackageCitation{DocID: c.DocID, NodeID: c.NodeID})
	}
	return out
}

func (a *API) deleteKnowledgePackage(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var name string
	found := false
	a.st.WithLock(func(st *store.State) {
		if p, ok := st.KnowledgePackages[id]; ok {
			name = p.Name
			delete(st.KnowledgePackages, id)
			found = true
		}
	})
	if !found {
		writeErr(w, http.StatusNotFound, "package not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "package_deleted",
		Detail: map[string]string{"package": name}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}
