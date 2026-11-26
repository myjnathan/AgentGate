// Package auth: password hashing (PBKDF2-HMAC-SHA256), web sessions,
// device-code login for agents, and tiered API tokens.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/myjnathan/agentgate/internal/attest"
	"github.com/myjnathan/agentgate/internal/exception"
	"github.com/myjnathan/agentgate/internal/pluginpol"
	"github.com/myjnathan/agentgate/internal/roles"
	"github.com/myjnathan/agentgate/internal/store"
)

// ErrAccountLocked is returned when the account is inside its
// lockout window; callers use errors.Is to distinguish a lockout
// from a plain bad password (the API audits it as account_locked,
// which is an alertable event — backlog F5).
var ErrAccountLocked = errors.New("account temporarily locked")

const pbkdf2Iter = 210_000

func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdf2Iter,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(dk)), nil
}

func CheckPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(parts[2])
	want, err2 := base64.StdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	dk, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(dk, want) == 1
}

func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Service manages sessions and login throttling.
type Service struct {
	st       *store.Store
	mu       sync.Mutex
	sessions map[string]session // session token -> session
	failures map[string][]time.Time
	// RoleFromGroups maps verified IdP groups to a local role
	// (backlog F26). It is only consulted for OIDC-managed accounts
	// (RoleSource "oidc", or empty on OIDC-provisioned rows) and
	// must only return auditor / operator / employee — never admin
	// or security. Nil means no mapping is configured: OIDC logins
	// never change a role. A locally assigned role (RoleSource
	// "local") is never overwritten by the IdP.
	RoleFromGroups func(groups []string) (string, bool)
}

type session struct {
	userID   string
	username string
	role     string
	expires  time.Time
}

func New(st *store.Store) *Service {
	return &Service{st: st, sessions: map[string]session{}, failures: map[string][]time.Time{}}
}

// Login verifies credentials with account-based lockout:
// 20 failures within 20 minutes lock the account for 5 minutes.
// The lockout is keyed by account, not source IP (OWASP guidance).
func (s *Service) Login(username, password string) (*store.User, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var fails []time.Time
	for _, t := range s.failures[username] {
		if now.Sub(t) < 20*time.Minute {
			fails = append(fails, t)
		}
	}
	if len(fails) >= 20 {
		return nil, "", ErrAccountLocked
	}
	var user *store.User
	s.st.Read(func(st *store.State) {
		for _, u := range st.Users {
			if u.Username == username {
				cp := *u
				user = &cp
			}
		}
	})
	if user == nil || user.Disabled || !CheckPassword(password, user.PasswordHash) {
		s.failures[username] = append(fails, now)
		return nil, "", fmt.Errorf("invalid credentials")
	}
	delete(s.failures, username)
	tok := store.NewSecret("ags_")
	s.sessions[tok] = session{userID: user.ID, username: user.Username, role: user.Role, expires: now.Add(12 * time.Hour)}
	return user, tok, nil
}

// OIDCClaims is the verified identity an OIDC provider hands to the
// login flow (a small local copy of oidc.Claims, so this package does
// not depend on the OIDC client).
type OIDCClaims struct {
	Subject           string
	PreferredUsername string
	Email             string
	DisplayName       string
	Department        string
	// Groups is the IdP group membership, used only by the F26 role
	// mapping (RoleFromGroups). Empty when the IdP sends none.
	Groups []string
}

// RevokeUserSessions deletes every web session of one user
// (backlog F21: org sync disables a departed employee, whose open
// browser sessions must stop working at once).
func (s *Service) RevokeUserSessions(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, sess := range s.sessions {
		if sess.userID == userID {
			delete(s.sessions, tok)
		}
	}
}

// SessionUser resolves a web session token.
func (s *Service) SessionUser(tok string) (userID, username, role string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[tok]
	if !ok || time.Now().After(sess.expires) {
		return "", "", "", false
	}
	return sess.userID, sess.username, sess.role, true
}

// CreateSession issues a 12-hour web session for an already
// authenticated user (used by the OIDC callback after the IdP has
// verified the identity).
func (s *Service) CreateSession(user *store.User) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := store.NewSecret("ags_")
	s.sessions[tok] = session{userID: user.ID, username: user.Username, role: user.Role, expires: time.Now().Add(12 * time.Hour)}
	return tok
}

// LoginOIDC finds or just-in-time provisions the user for a verified
// OIDC identity. The IdP is an identity source only, never a
// privilege source: a new user always starts as role "employee" and
// an admin promotes them locally. A disabled account cannot log in,
// and an OIDC identity can never take over a local account: if the
// derived username belongs to a different (local) user, a suffix is
// added instead of reusing it.
//
// Role mapping (backlog F26): when RoleFromGroups is set, an
// OIDC-managed account's role follows the admin-configured group
// mapping on every login — a mapped group yields that role
// (auditor / operator / employee only, never admin or security),
// no mapped group yields employee. A role an admin assigned locally
// (RoleSource "local") is never overwritten by the IdP.
func (s *Service) LoginOIDC(claims OIDCClaims) (*store.User, error) {
	if claims.Subject == "" {
		return nil, fmt.Errorf("oidc identity has no subject")
	}
	var user *store.User
	isNew := false
	s.st.WithLock(func(st *store.State) {
		for _, u := range st.Users {
			if u.OIDCSubject != "" && u.OIDCSubject == claims.Subject && u.AuthSource == "oidc" {
				cp := *u
				user = &cp
			}
		}
		if user != nil {
			return
		}
		isNew = true
		username := oidcUsername(claims)
		// Never collide with an existing local account.
		if _, taken := usernameTaken(st, username); taken {
			h := sha256.Sum256([]byte(claims.Subject))
			username = username + "-" + hex.EncodeToString(h[:])[:8]
		}
		display := claims.DisplayName
		if display == "" {
			display = username
		}
		role := "employee"
		if s.RoleFromGroups != nil {
			if r, ok := s.RoleFromGroups(claims.Groups); ok && roles.MappableViaIdP(r) {
				role = r
			}
		}
		user = &store.User{
			ID: store.NewID("usr"), Username: username, DisplayName: display,
			Department: claims.Department, Role: role, RoleSource: "oidc",
			OIDCSubject: claims.Subject, AuthSource: "oidc",
		}
		st.Users[user.ID] = user
	})
	if user == nil {
		return nil, fmt.Errorf("oidc provisioning failed")
	}
	if user.Disabled {
		return nil, fmt.Errorf("account disabled")
	}
	// Refresh the display name / department from the IdP on each login
	// (the IdP owns the profile; the role stays locally managed unless
	// it is IdP-managed through the F26 mapping).
	refreshName := claims.DisplayName != "" && user.DisplayName != claims.DisplayName
	refreshDept := user.Department != claims.Department
	mappedRole := ""
	if !isNew && s.RoleFromGroups != nil &&
		(user.RoleSource == "oidc" || (user.RoleSource == "" && user.AuthSource == "oidc")) {
		if r, ok := s.RoleFromGroups(claims.Groups); ok && roles.MappableViaIdP(r) {
			mappedRole = r
		} else {
			mappedRole = "employee"
		}
	}
	refreshRole := mappedRole != "" && mappedRole != user.Role
	if refreshName || refreshDept || refreshRole {
		s.st.WithLock(func(st *store.State) {
			if u, ok := st.Users[user.ID]; ok {
				if claims.DisplayName != "" {
					u.DisplayName = claims.DisplayName
				}
				u.Department = claims.Department
				if refreshRole {
					u.Role = mappedRole
					u.RoleSource = "oidc"
				}
				cp := *u
				user = &cp
			}
		})
	}
	_ = s.st.Save()
	return user, nil
}

func usernameTaken(st *store.State, username string) (*store.User, bool) {
	for _, u := range st.Users {
		if u.Username == username {
			return u, true
		}
	}
	return nil, false
}

// oidcUsername derives a local username from the verified claims:
// preferred_username first, then the email local part, then a
// subject-derived fallback. Only safe characters are kept.
func oidcUsername(c OIDCClaims) string {
	cand := c.PreferredUsername
	if cand == "" && c.Email != "" {
		cand = strings.SplitN(c.Email, "@", 2)[0]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(cand) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		h := sha256.Sum256([]byte(c.Subject))
		return "oidc-" + hex.EncodeToString(h[:])[:8]
	}
	return b.String()
}

// StartDeviceCode begins a device-code login for an agent.
func (s *Service) StartDeviceCode(agentType string, tier store.Tier) *store.DeviceCode {
	dc := &store.DeviceCode{
		Code:      "AG-" + strings.ToUpper(store.NewSecret("")[:6]),
		Secret:    store.NewSecret("poll_"),
		AgentType: agentType,
		Tier:      tier,
		Status:    "pending",
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	s.st.WithLock(func(st *store.State) { st.DeviceCodes[dc.Code] = dc })
	_ = s.st.Save()
	cp := *dc
	return &cp
}

// SubmitAttestation attaches an attestation report to a pending
// device code and evaluates it against the current policy for that
// agent type (backlog F14). The report is bound to this code (code +
// polling secret, single use, 10-minute expiry), which plays the RATS
// freshness role: a report cannot be replayed against a later login.
// The returned verdict is a preview; approval re-evaluates against
// the policy in force at that moment. A missing policy means no
// attestation requirement: the verdict is "unverified".
func (s *Service) SubmitAttestation(code, secret string, report attest.Report) (attest.Verdict, error) {
	if err := report.Validate(); err != nil {
		return attest.Verdict{}, err
	}
	var verdict attest.Verdict
	var err error
	s.st.WithLock(func(st *store.State) {
		dc, ok := st.DeviceCodes[code]
		if !ok || dc.Secret != secret || dc.Status != "pending" || time.Now().After(dc.ExpiresAt) {
			err = fmt.Errorf("device code invalid or expired")
			return
		}
		report.AgentType = dc.AgentType // the code, not the client, decides the agent type
		cp := report
		dc.Attestation = &cp
		if pol, ok := st.AttestationPolicies[dc.AgentType]; ok && pol != nil {
			verdict = attest.Evaluate(&cp, *pol)
		} else {
			verdict = attest.Verdict{Status: attest.VerdictUnverified}
		}
		dc.AttestVerdict = verdict.Status
		dc.AttestReasons = verdict.Reasons
	})
	if err == nil {
		_ = s.st.Save()
	}
	return verdict, err
}

// containsReason reports whether a verdict reason list already
// holds code (F38 merges plugin reasons into the F14 verdict).
func containsReason(reasons []string, code string) bool {
	for _, r := range reasons {
		if r == code {
			return true
		}
	}
	return false
}

// evaluateAttestation is the approval-time appraisal: the stored
// report (if any) against the policy in force now. No policy means
// "unverified"; no report means the policy's RequireReport decides
// between "downgraded" (report_missing) and "unverified".
func evaluateAttestation(dc *store.DeviceCode, policies map[string]*attest.Policy) attest.Verdict {
	pol, ok := policies[dc.AgentType]
	if !ok || pol == nil {
		return attest.Verdict{Status: attest.VerdictUnverified}
	}
	return attest.Evaluate(dc.Attestation, *pol)
}

// ApproveDeviceCode is called from the web UI by a signed-in user.
// It returns the plaintext token (delivered once, on poll) and the
// tier actually issued. A managed-tier request is only honoured when
// attestation passes (backlog F14): a failed or missing-required
// attestation downgrades the token to BYOA instead of refusing the
// login — the employee keeps working with general capabilities, and
// the downgrade is audited and alerted by the caller.
func (s *Service) ApproveDeviceCode(code string, user *store.User, deviceName string) (string, store.Tier, error) {
	var plain string
	var issued store.Tier
	var err error
	s.st.WithLock(func(st *store.State) {
		dc, ok := st.DeviceCodes[code]
		if !ok || dc.Status != "pending" || time.Now().After(dc.ExpiresAt) {
			err = fmt.Errorf("device code invalid or expired")
			return
		}
		verdict := evaluateAttestation(dc, st.AttestationPolicies)
		// F38: plugin policy appraisal, the same evaluation the
		// heartbeat uses (login and drift share one function, so
		// the verdict is identical in both places). It runs here,
		// not at SubmitAttestation: the scope needs the approving
		// user's department, which is only known at approval time.
		// Groups are not modelled yet, so the group list is empty
		// for now. observe/warn record the reasons without
		// changing the verdict; downgrade/block_sensitive
		// downgrade (block_sensitive's extra cut of confidential
		// channels lands in slice 2). Slice 3: the approving
		// user's approved plugin exceptions are honoured, so a
		// waived plugin does not downgrade this login.
		excepted := exception.ExceptedPlugins(st.Exceptions, user.ID, time.Now())
		if reasons, _, action := pluginpol.EvaluateReportWithExceptions(dc.Attestation, st.PluginPolicies, excepted, user.Department, nil, time.Now()); len(reasons) > 0 {
			for _, r := range reasons {
				if !containsReason(verdict.Reasons, r) {
					verdict.Reasons = append(verdict.Reasons, r)
				}
			}
			if action == pluginpol.ActionDowngrade || action == pluginpol.ActionBlockSensitive {
				verdict.Status = attest.VerdictDowngraded
			}
		}
		dc.AttestVerdict = verdict.Status
		dc.AttestReasons = verdict.Reasons
		issued = dc.Tier
		attested := false
		if dc.Tier == store.TierManaged {
			switch verdict.Status {
			case attest.VerdictPass:
				attested = true
			case attest.VerdictDowngraded:
				issued = store.TierBYOA
			case attest.VerdictUnverified:
				// No applicable policy, or the policy does not
				// require a report: honour the requested tier but
				// mark the token unverified.
			}
		} else if verdict.Status == attest.VerdictPass {
			attested = true
		}
		dev := &store.Device{ID: store.NewID("dev"), UserID: user.ID, Name: deviceName, LastSeen: time.Now()}
		st.Devices[dev.ID] = dev
		plain = store.NewSecret("ag_at_")
		tok := &store.APIToken{
			ID: store.NewID("tok"), Hash: HashToken(plain), UserID: user.ID,
			DeviceID: dev.ID, AgentType: dc.AgentType, Tier: issued,
			ExpiresAt: time.Now().Add(30 * 24 * time.Hour), CreatedAt: time.Now(),
			Attested: attested, AttestVerdict: verdict.Status,
		}
		st.Tokens[tok.ID] = tok
		dc.Status = "approved"
		dc.TokenID = tok.ID
		dc.Token = plain
	})
	if err == nil {
		_ = s.st.Save()
	}
	return plain, issued, err
}

// PollDeviceCode is called by the setup tool. The plaintext token is
// returned exactly once, on the first successful poll after approval.
func (s *Service) PollDeviceCode(code, secret string) (status, token string) {
	var out string
	s.st.WithLock(func(st *store.State) {
		dc, ok := st.DeviceCodes[code]
		if !ok || dc.Secret != secret || time.Now().After(dc.ExpiresAt) {
			out = "expired"
			return
		}
		switch dc.Status {
		case "approved":
			token = dc.Token
			dc.Token = "" // deliver once
			out = "approved"
		case "denied":
			out = "denied"
		default:
			out = "pending"
		}
	})
	return out, token
}

// ResolveToken resolves an agent API token to its record and owner.
func (s *Service) ResolveToken(plain string) (*store.APIToken, *store.User, bool) {
	h := HashToken(plain)
	var tok *store.APIToken
	var user *store.User
	s.st.Read(func(st *store.State) {
		for _, t := range st.Tokens {
			if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) == 1 && !t.Revoked && time.Now().Before(t.ExpiresAt) {
				cp := *t
				tok = &cp
				if u, ok := st.Users[t.UserID]; ok && !u.Disabled {
					uc := *u
					user = &uc
				}
			}
		}
	})
	if tok == nil || user == nil {
		return nil, nil, false
	}
	return tok, user, true
}
