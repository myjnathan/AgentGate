// Package api — agent attestation (F14).
//
// POST /api/setup/device/attest lets an agent submit its attestation
// evidence (version / config hash / binary hash / plugin list / guard
// state) bound to a pending device code. Admin endpoints manage the
// appraisal policy per agent type and list recent attestation
// verdicts (read from the audit log, so they are hash-chained like
// everything else).
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/myjnathan/agentgate/internal/attest"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// deviceAttest handles POST /api/setup/device/attest. No session is
// required: the device code + polling secret is the credential, and
// the report is bound to that code (RATS freshness). The verdict is
// returned so the setup tool can tell the employee what happened;
// approval re-evaluates against the policy in force at that moment.
func (a *API) deviceAttest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code   string        `json:"code"`
		Secret string        `json:"secret"`
		Report attest.Report `json:"report"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if in.Code == "" || in.Secret == "" {
		writeErr(w, http.StatusBadRequest, "code and secret are required")
		return
	}
	verdict, err := a.auth.SubmitAttestation(in.Code, in.Secret, in.Report)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	decision := "allow"
	if verdict.Status == attest.VerdictDowngraded {
		decision = "warn"
	}
	agentType := in.Report.AgentType
	a.st.Read(func(st *store.State) {
		if dc, ok := st.DeviceCodes[in.Code]; ok {
			agentType = dc.AgentType // the code, not the client, decides the agent type
		}
	})
	a.audit.Append(audit.Event{AgentType: agentType, Kind: "attestation",
		Action: "attest_submit", Decision: decision,
		Detail: map[string]string{
			"verdict": verdict.Status,
			"reasons": strings.Join(verdict.Reasons, ","),
		}})
	writeJSON(w, http.StatusOK, map[string]any{"verdict": verdict})
}

// listAttestationPolicies handles GET /api/attestation/policies.
func (a *API) listAttestationPolicies(w http.ResponseWriter, r *http.Request, u *store.User) {
	var out []*attest.Policy
	a.st.Read(func(st *store.State) {
		for _, p := range st.AttestationPolicies {
			cp := *p
			out = append(out, &cp)
		}
	})
	if out == nil {
		out = []*attest.Policy{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out})
}

// upsertAttestationPolicy handles POST /api/attestation/policies: one
// policy per agent type; posting again replaces it.
func (a *API) upsertAttestationPolicy(w http.ResponseWriter, r *http.Request, u *store.User) {
	var p attest.Policy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	p.AgentType = strings.ToLower(strings.TrimSpace(p.AgentType))
	if err := p.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.st.WithLock(func(st *store.State) {
		cp := p
		st.AttestationPolicies[p.AgentType] = &cp
	})
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, AgentType: p.AgentType,
		Kind: "admin", Action: "attestation_policy_upsert", Decision: "allow",
		Detail: map[string]string{"agent_type": p.AgentType, "min_version": p.MinVersion}})
	writeJSON(w, http.StatusOK, map[string]any{"policy": p})
}

// deleteAttestationPolicy handles DELETE /api/attestation/policies/{agent}.
func (a *API) deleteAttestationPolicy(w http.ResponseWriter, r *http.Request, u *store.User) {
	agent := strings.ToLower(strings.TrimSpace(r.PathValue("agent")))
	var ok bool
	a.st.WithLock(func(st *store.State) {
		if _, ok = st.AttestationPolicies[agent]; ok {
			delete(st.AttestationPolicies, agent)
		}
	})
	if !ok {
		writeErr(w, http.StatusNotFound, "no policy for agent type")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, AgentType: agent,
		Kind: "admin", Action: "attestation_policy_delete", Decision: "allow",
		Detail: map[string]string{"agent_type": agent}})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// listAttestationReports handles GET /api/attestation/reports: recent
// attestation events from the audit log, newest first.
func (a *API) listAttestationReports(w http.ResponseWriter, r *http.Request, u *store.User) {
	events := a.audit.Query("attestation", "", 50)
	if events == nil {
		events = []audit.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": events})
}
