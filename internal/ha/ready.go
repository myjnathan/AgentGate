package ha

import (
	"encoding/json"
	"net/http"
)

// Problems lists the readiness problems implied by the latest write
// health of the two durable stores plus the audit-chain check done at
// startup. Only category codes are returned — never paths or raw
// error text — because /readyz is unauthenticated like /healthz.
func Problems(storeErr, auditErr error, chainOK bool) []string {
	var p []string
	if storeErr != nil {
		p = append(p, "state_save_failed")
	}
	if auditErr != nil {
		p = append(p, "audit_append_failed")
	}
	if !chainOK {
		p = append(p, "audit_chain_invalid")
	}
	return p
}

// ReadyHandler returns an HTTP handler for GET /readyz, the readiness
// probe. It answers the Kubernetes readiness question — "can this node
// take traffic right now?" — as opposed to /healthz, which only says
// the process is alive. A failing node is drained by the load
// balancer, not restarted: the problems it reports (disk full, broken
// audit chain) are not fixed by restarting the process.
//
// storeErr and auditErr are read on every probe and report the most
// recent state-save / audit-append error (nil = healthy); chainOK is
// the audit-chain verification done once at startup.
func ReadyHandler(storeErr, auditErr func() error, chainOK bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		problems := Problems(storeErr(), auditErr(), chainOK)
		w.Header().Set("Content-Type", "application/json")
		if len(problems) == 0 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ready":true}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ready":    false,
			"problems": problems,
		})
	}
}
