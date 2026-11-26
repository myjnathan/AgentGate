package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/exception"
	"github.com/myjnathan/agentgate/internal/store"
)

// Exception approval flow (backlog F24): an employee whose call was
// blocked by the classification policy applies for an exception
// (reason + scope + validity), an admin approves or rejects it, and
// approval issues a one-time grant token bound to that employee,
// that data level and an expiry. The grant token is returned exactly
// once in the approve response — the stored record only holds its
// SHA-256 hash, and no later read ever returns the token again.
//
// Audit events carry metadata only (exception ID, data level, hours,
// applicant): the reason/scope text is business justification that
// may itself describe sensitive work, so it never enters the audit
// log. Top-secret exceptions need the operator opt-in
// (AGENTGATE_ALLOW_TOPSECRET_EXCEPTIONS); the default is closed.

// SetAllowTopSecretExceptions wires the operator opt-in for
// top-secret exceptions (backlog F24 red line). Set by cmd/agentgate
// from the environment; unset means top-secret calls can never be
// excepted.
func (a *API) SetAllowTopSecretExceptions(allow bool) {
	a.allowTopSecretExceptions = allow
}

// exceptionView is the wire shape of an exception request. The grant
// hash is deliberately absent: even the hash is useless to a reader
// (it cannot be turned back into a token), and the plaintext token
// is returned only once, inside the approve response. Plugin
// exceptions (kind "plugin", F38 slice 3) carry no grant at all —
// approval creates a time-limited waiver, not a one-time token —
// and their plugin triple rides in the plugin_* fields; data_level
// is "public" for them (not applicable) and must not be read as a
// classification decision.
type exceptionView struct {
	ID            string     `json:"id"`
	ApplicantID   string     `json:"applicant_id"`
	Applicant     string     `json:"applicant"`
	Department    string     `json:"department,omitempty"`
	Kind          string     `json:"kind"`
	DataLevel     string     `json:"data_level"`
	PluginID      string     `json:"plugin_id,omitempty"`
	PluginVersion string     `json:"plugin_version,omitempty"`
	PluginHash    string     `json:"plugin_hash,omitempty"`
	PluginSource  string     `json:"plugin_source,omitempty"`
	Reason        string     `json:"reason"`
	Scope         string     `json:"scope"`
	Hours         int        `json:"hours"`
	Status        string     `json:"status"`
	DecidedBy     string     `json:"decided_by,omitempty"`
	DecisionNote  string     `json:"decision_note,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	UsedAt        *time.Time `json:"used_at,omitempty"`
}

func viewException(e *store.Exception) exceptionView {
	kind := e.Kind
	if kind == "" {
		kind = exception.KindCall
	}
	return exceptionView{ID: e.ID, ApplicantID: e.ApplicantID, Applicant: e.Applicant,
		Department: e.Department, Kind: kind, DataLevel: e.DataLevel.String(),
		PluginID: e.PluginID, PluginVersion: e.PluginVersion, PluginHash: e.PluginHash,
		PluginSource: e.PluginSource, Reason: e.Reason,
		Scope: e.Scope, Hours: e.Hours, Status: e.Status, DecidedBy: e.DecidedBy,
		DecisionNote: e.DecisionNote, CreatedAt: e.CreatedAt, DecidedAt: e.DecidedAt,
		ExpiresAt: e.ExpiresAt, UsedAt: e.UsedAt}
}

// refreshExceptionStatuses flips approved-but-past-expiry grants to
// expired. Callers hold the store lock; the bool reports whether
// anything changed, so the caller knows to persist.
func refreshExceptionStatuses(st *store.State, now time.Time) bool {
	changed := false
	for _, e := range st.Exceptions {
		if exception.RefreshStatus(e, now) {
			changed = true
		}
	}
	return changed
}

// POST /api/exceptions — any logged-in user applies for an exception.
// kind "call" (the default) is the F24 one-time grant for a blocked
// high-data-level call; kind "plugin" (F38 slice 3) is a time-limited
// waiver for one plugin triple, judged on the same form.
func (a *API) createException(w http.ResponseWriter, r *http.Request, u *store.User) {
	var body struct {
		Kind          string `json:"kind"`
		DataLevel     string `json:"data_level"`
		PluginID      string `json:"plugin_id"`
		PluginVersion string `json:"plugin_version"`
		PluginHash    string `json:"plugin_hash"`
		PluginSource  string `json:"plugin_source"`
		Reason        string `json:"reason"`
		Scope         string `json:"scope"`
		Hours         int    `json:"hours"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	kind := body.Kind
	if kind == "" {
		kind = exception.KindCall
	}
	now := time.Now()
	rec := &store.Exception{ID: store.NewID("exc"), ApplicantID: u.ID, Applicant: u.Username,
		Department: u.Department, Kind: kind, Reason: body.Reason, Scope: body.Scope,
		Hours: body.Hours, Status: exception.StatusPending, CreatedAt: now}
	switch kind {
	case exception.KindCall:
		level, ok := store.ParseDataLevel(body.DataLevel)
		if !ok {
			writeErr(w, http.StatusBadRequest, "unknown data_level (public|internal|confidential|secret)")
			return
		}
		if err := exception.ValidateRequest(level, body.Reason, body.Scope, body.Hours, a.allowTopSecretExceptions); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		rec.DataLevel = level
	case exception.KindPlugin:
		if err := exception.ValidatePluginRequest(body.PluginID, body.PluginVersion, body.PluginHash, body.PluginSource, body.Reason, body.Scope, body.Hours); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		rec.PluginID = body.PluginID
		rec.PluginVersion = body.PluginVersion
		rec.PluginHash = body.PluginHash
		rec.PluginSource = body.PluginSource
		// DataLevel stays public (zero): not applicable to a plugin
		// waiver, and no F24 grant logic may ever match it — a
		// plugin exception has no grant hash.
	default:
		writeErr(w, http.StatusBadRequest, "unknown kind (call|plugin)")
		return
	}
	a.st.WithLock(func(st *store.State) {
		st.Exceptions[rec.ID] = rec
	})
	_ = a.st.Save()
	detail := map[string]string{"exception_id": rec.ID, "kind": kind, "hours": strconv.Itoa(body.Hours)}
	if kind == exception.KindPlugin {
		detail["plugin_id"] = rec.PluginID
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "exception",
		Action: "exception_requested", DataLevel: rec.DataLevel.String(), Decision: "allow",
		Detail: detail})
	writeJSON(w, http.StatusCreated, viewException(rec))
}

// GET /api/exceptions — admins see all requests; anyone else sees
// only their own. ?status= filters by status.
func (a *API) listExceptions(w http.ResponseWriter, r *http.Request, u *store.User) {
	isAdmin := u.Role == "admin" || u.Role == "security"
	status := r.URL.Query().Get("status")
	now := time.Now()
	var out []exceptionView
	a.st.WithLock(func(st *store.State) {
		changed := refreshExceptionStatuses(st, now)
		for _, e := range st.Exceptions {
			if !isAdmin && e.ApplicantID != u.ID {
				continue
			}
			if status != "" && e.Status != status {
				continue
			}
			out = append(out, viewException(e))
		}
		if changed {
			_ = a.st.Save()
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"exceptions": out})
}

// GET /api/exceptions/{id} — the applicant or an admin.
func (a *API) getException(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var rec *store.Exception
	a.st.WithLock(func(st *store.State) {
		if e, ok := st.Exceptions[id]; ok {
			exception.RefreshStatus(e, time.Now())
			cp := *e
			rec = &cp
		}
	})
	_ = a.st.Save()
	if rec == nil {
		writeErr(w, http.StatusNotFound, "exception not found")
		return
	}
	if u.Role != "admin" && u.Role != "security" && rec.ApplicantID != u.ID {
		writeErr(w, http.StatusForbidden, "not your exception request")
		return
	}
	writeJSON(w, http.StatusOK, viewException(rec))
}

// POST /api/exceptions/{id}/approve — admin only, pending only.
// A call exception mints the one-time grant; the plaintext token is
// returned exactly once, in this response. The approver hands it to
// the applicant (out of band); the gateway accepts it as the
// X-AgentGate-Exception-Grant header on the next blocked call.
// A plugin exception mints no grant: approval creates a
// time-limited waiver (applicant + plugin triple + expiry) that the
// plugin evaluation honours until ExpiresAt, so the response
// carries no "grant" key at all.
func (a *API) approveException(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var body struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	now := time.Now()
	var rec *store.Exception
	var token, hash string
	a.st.WithLock(func(st *store.State) {
		if e, ok := st.Exceptions[id]; ok && e.Status == exception.StatusPending {
			expires := now.Add(time.Duration(e.Hours) * time.Hour)
			e.Status = exception.StatusApproved
			if e.Kind == exception.KindPlugin {
				// No grant: the waiver is the approval itself.
				e.GrantHash = ""
			} else {
				token, hash = exception.NewGrant()
				e.GrantHash = hash
			}
			e.DecidedBy = u.Username
			e.DecisionNote = body.Note
			e.DecidedAt = &now
			e.ExpiresAt = &expires
			cp := *e
			rec = &cp
		}
	})
	if rec == nil {
		writeErr(w, http.StatusBadRequest, "exception not found or not pending")
		return
	}
	_ = a.st.Save()
	detail := map[string]string{"exception_id": rec.ID, "kind": rec.Kind, "applicant": rec.Applicant,
		"hours": strconv.Itoa(rec.Hours)}
	if rec.Kind == exception.KindPlugin {
		detail["plugin_id"] = rec.PluginID
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "exception",
		Action: "exception_approved", DataLevel: rec.DataLevel.String(), Decision: "allow",
		Detail: detail})
	// The grant rides alongside the view, once: no later read
	// returns it, and the store only holds its hash. A plugin
	// approval has no grant to ride.
	out := viewException(rec)
	resp := map[string]any{"exception": out}
	if token != "" {
		resp["grant"] = token
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/exceptions/{id}/reject — admin only, pending only.
func (a *API) rejectException(w http.ResponseWriter, r *http.Request, u *store.User) {
	a.decideException(w, r, u, exception.StatusRejected, "exception_rejected", "exception not found or not pending")
}

// POST /api/exceptions/{id}/revoke — admin only, approved only.
// Cancels a grant that has not been used yet (e.g. the need went
// away, or the top-secret opt-in was switched off after approval).
func (a *API) revokeException(w http.ResponseWriter, r *http.Request, u *store.User) {
	a.decideException(w, r, u, exception.StatusRevoked, "exception_revoked", "exception not found or not approved")
}

func (a *API) decideException(w http.ResponseWriter, r *http.Request, u *store.User, status, action, errMsg string) {
	id := r.PathValue("id")
	var body struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	now := time.Now()
	var rec *store.Exception
	a.st.WithLock(func(st *store.State) {
		if e, ok := st.Exceptions[id]; ok {
			want := exception.StatusPending
			if status == exception.StatusRevoked {
				want = exception.StatusApproved
			}
			if e.Status == want {
				e.Status = status
				e.DecidedBy = u.Username
				e.DecisionNote = body.Note
				e.DecidedAt = &now
				cp := *e
				rec = &cp
			}
		}
	})
	if rec == nil {
		writeErr(w, http.StatusBadRequest, errMsg)
		return
	}
	_ = a.st.Save()
	detail := map[string]string{"exception_id": rec.ID, "kind": rec.Kind, "applicant": rec.Applicant}
	if rec.Kind == exception.KindPlugin {
		detail["plugin_id"] = rec.PluginID
	}
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "exception",
		Action: action, DataLevel: rec.DataLevel.String(), Decision: "allow",
		Detail: detail})
	writeJSON(w, http.StatusOK, viewException(rec))
}
