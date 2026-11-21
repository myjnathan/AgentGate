// Package api — department monthly budgets (F13).
//
// GET /api/budgets lists every budget with this month's spend so far
// (computed live from the audit log, the same source stats uses),
// POST creates one, PATCH changes its cap or alert threshold, DELETE
// removes it. At most one budget per department; a budget is a hard
// cap — once the department's spend this calendar month (UTC) reaches
// it, the gateway rejects further model calls with 429
// department_budget_exceeded.
package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/billing"
	"github.com/myjnathan/agentgate/internal/store"
)

// budgetView is one department budget as the console sees it,
// including this month's spend so far.
type budgetView struct {
	ID             string    `json:"id"`
	Department     string    `json:"department"`
	MonthlyCapUSD  float64   `json:"monthly_cap_usd"`
	AlertThreshold float64   `json:"alert_threshold"`
	SpentUSD       float64   `json:"spent_usd"`
	RemainingUSD   float64   `json:"remaining_usd"`
	PercentUsed    float64   `json:"percent_used"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// validateBudgetInput rejects the mistakes that would make a budget
// meaningless: an empty department, a non-positive cap, or a
// threshold outside (0, 1] (0 disables the warning).
func validateBudgetInput(department string, capUSD, threshold float64) string {
	if department == "" {
		return "department required"
	}
	if capUSD <= 0 {
		return "monthly_cap_usd must be positive"
	}
	if threshold < 0 || threshold > 1 {
		return "alert_threshold must be between 0 and 1 (0 disables the warning)"
	}
	return ""
}

func (a *API) budgetViews() []budgetView {
	start, end := billing.MonthWindow(time.Now())
	depts := map[string]string{}
	var budgets []*store.DepartmentBudget
	a.st.Read(func(st *store.State) {
		for id, usr := range st.Users {
			depts[id] = usr.Department
		}
		for _, b := range st.Budgets {
			budgets = append(budgets, b)
		}
	})
	deptOf := func(actorID string) string { return depts[actorID] }
	events := a.audit.All()
	out := make([]budgetView, 0, len(budgets))
	for _, b := range budgets {
		spent := billing.DepartmentSpend(events, deptOf, b.Department, start, end)
		remaining := b.MonthlyCapUSD - spent
		if remaining < 0 {
			remaining = 0
		}
		percent := 0.0
		if b.MonthlyCapUSD > 0 {
			percent = spent / b.MonthlyCapUSD
		}
		out = append(out, budgetView{ID: b.ID, Department: b.Department,
			MonthlyCapUSD: b.MonthlyCapUSD, AlertThreshold: b.AlertThreshold,
			SpentUSD: spent, RemainingUSD: remaining, PercentUsed: percent,
			CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Department < out[j].Department })
	return out
}

func (a *API) listBudgets(w http.ResponseWriter, r *http.Request, u *store.User) {
	writeJSON(w, http.StatusOK, map[string]any{"budgets": a.budgetViews()})
}

func (a *API) createBudget(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		Department     string  `json:"department"`
		MonthlyCapUSD  float64 `json:"monthly_cap_usd"`
		AlertThreshold float64 `json:"alert_threshold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid budget")
		return
	}
	if msg := validateBudgetInput(in.Department, in.MonthlyCapUSD, in.AlertThreshold); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	var dup bool
	a.st.Read(func(st *store.State) {
		for _, b := range st.Budgets {
			if b.Department == in.Department {
				dup = true
			}
		}
	})
	if dup {
		writeErr(w, http.StatusConflict, "department already has a budget (patch it instead)")
		return
	}
	now := time.Now().UTC()
	b := &store.DepartmentBudget{ID: store.NewID("bud"), Department: in.Department,
		MonthlyCapUSD: in.MonthlyCapUSD, AlertThreshold: in.AlertThreshold,
		CreatedAt: now, UpdatedAt: now}
	a.st.WithLock(func(st *store.State) { st.Budgets[b.ID] = b })
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "budget_created",
		Detail: map[string]string{"department": b.Department}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": b.ID, "department": b.Department,
		"monthly_cap_usd": b.MonthlyCapUSD, "alert_threshold": b.AlertThreshold})
}

func (a *API) updateBudget(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		MonthlyCapUSD  *float64 `json:"monthly_cap_usd"`
		AlertThreshold *float64 `json:"alert_threshold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid budget")
		return
	}
	if in.MonthlyCapUSD == nil && in.AlertThreshold == nil {
		writeErr(w, http.StatusBadRequest, "nothing to update")
		return
	}
	if in.MonthlyCapUSD != nil && *in.MonthlyCapUSD <= 0 {
		writeErr(w, http.StatusBadRequest, "monthly_cap_usd must be positive")
		return
	}
	if in.AlertThreshold != nil && (*in.AlertThreshold < 0 || *in.AlertThreshold > 1) {
		writeErr(w, http.StatusBadRequest, "alert_threshold must be between 0 and 1 (0 disables the warning)")
		return
	}
	var b *store.DepartmentBudget
	a.st.WithLock(func(st *store.State) {
		if x, ok := st.Budgets[r.PathValue("id")]; ok {
			if in.MonthlyCapUSD != nil {
				x.MonthlyCapUSD = *in.MonthlyCapUSD
			}
			if in.AlertThreshold != nil {
				x.AlertThreshold = *in.AlertThreshold
			}
			x.UpdatedAt = time.Now().UTC()
			b = x
		}
	})
	if b == nil {
		writeErr(w, http.StatusNotFound, "budget not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "budget_updated",
		Detail: map[string]string{"department": b.Department}})
	writeJSON(w, http.StatusOK, map[string]any{"id": b.ID, "department": b.Department,
		"monthly_cap_usd": b.MonthlyCapUSD, "alert_threshold": b.AlertThreshold})
}

func (a *API) deleteBudget(w http.ResponseWriter, r *http.Request, u *store.User) {
	var b *store.DepartmentBudget
	a.st.WithLock(func(st *store.State) {
		if x, ok := st.Budgets[r.PathValue("id")]; ok {
			b = x
			delete(st.Budgets, r.PathValue("id"))
		}
	})
	if b == nil {
		writeErr(w, http.StatusNotFound, "budget not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "budget_deleted",
		Detail: map[string]string{"department": b.Department}})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": b.ID})
}
