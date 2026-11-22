package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/store"
)

// SetFolderKB wires the folder knowledge-base registry (backlog
// F18) used by the folder endpoints below.
func (a *API) SetFolderKB(fk *folderkb.Registry) {
	a.fk = fk
}

type folderSourceView struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Path       string             `json:"path"`
	Level      string             `json:"level"`
	Department string             `json:"department,omitempty"`
	Enabled    bool               `json:"enabled"`
	Docs       int                `json:"docs"`
	LastScan   string             `json:"last_scan,omitempty"`
	LastErr    string             `json:"last_error,omitempty"`
	Last       folderkb.ScanStats `json:"last_scan_stats"`
}

func (a *API) listFolderSources(w http.ResponseWriter, r *http.Request, u *store.User) {
	var out []folderSourceView
	if a.fk != nil {
		for _, st := range a.fk.Status() {
			v := folderSourceView{ID: st.Source.ID, Name: st.Source.Name, Path: st.Source.Path,
				Level: st.Source.Level.String(), Department: st.Source.Department,
				Enabled: st.Source.Enabled,
				Docs:    st.Docs, LastErr: st.LastErr, Last: st.Last}
			if st.LastScan != nil {
				v.LastScan = st.LastScan.Format("2006-01-02T15:04:05Z07:00")
			}
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": out})
}

func (a *API) createFolderSource(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		Level      string `json:"level"`
		Department string `json:"department"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.Path == "" {
		writeErr(w, http.StatusBadRequest, "name and path required")
		return
	}
	lvl := store.LevelInternal
	if in.Level != "" {
		v, ok := store.ParseDataLevel(in.Level)
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid level (public|internal|confidential|secret)")
			return
		}
		lvl = v
	}
	abs, err := filepath.Abs(in.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid path")
		return
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		writeErr(w, http.StatusBadRequest, "path must be an existing directory")
		return
	}
	if a.fk != nil {
		for _, s := range a.fk.Sources() {
			if s.Path == abs {
				writeErr(w, http.StatusConflict, "folder already registered")
				return
			}
		}
	}
	src := &store.KBFolderSource{ID: store.NewID("kbf"), Name: in.Name, Path: abs,
		Level: lvl, Department: in.Department, Enabled: true, CreatedAt: time.Now()}
	// Index before persisting: a folder that cannot be read is a
	// 400, not a registered-but-broken source.
	var stats folderkb.ScanStats
	if a.fk != nil {
		stats, err = a.fk.Rescan(src)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	a.st.WithLock(func(st *store.State) { st.KBFolderSources[src.ID] = src })
	_ = a.st.Save()
	a.refreshSemanticIndex()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "folder_source_created",
		Detail: map[string]string{"folder": src.Name, "path": src.Path, "level": lvl.String(),
			"docs": strconv.Itoa(stats.Added + stats.Unchanged)}})
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": src.ID, "name": src.Name, "path": src.Path, "level": lvl.String(), "docs": stats.Added + stats.Unchanged})
}

func (a *API) deleteFolderSource(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var name string
	a.st.WithLock(func(st *store.State) {
		if s, ok := st.KBFolderSources[id]; ok {
			name = s.Name
			delete(st.KBFolderSources, id)
		}
	})
	if name == "" {
		writeErr(w, http.StatusNotFound, "folder source not found")
		return
	}
	_ = a.st.Save()
	if a.fk != nil {
		a.fk.DropSource(id)
	}
	if a.semidx != nil {
		a.semidx.DropSource(id)
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "folder_source_deleted",
		Detail: map[string]string{"folder": name}})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (a *API) rescanFolderSource(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var src *store.KBFolderSource
	var ok bool
	if a.fk != nil {
		src, ok = a.fk.Source(id)
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "folder source not found")
		return
	}
	stats, err := a.fk.Rescan(src)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.refreshSemanticIndex()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "folder_source_rescanned",
		Detail: map[string]string{"folder": src.Name,
			"added": strconv.Itoa(stats.Added), "updated": strconv.Itoa(stats.Updated),
			"removed": strconv.Itoa(stats.Removed), "unchanged": strconv.Itoa(stats.Unchanged),
			"errors": strconv.Itoa(stats.Errors)}})
	writeJSON(w, http.StatusOK, map[string]any{
		"stats": stats, "docs": a.fk.DocCount(src.ID)})
}
