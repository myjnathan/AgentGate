package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/myjnathan/agentgate/internal/attest"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/exception"
	"github.com/myjnathan/agentgate/internal/pluginpol"
	"github.com/myjnathan/agentgate/internal/store"
	"github.com/myjnathan/agentgate/internal/terminal"
)

// Terminal inventory (backlog F23): the platform side of continuous
// attestation. A sidecar heartbeat upserts one Terminal record per
// (user, agent type); drift is judged by the same F14 appraisal
// policy that judges login-time reports, so a terminal that falls
// behind shows up red in the admin list without waiting for its
// next login.

// SetSidecarPolicy wires the policy snapshot the sidecar caches
// (backlog F23): the gateway's banned/custom words, dry-run flag and
// offline max level. Set by cmd/agentgate from the gateway policy;
// unset means the snapshot endpoint reports unavailable.
func (a *API) SetSidecarPolicy(fn func() terminal.PolicySnapshot) {
	a.sidecarPolicyFn = fn
}

// POST /api/terminals/heartbeat — one sidecar heartbeat.
//
// Authenticated by API token (or, for manual use, a web session).
// The agent type and tier come from the bearer token when one is
// present — a heartbeat can only describe the agent that holds the
// token — and fall back to the body's agent_type for session calls.
// Drift transitions and offline backfills are audited; the heartbeat
// itself is not, or the audit log would be one heartbeat per minute
// per terminal.
func (a *API) terminalHeartbeat(w http.ResponseWriter, r *http.Request, u *store.User) {
	var hb terminal.Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := hb.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	agentType := hb.AgentType
	tier := ""
	if tok, _, ok := a.auth.ResolveToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); ok {
		agentType = tok.AgentType
		tier = string(tok.Tier)
	}
	if agentType == "" {
		writeErr(w, http.StatusBadRequest, "agent_type required")
		return
	}
	hb.AgentType = agentType
	now := time.Now()
	var rec *terminal.Terminal
	var prevDrift []string
	var prevPluginAction string
	var prevPluginReasons []string
	var prevPluginUnknown []string
	var pluginUnknown []string
	var pluginActionChanged, pluginReasonsChanged, pluginUnknownChanged bool
	isNew := false
	a.st.WithLock(func(st *store.State) {
		for _, t := range st.Terminals {
			if t.UserID == u.ID && t.AgentType == agentType {
				rec = t
				break
			}
		}
		if rec == nil {
			rec = &terminal.Terminal{ID: store.NewID("term"), UserID: u.ID, Username: u.Username,
				Department: u.Department, AgentType: agentType, FirstSeen: now}
			st.Terminals[rec.ID] = rec
			isNew = true
		}
		prevDrift = append([]string(nil), rec.DriftReasons...)
		prevPluginAction = rec.PluginAction
		prevPluginReasons = append([]string(nil), rec.PluginReasons...)
		prevPluginUnknown = append([]string(nil), rec.PluginUnknown...)
		rec.Username = u.Username
		rec.Department = u.Department
		rec.Tier = tier
		rec.Version = hb.Version
		rec.ConfigHash = hb.ConfigHash
		rec.BinaryHash = hb.BinaryHash
		rec.Plugins = append([]string(nil), hb.Plugins...)
		if len(hb.PluginDetails) > 0 {
			rec.PluginDetails = append([]attest.PluginEvidence(nil), hb.PluginDetails...)
		} else {
			rec.PluginDetails = nil
		}
		rec.GuardApplied = hb.GuardApplied
		rec.OS = hb.OS
		rec.LastSeen = now
		rec.HeartbeatCount++
		// Drift is judged by the F14 appraisal policy for this
		// agent type (nil policy = no appraisal, no drift), plus
		// the F38 plugin appraisal — the same evaluation login
		// uses, so a terminal cannot pass at login and drift
		// silently afterwards (or vice versa).
		rec.DriftReasons = terminal.DriftReasons(hb.Report(), st.AttestationPolicies[agentType])
		// F38 slice 3: the terminal owner's approved plugin
		// exceptions are honoured here, so a waived plugin stops
		// drifting (and its disposition clears) until the
		// exception expires — at which point the next heartbeat
		// drifts again with no admin click anywhere.
		excepted := exception.ExceptedPlugins(st.Exceptions, u.ID, now)
		reasons, unknown, action := pluginpol.EvaluateReportWithExceptions(hb.Report(), st.PluginPolicies, excepted, u.Department, nil, now)
		if len(reasons) > 0 || len(unknown) > 0 {
			for _, r := range reasons {
				if !containsString(rec.DriftReasons, r) {
					rec.DriftReasons = append(rec.DriftReasons, r)
				}
			}
			pluginUnknown = unknown
		}
		// F38 slice 2: the disposition in force for this terminal.
		// Violations take the policy's disposition; unknown plugins
		// alone (denylist, flagged but allowed) sit at warn; a
		// clean report clears the disposition — remediation is
		// automatic, no admin click needed.
		newAction := ""
		switch {
		case len(reasons) > 0:
			newAction = action
		case len(unknown) > 0:
			newAction = pluginpol.ActionWarn
		}
		pluginActionChanged = newAction != prevPluginAction
		pluginReasonsChanged = strings.Join(reasons, ",") != strings.Join(prevPluginReasons, ",")
		if pluginActionChanged {
			rec.PluginAction = newAction
			if newAction != "" {
				rec.PluginReasons = append([]string(nil), reasons...)
				at := now
				rec.PluginActionAt = &at
			} else {
				rec.PluginReasons = nil
				rec.PluginActionAt = nil
			}
		} else if newAction != "" {
			// Same disposition, new reasons: keep the record
			// current so the asset view never shows stale causes.
			rec.PluginReasons = append([]string(nil), reasons...)
		}
		rec.PluginUnknown = append([]string(nil), unknown...)
		if len(rec.PluginUnknown) == 0 {
			rec.PluginUnknown = nil
		}
		pluginUnknownChanged = strings.Join(unknown, ",") != strings.Join(prevPluginUnknown, ",")
		if hb.Offline != nil && hb.Offline.Events > 0 {
			off := *hb.Offline
			rec.LastOfflineAudit = &off
			rec.LastOfflineAt = &now
		}
	})
	_ = a.st.Save()
	// Audit only transitions and backfills, never the heartbeat
	// itself: a heartbeat per minute per terminal would drown the
	// log, and the record already carries the latest state.
	detail := map[string]string{"agent_type": agentType, "version": hb.Version, "tier": tier}
	if isNew {
		a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, AgentType: agentType,
			Kind: "terminal", Action: "terminal_registered", Decision: "allow", Detail: detail})
	}
	driftNow := len(rec.DriftReasons) > 0
	driftWas := len(prevDrift) > 0
	if driftNow && !driftWas {
		d := map[string]string{"agent_type": agentType, "version": hb.Version,
			"reasons": strings.Join(rec.DriftReasons, ",")}
		// Denylist unknown plugins are allowed by default but
		// flagged: record them so the alert (slice 2) has the
		// names without waiting for the next drift transition.
		if len(pluginUnknown) > 0 {
			d["plugin_unknown"] = strings.Join(pluginUnknown, ",")
		}
		a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, AgentType: agentType,
			Kind: "terminal", Action: "terminal_drift", Decision: "allow", Detail: d})
	} else if !driftNow && driftWas {
		a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, AgentType: agentType,
			Kind: "terminal", Action: "terminal_drift_cleared", Decision: "allow", Detail: detail})
	}
	// F38 slice 2: plugin disposition transitions are audited on
	// their own — drift alone does not say which rung of the ladder
	// is in force, and a terminal already drifted for F14 reasons
	// would otherwise escalate silently. The action event is not
	// alerted (the drift alert covers first entry); remediation is
	// recorded so the auto-restore is visible in the audit trail.
	if rec.PluginAction != "" && (pluginActionChanged || pluginReasonsChanged) {
		d := map[string]string{"agent_type": agentType, "version": hb.Version,
			"action": rec.PluginAction, "reasons": strings.Join(rec.PluginReasons, ",")}
		a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, AgentType: agentType,
			Kind: "terminal", Action: "terminal_plugin_action", Decision: "allow", Detail: d})
	} else if rec.PluginAction == "" && prevPluginAction != "" {
		d := map[string]string{"agent_type": agentType, "version": hb.Version,
			"previous_action": prevPluginAction}
		a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, AgentType: agentType,
			Kind: "terminal", Action: "terminal_plugin_remediated", Decision: "allow", Detail: d})
	}
	// Unknown plugins alone do not drift, so they get their own
	// alertable event — unless this heartbeat already fired a
	// terminal_drift event, which carries the same names in its
	// detail and already alerted.
	if pluginUnknownChanged && len(pluginUnknown) > 0 && !(driftNow && !driftWas) {
		d := map[string]string{"agent_type": agentType, "version": hb.Version,
			"plugins": strings.Join(pluginUnknown, ",")}
		a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, AgentType: agentType,
			Kind: "terminal", Action: "terminal_plugin_unknown", Decision: "allow", Detail: d})
	}
	if hb.Offline != nil && hb.Offline.Events > 0 {
		byCat := make([]string, 0, len(hb.Offline.ByCategory))
		for k, v := range hb.Offline.ByCategory {
			byCat = append(byCat, k+":"+strconv.FormatInt(v, 10))
		}
		sort.Strings(byCat)
		d := map[string]string{"agent_type": agentType,
			"events":       strconv.Itoa(hb.Offline.Events),
			"blocked":      strconv.Itoa(hb.Offline.Blocked),
			"unreachable":  strconv.Itoa(hb.Offline.Unreachable),
			"window_start": hb.Offline.WindowStart.Format(time.RFC3339),
			"window_end":   hb.Offline.WindowEnd.Format(time.RFC3339),
			"by_category":  strings.Join(byCat, ",")}
		a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, AgentType: agentType,
			Kind: "terminal", Action: "terminal_offline_audit", Decision: "allow", Detail: d})
	}
	drift := rec.DriftReasons
	if drift == nil {
		drift = []string{}
	}
	if pluginUnknown == nil {
		pluginUnknown = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"terminal_id": rec.ID, "status": rec.Status(now), "drift_reasons": drift,
		"plugin_unknown": pluginUnknown, "plugin_action": rec.PluginAction,
		"last_seen": rec.LastSeen,
	})
}

// containsString reports whether list holds s.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// terminalView is the admin-facing terminal row: the record plus its
// computed freshness status.
type terminalView struct {
	*terminal.Terminal
	Status string `json:"status"`
}

// GET /api/terminals — the admin asset list (backlog F23). Filters:
// department, agent_type, version, status (online|stale|offline),
// drift (true = only drifted terminals). Sorted by last seen,
// newest first.
func (a *API) listTerminals(w http.ResponseWriter, r *http.Request, u *store.User) {
	q := r.URL.Query()
	dept := q.Get("department")
	agent := q.Get("agent_type")
	version := q.Get("version")
	status := q.Get("status")
	if status != "" && status != terminal.StatusOnline && status != terminal.StatusStale && status != terminal.StatusOffline {
		writeErr(w, http.StatusBadRequest, "invalid status (online|stale|offline)")
		return
	}
	driftFilter := -1 // -1 unset, 0 only clean, 1 only drifted
	if v := q.Get("drift"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid drift (true|false)")
			return
		}
		if b {
			driftFilter = 1
		} else {
			driftFilter = 0
		}
	}
	now := time.Now()
	var out []terminalView
	a.st.Read(func(st *store.State) {
		for _, t := range st.Terminals {
			if dept != "" && t.Department != dept {
				continue
			}
			if agent != "" && t.AgentType != agent {
				continue
			}
			if version != "" && t.Version != version {
				continue
			}
			if status != "" && t.Status(now) != status {
				continue
			}
			if driftFilter == 1 && !t.IsDrifted() {
				continue
			}
			if driftFilter == 0 && t.IsDrifted() {
				continue
			}
			out = append(out, terminalView{Terminal: t, Status: t.Status(now)})
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	if out == nil {
		out = []terminalView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"terminals": out})
}

// GET /api/terminals/{id} — one terminal's record.
func (a *API) getTerminal(w http.ResponseWriter, r *http.Request, u *store.User) {
	var rec *terminal.Terminal
	a.st.Read(func(st *store.State) { rec = st.Terminals[r.PathValue("id")] })
	if rec == nil {
		writeErr(w, http.StatusNotFound, "terminal not found")
		return
	}
	writeJSON(w, http.StatusOK, terminalView{Terminal: rec, Status: rec.Status(time.Now())})
}

// GET /api/sidecar/policy — the policy snapshot a sidecar caches
// (backlog F23). Any valid token may fetch it: the snapshot holds
// word lists and a dry-run flag, not secrets, and the sidecar
// enforces it locally. The version lets the sidecar tell a stale
// cache from a current one.
func (a *API) getSidecarPolicy(w http.ResponseWriter, r *http.Request, u *store.User) {
	if a.sidecarPolicyFn == nil {
		writeErr(w, http.StatusServiceUnavailable, "sidecar policy unavailable")
		return
	}
	snap := a.sidecarPolicyFn()
	writeJSON(w, http.StatusOK, map[string]any{
		"source":            "gateway",
		"version":           snap.Version,
		"banned_words":      snap.BannedWords,
		"custom_words":      snap.CustomWords,
		"dry_run":           snap.DryRun,
		"offline_max_level": snap.OfflineMaxLevel,
	})
}
