// Audit export and archiving endpoints (F4).
//
// GET /api/audit/export streams the hash-chained audit log as NDJSON
// for a time range; the chain verification result travels in response
// headers so the exported package is self-describing. POST
// /api/audit/archive moves events older than a cutoff into a
// gzip-compressed archive (see internal/audit). Export is readable by
// the auditor too (backlog F26); archiving stays admin / security.
// Both write their own audit event — an export or archive is itself
// auditable, and the event is appended after the response body so it
// is never included in its own export.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

func parseTimeParam(r *http.Request, name string) (time.Time, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be RFC3339 (e.g. 2026-11-01T00:00:00Z)", name)
	}
	return t, nil
}

func (a *API) exportAudit(w http.ResponseWriter, r *http.Request, u *store.User) {
	since, err := parseTimeParam(r, "since")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	until, err := parseTimeParam(r, "until")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !until.IsZero() && !since.IsZero() && !until.After(since) {
		writeErr(w, http.StatusBadRequest, "until must be after since")
		return
	}
	// Buffer first: the verification headers must be set before the
	// body, and the summary is only known once every event is written.
	var buf bytes.Buffer
	sum, err := a.audit.Export(&buf, audit.ExportFilter{
		Since: since, Until: until,
		Kind:  r.URL.Query().Get("kind"),
		Actor: r.URL.Query().Get("actor"),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "export failed")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="agentgate-audit-%s.ndjson"`,
		time.Now().UTC().Format("20060102-150405")))
	w.Header().Set("X-Audit-Chain-Valid", strconv.FormatBool(sum.ChainValid))
	w.Header().Set("X-Audit-Event-Count", strconv.FormatInt(sum.Count, 10))
	w.Header().Set("X-Audit-First-Seq", strconv.FormatInt(sum.FirstSeq, 10))
	w.Header().Set("X-Audit-Last-Seq", strconv.FormatInt(sum.LastSeq, 10))
	w.Header().Set("X-Audit-Head-Hash", sum.HeadHash)
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
	// Record the export itself (after the body, so it is not part of
	// its own export). No event content, only the range and count.
	detail := map[string]string{
		"events":      strconv.FormatInt(sum.Count, 10),
		"chain_valid": strconv.FormatBool(sum.ChainValid),
	}
	if !since.IsZero() {
		detail["since"] = since.Format(time.RFC3339)
	}
	if !until.IsZero() {
		detail["until"] = until.Format(time.RFC3339)
	}
	if k := r.URL.Query().Get("kind"); k != "" {
		detail["kind"] = k
	}
	if actor := r.URL.Query().Get("actor"); actor != "" {
		detail["actor_filter"] = actor
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin",
		Action: "audit_exported", Decision: "allow", Detail: detail})
}

func (a *API) archiveAudit(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Before string `json:"before"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Before == "" {
		writeErr(w, http.StatusBadRequest, "before (RFC3339) is required")
		return
	}
	before, err := time.Parse(time.RFC3339, in.Before)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "before must be RFC3339 (e.g. 2026-05-01T00:00:00Z)")
		return
	}
	rep, err := a.audit.Archive(before, u.Username)
	if err != nil {
		if errors.Is(err, audit.ErrChainInvalid) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "archive failed")
		return
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin",
		Action: "audit_archived", Decision: "allow", Detail: map[string]string{
			"archive_file": rep.ArchiveFile,
			"archived":     strconv.Itoa(rep.Archived),
		}})
	writeJSON(w, http.StatusOK, rep)
}
