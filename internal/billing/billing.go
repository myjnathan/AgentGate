// Package billing prices model calls and enforces department
// monthly budgets (backlog F13).
//
// The pricing model follows LiteLLM's cost table shape: every model
// carries an input price and an output price per million tokens, in
// USD, configured per channel (the same model can cost differently
// from different providers). LiteLLM's own price table covers real
// provider models; AgentGate channels serve admin-configured models
// (including mock and local ones), so the table itself is not copied
// — only the input/output per-million shape and the
// completion_cost formula are reused.
//
// Budgets follow Portkey's monthly semantics: the month is the
// calendar month in UTC, resetting on the 1st at 00:00 UTC, and the
// cap is a hard cap — once a department's spend this month reaches
// it, the next model call is rejected (LiteLLM's "the very next
// call is rejected" rule), never the call already in flight.
package billing

import (
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// Cost prices one call: prompt tokens at the input rate plus
// completion tokens at the output rate, per million tokens, in USD.
// A zero price means the model is unpriced and the call costs
// nothing on paper — callers must not mistake that for "free".
func Cost(promptTokens, completionTokens int64, p store.ModelPrice) float64 {
	return float64(promptTokens)/1e6*p.InputPerMillion +
		float64(completionTokens)/1e6*p.OutputPerMillion
}

// MonthWindow returns the calendar month containing now, in UTC:
// [1st 00:00 UTC, 1st of next month 00:00 UTC).
func MonthWindow(now time.Time) (start, end time.Time) {
	u := now.UTC()
	start = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	end = start.AddDate(0, 1, 0)
	return start, end
}

// DepartmentSpend sums the priced cost of a department's model calls
// in [start, end). Only Kind "model_call" events count — blocked
// calls, cache hits and admin events never consumed upstream tokens,
// so they cost nothing. deptOf maps an audit actor ID to a
// department; "" means unknown and never matches a named budget.
func DepartmentSpend(events []audit.Event, deptOf func(actorID string) string, dept string, start, end time.Time) float64 {
	var total float64
	for _, ev := range events {
		if ev.Kind != "model_call" {
			continue
		}
		if ev.Time.Before(start) || !ev.Time.Before(end) {
			continue
		}
		if deptOf(ev.ActorID) != dept {
			continue
		}
		total += ev.CostUSD
	}
	return total
}
