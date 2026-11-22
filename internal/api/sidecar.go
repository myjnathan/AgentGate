package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/myjnathan/agentgate/internal/store"
)

// GET /api/sidecar/stats — endpoint sidecar adoption (F15).
//
// Counts model calls that arrived through the local privacy gateway
// (ag-guard) by scanning the hash-chained audit log for model_call
// and cache_hit events whose Detail carries "sidecar" (set by the
// gateway in internal/gateway when X-AgentGate-Sidecar is present).
// Local blocks never reach the server, so they are not counted here:
// the sidecar's own GET /status on the endpoint is the only place a
// local block is visible. That split is deliberate — the platform
// sees adoption, the endpoint keeps the sensitive detail.
type sidecarStatsResponse struct {
	SidecarCalls     int64             `json:"sidecar_calls"`
	SidecarCacheHits int64             `json:"sidecar_cache_hits"`
	LastSeen         *time.Time        `json:"last_seen,omitempty"`
	ByAgent          []sidecarAgentRow `json:"by_agent"`
}

type sidecarAgentRow struct {
	AgentType string `json:"agent_type"`
	Calls     int64  `json:"calls"`
}

func (a *API) sidecarStats(w http.ResponseWriter, r *http.Request, u *store.User) {
	var resp sidecarStatsResponse
	byAgent := map[string]int64{}
	for _, ev := range a.audit.All() {
		if ev.Detail["sidecar"] == "" {
			continue
		}
		switch ev.Kind {
		case "model_call":
			resp.SidecarCalls++
			// ByAgent counts forwarded model calls only (the same
			// population as sidecar_calls); cache hits are reported
			// separately as sidecar_cache_hits.
			if ev.AgentType != "" {
				byAgent[ev.AgentType]++
			}
		case "cache_hit":
			resp.SidecarCacheHits++
		default:
			continue
		}
		if resp.LastSeen == nil || ev.Time.After(*resp.LastSeen) {
			t := ev.Time
			resp.LastSeen = &t
		}
	}
	for agent, n := range byAgent {
		resp.ByAgent = append(resp.ByAgent, sidecarAgentRow{AgentType: agent, Calls: n})
	}
	sort.Slice(resp.ByAgent, func(i, j int) bool { return resp.ByAgent[i].Calls > resp.ByAgent[j].Calls })
	writeJSON(w, http.StatusOK, resp)
}
