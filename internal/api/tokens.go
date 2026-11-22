// Package api — token management (F3).
//
// GET /api/tokens lists agent tokens with their rate limits
// (admin/security only). PATCH /api/tokens/{id} adjusts a token's
// RPM/TPM limits. Token hashes are never exposed; only the token ID
// and metadata leave the server.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// tokenView is the admin-facing token shape: metadata and limits,
// never the token hash or plaintext. Attested/AttestVerdict report
// the attestation result at issuance (backlog F14).
type tokenView struct {
	ID            string     `json:"id"`
	UserID        string     `json:"user_id"`
	Username      string     `json:"username"`
	AgentType     string     `json:"agent_type"`
	Tier          store.Tier `json:"tier"`
	Attested      bool       `json:"attested"`
	AttestVerdict string     `json:"attest_verdict,omitempty"`
	RPMLimit      int        `json:"rpm_limit"`
	TPMLimit      int        `json:"tpm_limit"`
	MonthlyCap    int64      `json:"monthly_cap_tokens"`
	UsedTokens    int64      `json:"used_tokens"`
	ExpiresAt     time.Time  `json:"expires_at"`
	Revoked       bool       `json:"revoked"`
	CreatedAt     time.Time  `json:"created_at"`
}

func (a *API) listTokens(w http.ResponseWriter, r *http.Request, u *store.User) {
	users := map[string]string{}
	var out []tokenView
	a.st.Read(func(st *store.State) {
		for id, usr := range st.Users {
			users[id] = usr.Username
		}
		for _, tk := range st.Tokens {
			out = append(out, tokenView{ID: tk.ID, UserID: tk.UserID, Username: users[tk.UserID],
				AgentType: tk.AgentType, Tier: tk.Tier, Attested: tk.Attested, AttestVerdict: tk.AttestVerdict,
				RPMLimit: tk.RPM, TPMLimit: tk.TPM,
				MonthlyCap: tk.MonthlyCap, UsedTokens: tk.UsedTokens,
				ExpiresAt: tk.ExpiresAt, Revoked: tk.Revoked, CreatedAt: tk.CreatedAt})
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (a *API) updateTokenLimits(w http.ResponseWriter, r *http.Request, u *store.User) {
	id := r.PathValue("id")
	var in struct {
		RPMLimit *int `json:"rpm_limit"`
		TPMLimit *int `json:"tpm_limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if (in.RPMLimit != nil && *in.RPMLimit < 0) || (in.TPMLimit != nil && *in.TPMLimit < 0) {
		writeErr(w, http.StatusBadRequest, "rate limits must be >= 0")
		return
	}
	var updated *tokenView
	a.st.WithLock(func(st *store.State) {
		tk, ok := st.Tokens[id]
		if !ok {
			return
		}
		if in.RPMLimit != nil {
			tk.RPM = *in.RPMLimit
		}
		if in.TPMLimit != nil {
			tk.TPM = *in.TPMLimit
		}
		username := ""
		if usr, ok := st.Users[tk.UserID]; ok {
			username = usr.Username
		}
		v := tokenView{ID: tk.ID, UserID: tk.UserID, Username: username,
			AgentType: tk.AgentType, Tier: tk.Tier, RPMLimit: tk.RPM, TPMLimit: tk.TPM,
			MonthlyCap: tk.MonthlyCap, UsedTokens: tk.UsedTokens,
			ExpiresAt: tk.ExpiresAt, Revoked: tk.Revoked, CreatedAt: tk.CreatedAt}
		updated = &v
	})
	if updated == nil {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	_ = a.st.Save()
	a.audit.Append(audit.Event{Actor: u.Username, ActorID: u.ID, Kind: "admin", Action: "token_rate_limit_updated",
		Decision: "allow", Detail: map[string]string{"token_id": id,
			"rpm_limit": strconv.Itoa(updated.RPMLimit),
			"tpm_limit": strconv.Itoa(updated.TPMLimit)}})
	writeJSON(w, http.StatusOK, updated)
}
