package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/semidx"
	"github.com/myjnathan/agentgate/internal/store"
)

// SetSemanticIndex wires the semantic index (backlog F19) used by the
// endpoints below. A nil index disables them (503).
func (a *API) SetSemanticIndex(ix *semidx.Index) {
	a.semidx = ix
}

// SetSemanticModel wires the model generator configured from the
// environment (AGENTGATE_SEMANTIC_MODEL_URL / _TOKEN / _MODEL). It is
// the default for model regeneration; a request may still supply an
// inline endpoint instead.
func (a *API) SetSemanticModel(gen *semidx.ModelGenerator) {
	a.semModel = gen
}

// refreshSemanticIndex regenerates extractive entries for added or
// changed documents after a rescan. It is a no-op when the folder
// index or the semantic index is not wired (unit tests that only
// exercise the folder endpoints).
func (a *API) refreshSemanticIndex() {
	if a.fk == nil || a.semidx == nil {
		return
	}
	a.semidx.RegenerateChanged(context.Background(), a.fk.Snapshots())
}

// semanticCoverage returns the index-governance report: totals,
// per-document status, stale documents and vanished files.
func (a *API) semanticCoverage(w http.ResponseWriter, r *http.Request, u *store.User) {
	if a.semidx == nil || a.fk == nil {
		writeErr(w, http.StatusServiceUnavailable, "semantic index not available")
		return
	}
	cov := a.semidx.Coverage(a.fk.Snapshots())
	writeJSON(w, http.StatusOK, map[string]any{"coverage": cov})
}

// semanticEntries returns one document's entries. The level gate is
// the admin's own visibility: only admin/security call this endpoint,
// so the full index is visible here; the tier gate lives in the MCP
// tool instead.
func (a *API) semanticEntries(w http.ResponseWriter, r *http.Request, u *store.User) {
	if a.semidx == nil {
		writeErr(w, http.StatusServiceUnavailable, "semantic index not available")
		return
	}
	id := r.URL.Query().Get("doc_id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "doc_id required")
		return
	}
	entries, ok := a.semidx.Entries(id, store.LevelSecret)
	if !ok {
		writeErr(w, http.StatusNotFound, "document has no semantic entries")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"doc_id": id, "entries": entries})
}

type semanticRegenResult struct {
	DocID       string `json:"doc_id"`
	Entries     int    `json:"entries"`
	GeneratedBy string `json:"generated_by"`
	ModelError  string `json:"model_error,omitempty"`
}

// regenerateSemantic regenerates a folder's entries. By default the
// extractive generator runs (no model, no leak channel). With
// {"model": true} the model generator runs instead: it uses the
// inline endpoint when model_url is given, otherwise the server's
// configured model generator, and falls back to extractive entries
// per document when the model fails — the fallback is reported, not
// hidden. The supplied token is used for one request only: it is
// never stored, logged or returned.
func (a *API) regenerateSemantic(w http.ResponseWriter, r *http.Request, u *store.User) {
	if a.semidx == nil || a.fk == nil {
		writeErr(w, http.StatusServiceUnavailable, "semantic index not available")
		return
	}
	id := r.PathValue("id")
	src, ok := a.fk.Source(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "folder source not found")
		return
	}
	var in struct {
		Model     bool   `json:"model"`
		ModelURL  string `json:"model_url"`
		ModelTok  string `json:"model_token"`
		ModelName string `json:"model_name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var gen semidx.Generator = semidx.ExtractiveGenerator{}
	modelRequested := in.Model
	if modelRequested {
		switch {
		case in.ModelURL != "":
			gen = semidx.NewModelGenerator(semidx.ModelConfig{
				Endpoint: in.ModelURL, Token: in.ModelTok, Model: in.ModelName}, nil)
		case a.semModel != nil:
			gen = a.semModel
		default:
			writeErr(w, http.StatusServiceUnavailable, "semantic model not configured (set AGENTGATE_SEMANTIC_MODEL_URL or pass model_url)")
			return
		}
	}
	var snaps []folderkb.DocSnapshot
	for _, s := range a.fk.Snapshots() {
		if s.SourceID == id {
			snaps = append(snaps, s)
		}
	}
	var results []semanticRegenResult
	fallback := false
	for _, snap := range snaps {
		de := a.semidx.RegenerateDoc(r.Context(), snap, gen)
		results = append(results, semanticRegenResult{
			DocID: de.DocID, Entries: len(de.Entries),
			GeneratedBy: de.GeneratedBy, ModelError: de.ModelError})
		if de.ModelError != "" {
			fallback = true
		}
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin",
		Action: "semantic_regenerated", Detail: map[string]string{"folder": src.Name,
			"model":          strconv.FormatBool(modelRequested),
			"docs":           strconv.Itoa(len(results)),
			"model_fallback": strconv.FormatBool(fallback)}})
	writeJSON(w, http.StatusOK, map[string]any{
		"regenerated": len(results), "documents": results, "model_fallback": fallback})
}
