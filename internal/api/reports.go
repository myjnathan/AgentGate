// Periodic compliance reports (backlog F25).
//
// POST /api/reports generates a frozen weekly or monthly report from
// the hash-chained audit log plus a point-in-time snapshot of channel
// health, tokens and terminals (see internal/report). GET /api/reports
// lists generated reports, GET /api/reports/{id} previews one, GET
// /api/reports/{id}/export downloads it as a CSV table, DELETE removes
// one. All endpoints are admin / security only: a report is a
// management review artifact (the auditor role that reads reports
// without changing policy arrives with F26).
//
// A report contains statistics and metadata only — no request or
// response text, no matched sensitive values — and generation, export
// and deletion are themselves audited (counts and IDs only).
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/report"
	"github.com/myjnathan/agentgate/internal/store"
)

// reportSummary is the list view of a generated report: identity and
// headline numbers, not the full breakdown (preview fetches that).
type reportSummary struct {
	ID          string    `json:"id"`
	Period      string    `json:"period"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	GeneratedAt time.Time `json:"generated_at"`
	GeneratedBy string    `json:"generated_by"`
	Calls       int64     `json:"calls"`
	Tokens      int64     `json:"tokens"`
	Blocked     int64     `json:"blocked"`
	CostUSD     float64   `json:"cost_usd"`
}

func toReportSummary(rep *report.Report) reportSummary {
	return reportSummary{
		ID: rep.ID, Period: rep.Period, PeriodStart: rep.PeriodStart, PeriodEnd: rep.PeriodEnd,
		GeneratedAt: rep.GeneratedAt, GeneratedBy: rep.GeneratedBy,
		Calls: rep.Calls, Tokens: rep.Tokens, Blocked: rep.Blocked, CostUSD: rep.CostUSD,
	}
}

// buildReportSnapshot freezes the point-in-time platform state the
// report pairs with its audit window: channel health, token counts
// and terminal counts as observed right now.
func (a *API) buildReportSnapshot() report.Snapshot {
	now := time.Now()
	snap := report.Snapshot{DeptOf: func(string) string { return "" }}
	a.st.Read(func(st *store.State) {
		depts := map[string]string{}
		for id, usr := range st.Users {
			depts[id] = usr.Department
		}
		snap.DeptOf = func(actorID string) string { return depts[actorID] }
		for _, ch := range st.Channels {
			snap.Channels = append(snap.Channels, report.ChannelHealth{
				Name: ch.Name, Health: ch.Health(now), ConsecutiveFailures: ch.ConsecutiveFailures})
		}
		sort.Slice(snap.Channels, func(i, j int) bool { return snap.Channels[i].Name < snap.Channels[j].Name })
		for _, tok := range st.Tokens {
			snap.Tokens.Total++
			switch tok.Tier {
			case store.TierManaged:
				snap.Tokens.Managed++
			default:
				snap.Tokens.BYOA++
			}
			if tok.Attested {
				snap.Tokens.Attested++
			}
			if tok.Revoked {
				snap.Tokens.Revoked++
			} else if !tok.ExpiresAt.IsZero() && tok.ExpiresAt.Before(now) {
				snap.Tokens.Expired++
			}
		}
		for _, term := range st.Terminals {
			snap.Terminals.Total++
			switch term.Status(now) {
			case "online":
				snap.Terminals.Online++
			case "stale":
				snap.Terminals.Stale++
			default:
				snap.Terminals.Offline++
			}
			if term.IsDrifted() {
				snap.Terminals.Drifted++
			}
		}
	})
	snap.ChainValid = a.audit.Verify()
	return snap
}

func (a *API) generateReport(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Period string `json:"period"` // weekly | monthly
		Date   string `json:"date"`   // YYYY-MM-DD, default today (UTC)
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if !report.ValidatePeriod(in.Period) {
		writeErr(w, http.StatusBadRequest, "period must be weekly or monthly")
		return
	}
	ref := time.Now().UTC()
	if in.Date != "" {
		t, err := time.Parse("2006-01-02", in.Date)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
			return
		}
		ref = t
	}
	start, end, err := report.PeriodWindow(in.Period, ref)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rep := report.Generate(a.audit.All(), a.buildReportSnapshot(), in.Period, start, end, u.Username)
	rep.ID = store.NewID("rep")
	a.st.WithLock(func(st *store.State) {
		st.Reports[rep.ID] = rep
		// Retain at most a year of weekly reports; drop the oldest
		// first so the store cannot grow without bound.
		for len(st.Reports) > report.MaxStoredReports {
			oldestID := ""
			var oldest time.Time
			for id, r := range st.Reports {
				if oldestID == "" || r.GeneratedAt.Before(oldest) {
					oldestID, oldest = id, r.GeneratedAt
				}
			}
			delete(st.Reports, oldestID)
		}
	})
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "report_generated",
		Detail: map[string]string{"report_id": rep.ID, "period": rep.Period,
			"period_start": start.Format(time.RFC3339), "period_end": end.Format(time.RFC3339),
			"events": fmt.Sprintf("%d", rep.Audit.EventsInWindow)}})
	writeJSON(w, http.StatusCreated, rep)
}

func (a *API) listReports(w http.ResponseWriter, r *http.Request, u *store.User) {
	out := []reportSummary{}
	a.st.Read(func(st *store.State) {
		for _, rep := range st.Reports {
			out = append(out, toReportSummary(rep))
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].GeneratedAt.After(out[j].GeneratedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"reports": out})
}

func (a *API) getReport(w http.ResponseWriter, r *http.Request, u *store.User) {
	var rep *report.Report
	a.st.Read(func(st *store.State) {
		if v, ok := st.Reports[r.PathValue("id")]; ok {
			cp := *v
			rep = &cp
		}
	})
	if rep == nil {
		writeErr(w, http.StatusNotFound, "report not found")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (a *API) exportReport(w http.ResponseWriter, r *http.Request, u *store.User) {
	var rep *report.Report
	a.st.Read(func(st *store.State) {
		if v, ok := st.Reports[r.PathValue("id")]; ok {
			cp := *v
			rep = &cp
		}
	})
	if rep == nil {
		writeErr(w, http.StatusNotFound, "report not found")
		return
	}
	var buf bytes.Buffer
	if err := report.WriteCSV(&buf, rep); err != nil {
		writeErr(w, http.StatusInternalServerError, "export failed")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="agentgate-report-%s.csv"`, rep.ID))
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
	// Record the export itself (after the body, so it is never part
	// of its own export). ID and period only.
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "report_exported",
		Detail: map[string]string{"report_id": rep.ID, "period": rep.Period}})
}

func (a *API) deleteReport(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var found bool
	a.st.WithLock(func(st *store.State) {
		if _, ok := st.Reports[id]; ok {
			delete(st.Reports, id)
			found = true
		}
	})
	if !found {
		writeErr(w, http.StatusNotFound, "report not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "report_deleted",
		Detail: map[string]string{"report_id": id}})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
