// Package api — usage statistics (F2).
//
// GET /api/stats aggregates model-gateway usage from the hash-chained
// audit log, so the numbers reconcile with the audit trail by
// construction: every counted call is one audit event, and every audit
// event in scope is counted exactly once.
//
// Counted events:
//   - Kind "model_call": a forwarded chat completion (any decision:
//     allow | warn | redact | reroute | dry_run:*). Tokens come from the
//     upstream usage report recorded on the event.
//   - Kind "violation" + Action "model_call_blocked": a call the
//     protection pipeline refused (banned content, BYOA confidential,
//     no channel for the data level). Tokens are 0 and the call counts
//     toward "blocked".
//
// Dimensions (the enterprise angles generic gateways lack): user,
// department (resolved from the actor's user record), channel, model,
// agent type, data level, and disposition. Cost in currency is
// deliberately out of scope here — channels carry no price yet;
// that is F13 (department budgets).
package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// statsBucket is one row of an aggregation: calls, tokens consumed,
// and how many of the calls were blocked by the protection pipeline.
type statsBucket struct {
	Key     string `json:"key"`
	Calls   int64  `json:"calls"`
	Tokens  int64  `json:"tokens"`
	Blocked int64  `json:"blocked"`
}

type statsTotals struct {
	Calls   int64 `json:"calls"`
	Tokens  int64 `json:"tokens"`
	Blocked int64 `json:"blocked"`
}

type statsResponse struct {
	Since        *time.Time    `json:"since,omitempty"`
	Until        *time.Time    `json:"until,omitempty"`
	Totals       statsTotals   `json:"totals"`
	ByUser       []statsBucket `json:"by_user"`
	ByDepartment []statsBucket `json:"by_department"`
	ByChannel    []statsBucket `json:"by_channel"`
	ByModel      []statsBucket `json:"by_model"`
	ByAgentType  []statsBucket `json:"by_agent_type"`
	ByDataLevel  []statsBucket `json:"by_data_level"`
	ByDecision   []statsBucket `json:"by_decision"`
}

// statsEvent reports whether an audit event is a model call in scope
// for usage stats, and whether it was blocked.
func statsEvent(ev audit.Event) (inScope, blocked bool) {
	switch {
	case ev.Kind == "model_call":
		return true, false
	case ev.Kind == "violation" && ev.Action == "model_call_blocked":
		return true, true
	default:
		return false, false
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// aggregateStats folds audit events into per-dimension buckets.
// deptOf maps an actor ID to a department; "" means unknown.
// since/until bound the window (zero value = unbounded, half-open
// [since, until)).
func aggregateStats(events []audit.Event, deptOf func(actorID string) string, since, until time.Time) statsResponse {
	res := statsResponse{}
	if !since.IsZero() {
		t := since.UTC()
		res.Since = &t
	}
	if !until.IsZero() {
		t := until.UTC()
		res.Until = &t
	}
	acc := map[string]map[string]*statsBucket{}
	add := func(dim, key string, tokens int64, blocked bool) {
		m := acc[dim]
		if m == nil {
			m = map[string]*statsBucket{}
			acc[dim] = m
		}
		b := m[key]
		if b == nil {
			b = &statsBucket{Key: key}
			m[key] = b
		}
		b.Calls++
		b.Tokens += tokens
		if blocked {
			b.Blocked++
		}
	}
	for _, ev := range events {
		inScope, blocked := statsEvent(ev)
		if !inScope {
			continue
		}
		if !since.IsZero() && ev.Time.Before(since) {
			continue
		}
		if !until.IsZero() && !ev.Time.Before(until) {
			continue
		}
		res.Totals.Calls++
		res.Totals.Tokens += ev.Tokens
		if blocked {
			res.Totals.Blocked++
		}
		decision := ev.Decision
		if blocked {
			decision = "block"
		}
		add("user", orUnknown(ev.Actor), ev.Tokens, blocked)
		add("department", orUnknown(deptOf(ev.ActorID)), ev.Tokens, blocked)
		add("channel", orNone(ev.Channel), ev.Tokens, blocked)
		add("model", orUnknown(ev.Model), ev.Tokens, blocked)
		add("agent_type", orUnknown(ev.AgentType), ev.Tokens, blocked)
		add("data_level", orUnknown(ev.DataLevel), ev.Tokens, blocked)
		add("decision", orUnknown(decision), ev.Tokens, blocked)
	}
	flatten := func(dim string) []statsBucket {
		m := acc[dim]
		out := make([]statsBucket, 0, len(m))
		for _, b := range m {
			out = append(out, *b)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Calls != out[j].Calls {
				return out[i].Calls > out[j].Calls
			}
			return out[i].Key < out[j].Key
		})
		return out
	}
	res.ByUser = flatten("user")
	res.ByDepartment = flatten("department")
	res.ByChannel = flatten("channel")
	res.ByModel = flatten("model")
	res.ByAgentType = flatten("agent_type")
	res.ByDataLevel = flatten("data_level")
	res.ByDecision = flatten("decision")
	return res
}

func (a *API) getStats(w http.ResponseWriter, r *http.Request, u *store.User) {
	var since, until time.Time
	q := r.URL.Query()
	if s := q.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "since must be RFC3339 (e.g. 2026-11-01T00:00:00Z)")
			return
		}
		since = t
	}
	if s := q.Get("until"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "until must be RFC3339 (e.g. 2026-12-01T00:00:00Z)")
			return
		}
		until = t
	}
	if !until.IsZero() && !since.IsZero() && !until.After(since) {
		writeErr(w, http.StatusBadRequest, "until must be after since")
		return
	}
	// Resolve departments once; audit events only carry the actor ID.
	depts := map[string]string{}
	a.st.Read(func(st *store.State) {
		for id, usr := range st.Users {
			depts[id] = usr.Department
		}
	})
	deptOf := func(actorID string) string { return depts[actorID] }
	writeJSON(w, http.StatusOK, aggregateStats(a.audit.All(), deptOf, since, until))
}
