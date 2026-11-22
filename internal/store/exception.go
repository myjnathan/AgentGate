package store

import "time"

// Exception is one high-data-level call exception request (backlog
// F24) and, after approval, its one-time grant. GrantHash is the
// SHA-256 of the grant token; the plaintext token is returned exactly
// once in the approve response and never stored or returned again.
type Exception struct {
	ID          string    `json:"id"`
	ApplicantID string    `json:"applicant_id"`
	Applicant   string    `json:"applicant"`
	Department  string    `json:"department,omitempty"`
	DataLevel   DataLevel `json:"data_level"`
	// Reason is why the blocked call is needed for the business;
	// Scope describes the intended call (model / task) so the
	// approver can judge it. Both are audit metadata — the gateway
	// does not machine-match scope text to a call; the binding the
	// gateway enforces is applicant + data level + expiry +
	// single use.
	Reason    string `json:"reason"`
	Scope     string `json:"scope"`
	Hours     int    `json:"hours"`
	Status    string `json:"status"`
	GrantHash string `json:"grant_hash,omitempty"`
	// DecidedBy / DecisionNote record the approval decision;
	// UsedAt records the single gateway call that consumed the grant.
	DecidedBy    string     `json:"decided_by,omitempty"`
	DecisionNote string     `json:"decision_note,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	UsedAt       *time.Time `json:"used_at,omitempty"`
}
