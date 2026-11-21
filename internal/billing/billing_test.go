package billing

import (
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

func TestCost(t *testing.T) {
	p := store.ModelPrice{InputPerMillion: 2.50, OutputPerMillion: 10.00}
	// 1,000,000 prompt + 500,000 completion tokens: 2.50 + 5.00.
	if got := Cost(1_000_000, 500_000, p); got != 7.50 {
		t.Fatalf("Cost = %v, want 7.50", got)
	}
	if got := Cost(0, 0, p); got != 0 {
		t.Fatalf("Cost(0,0) = %v, want 0", got)
	}
	// Unpriced model: zero price, zero cost (callers report it as
	// unpriced, not free).
	if got := Cost(1000, 1000, store.ModelPrice{}); got != 0 {
		t.Fatalf("Cost unpriced = %v, want 0", got)
	}
}

func TestMonthWindow(t *testing.T) {
	start, end := MonthWindow(time.Date(2026, 11, 21, 23, 30, 0, 0, time.FixedZone("CST", 8*3600)))
	if !start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("month start = %v, want 2026-11-01 UTC", start)
	}
	if !end.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("month end = %v, want 2026-12-01 UTC", end)
	}
	// February in a leap year ends March 1st.
	_, end = MonthWindow(time.Date(2028, 2, 15, 0, 0, 0, 0, time.UTC))
	if !end.Equal(time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("leap Feb end = %v, want 2028-03-01 UTC", end)
	}
}

func TestDepartmentSpend(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	start, end := MonthWindow(now)
	ev := func(kind string, actorID string, cost float64, at time.Time) audit.Event {
		return audit.Event{Kind: kind, ActorID: actorID, CostUSD: cost, Time: at}
	}
	events := []audit.Event{
		ev("model_call", "u1", 1.50, now),                   // 研发部, in month
		ev("model_call", "u1", 0.25, now.Add(-time.Hour)),   // same dept
		ev("model_call", "u2", 9.99, now),                   // other dept
		ev("model_call", "u1", 5.00, now.AddDate(0, -1, 0)), // last month: excluded
		ev("violation", "u1", 3.00, now),                    // blocked: no cost
		ev("cache_hit", "u1", 0.75, now),                    // cached: no upstream cost
		ev("model_call", "", 2.00, now),                     // unknown dept: excluded
	}
	deptOf := func(id string) string {
		if id == "u1" {
			return "研发部"
		}
		if id == "u2" {
			return "IT"
		}
		return ""
	}
	if got := DepartmentSpend(events, deptOf, "研发部", start, end); got != 1.75 {
		t.Fatalf("DepartmentSpend(研发部) = %v, want 1.75", got)
	}
	if got := DepartmentSpend(events, deptOf, "IT", start, end); got != 9.99 {
		t.Fatalf("DepartmentSpend(IT) = %v, want 9.99", got)
	}
	if got := DepartmentSpend(events, deptOf, "财务部", start, end); got != 0 {
		t.Fatalf("DepartmentSpend(财务部) = %v, want 0", got)
	}
}
