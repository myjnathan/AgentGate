package auth_test

import (
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/attest"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/store"
)

// newServiceWithUser returns an auth Service backed by a fresh store
// containing one enabled user with the given password.
func newServiceWithUser(t *testing.T, username, password string) (*auth.Service, *store.Store, *store.User) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	u := &store.User{
		ID: store.NewID("usr"), Username: username, DisplayName: "Test User",
		PasswordHash: h, Department: "研发部", Role: "employee",
	}
	st.WithLock(func(s *store.State) { s.Users[u.ID] = u })
	return auth.New(st), st, u
}

func TestCheckPasswordCorrectAndWrong(t *testing.T) {
	encoded, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword("correct horse battery staple", encoded) {
		t.Fatal("correct password should verify")
	}
	if auth.CheckPassword("wrong password", encoded) {
		t.Fatal("wrong password must not verify")
	}
	if auth.CheckPassword("", encoded) {
		t.Fatal("empty password must not verify")
	}
	// Malformed encodings must fail closed, not panic.
	for _, bad := range []string{"", "garbage", "pbkdf2$only$three", "bcrypt$210000$AAAA$BBBB", "pbkdf2$210000$!!!$BBBB"} {
		if auth.CheckPassword("anything", bad) {
			t.Fatalf("malformed hash %q must not verify", bad)
		}
	}
}

func TestDeviceCodeFlowDeliversTokenOnce(t *testing.T) {
	svc, _, user := newServiceWithUser(t, "alice", "demo12345")

	dc := svc.StartDeviceCode("dsh", store.TierManaged)
	if dc.Code == "" || dc.Secret == "" {
		t.Fatal("device code should carry a user code and a polling secret")
	}

	// Poll before approval: pending, no token.
	status, token := svc.PollDeviceCode(dc.Code, dc.Secret)
	if status != "pending" || token != "" {
		t.Fatalf("before approval: got status=%q token=%q, want pending and empty token", status, token)
	}
	// Wrong polling secret must not leak status or token.
	status, token = svc.PollDeviceCode(dc.Code, "poll_wrongsecret")
	if status != "expired" || token != "" {
		t.Fatalf("wrong secret: got status=%q token=%q, want expired and empty token", status, token)
	}

	if _, _, err := svc.ApproveDeviceCode(dc.Code, user, "test laptop"); err != nil {
		t.Fatal(err)
	}

	// First poll after approval delivers the plaintext token.
	status, token = svc.PollDeviceCode(dc.Code, dc.Secret)
	if status != "approved" || token == "" {
		t.Fatalf("first poll: got status=%q token empty=%v, want approved with a token", status, token == "")
	}
	if !strings.HasPrefix(token, "ag_at_") {
		t.Fatalf("delivered token %q should carry the ag_at_ prefix", token)
	}
	delivered := token

	// Second poll must NOT return the token again (delivered exactly once).
	status, token = svc.PollDeviceCode(dc.Code, dc.Secret)
	if token != "" {
		t.Fatalf("second poll returned the token again (%q); it must be delivered exactly once", token)
	}
	_ = status

	// The delivered token resolves to the approving user and managed tier.
	tok, owner, ok := svc.ResolveToken(delivered)
	if !ok {
		t.Fatal("delivered token should resolve")
	}
	if owner.ID != user.ID || tok.Tier != store.TierManaged || tok.AgentType != "dsh" {
		t.Fatalf("resolved token mismatch: owner=%v tier=%v agent=%v", owner.Username, tok.Tier, tok.AgentType)
	}
}

func TestApproveUnknownDeviceCodeFails(t *testing.T) {
	svc, _, user := newServiceWithUser(t, "alice", "demo12345")
	if _, _, err := svc.ApproveDeviceCode("AG-NOPE", user, "x"); err == nil {
		t.Fatal("approving an unknown device code must fail")
	}
}

func TestLoginLockoutAfterTwentyFailures(t *testing.T) {
	svc, _, _ := newServiceWithUser(t, "bob", "demo12345")

	// A few failures must not lock the account yet: correct login still works.
	for i := 0; i < 3; i++ {
		if _, _, err := svc.Login("bob", "wrong"); err == nil {
			t.Fatal("wrong password must fail")
		}
	}
	if _, _, err := svc.Login("bob", "demo12345"); err != nil {
		t.Fatalf("correct login after 3 failures should succeed, got %v", err)
	}

	// Drive the failure counter to the lockout threshold (20 failures).
	for i := 0; i < 20; i++ {
		if _, _, err := svc.Login("bob", "wrong"); err == nil {
			t.Fatalf("failure attempt %d unexpectedly succeeded", i+1)
		}
	}
	// Now even the correct password is refused with a lockout error.
	_, _, err := svc.Login("bob", "demo12345")
	if err == nil {
		t.Fatal("account should be locked after 20 failures")
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Fatalf("expected lockout error, got %v", err)
	}

	// Lockout is keyed per account: a different (unknown) account gets a
	// plain invalid-credentials error, not bob's lockout.
	_, _, err = svc.Login("mallory", "whatever")
	if err == nil || strings.Contains(err.Error(), "locked") {
		t.Fatalf("unknown account should get invalid credentials (not lockout), got %v", err)
	}
}

func TestLoginOIDCProvisionsEmployee(t *testing.T) {
	svc, st, _ := newServiceWithUser(t, "admin", "demo12345")

	claims := auth.OIDCClaims{Subject: "oidc-sub-1", PreferredUsername: "Alice",
		Email: "alice@example.test", DisplayName: "Alice Demo", Department: "研发部"}
	user, err := svc.LoginOIDC(claims)
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "alice" || user.Role != "employee" || user.AuthSource != "oidc" ||
		user.OIDCSubject != "oidc-sub-1" || user.Department != "研发部" {
		t.Fatalf("provisioned user mismatch: %+v", user)
	}
	if user.PasswordHash != "" {
		t.Fatal("OIDC user must have no local password hash")
	}
	// Second login finds the same user and refreshes the profile.
	claims.DisplayName = "Alice Renamed"
	user2, err := svc.LoginOIDC(claims)
	if err != nil {
		t.Fatal(err)
	}
	if user2.ID != user.ID || user2.DisplayName != "Alice Renamed" {
		t.Fatalf("repeat OIDC login must return the same user with refreshed profile: %+v", user2)
	}
	// A session issued for the OIDC user resolves.
	tok := svc.CreateSession(user2)
	uid, uname, role, ok := svc.SessionUser(tok)
	if !ok || uid != user.ID || uname != "alice" || role != "employee" {
		t.Fatalf("session mismatch: %q %q %q ok=%v", uid, uname, role, ok)
	}
	// The OIDC user cannot log in with a local password (no hash).
	if _, _, err := svc.Login("alice", "anything"); err == nil {
		t.Fatal("OIDC user must not have a local password login")
	}
	_ = st
}

func TestLoginOIDCCannotTakeOverLocalAccount(t *testing.T) {
	svc, _, _ := newServiceWithUser(t, "alice", "demo12345")

	// An OIDC identity whose preferred username collides with a local
	// account must get a suffixed username, not the local account.
	user, err := svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-sub-9", PreferredUsername: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if user.Username == "alice" {
		t.Fatal("OIDC provisioning must not reuse a local username")
	}
	if user.ID == "" || user.OIDCSubject != "oidc-sub-9" {
		t.Fatalf("provisioned user mismatch: %+v", user)
	}
}

func TestLoginOIDCRejectsDisabledAndEmptySubject(t *testing.T) {
	svc, st, _ := newServiceWithUser(t, "admin", "demo12345")

	if _, err := svc.LoginOIDC(auth.OIDCClaims{}); err == nil {
		t.Fatal("empty subject must be rejected")
	}
	u, err := svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-sub-2", PreferredUsername: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	st.WithLock(func(s *store.State) { s.Users[u.ID].Disabled = true })
	if _, err := svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-sub-2", PreferredUsername: "bob"}); err == nil {
		t.Fatal("disabled OIDC account must be rejected")
	}
}

func TestOIDCUsernameSanitization(t *testing.T) {
	svc, _, _ := newServiceWithUser(t, "admin", "demo12345")

	// Email local part is the fallback; unsafe characters are stripped.
	user, err := svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-sub-3", Email: "Bob.Smith+tag@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "bob.smithtag" {
		t.Fatalf("username = %q, want bob.smithtag", user.Username)
	}
	// No usable name at all: subject-derived fallback.
	user, err = svc.LoginOIDC(auth.OIDCClaims{Subject: "oidc-sub-4"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(user.Username, "oidc-") {
		t.Fatalf("username = %q, want oidc- prefix", user.Username)
	}
}

// TestAttestationDowngradesManagedTokens (backlog F14): a managed-tier
// request is honoured only when attestation passes; a failed or
// missing-required attestation issues a BYOA token instead.
func TestAttestationDowngradesManagedTokens(t *testing.T) {
	svc, st, user := newServiceWithUser(t, "alice", "demo12345")
	st.WithLock(func(s *store.State) {
		s.AttestationPolicies["dsh"] = &attest.Policy{
			AgentType: "dsh", MinVersion: "1.2.0", RequireReport: true,
		}
	})
	good := attest.Report{Version: "1.2.0", GuardApplied: true}
	bad := attest.Report{Version: "1.1.9", GuardApplied: true}

	// Passing report -> managed, attested.
	dc := svc.StartDeviceCode("dsh", store.TierManaged)
	if v, err := svc.SubmitAttestation(dc.Code, dc.Secret, good); err != nil || v.Status != attest.VerdictPass {
		t.Fatalf("submit good report: verdict=%+v err=%v", v, err)
	}
	plain, issued, err := svc.ApproveDeviceCode(dc.Code, user, "good laptop")
	if err != nil || issued != store.TierManaged {
		t.Fatalf("approve good: issued=%v err=%v", issued, err)
	}
	tok, _, ok := svc.ResolveToken(plain)
	if !ok || !tok.Attested || tok.AttestVerdict != attest.VerdictPass {
		t.Fatalf("good token: attested=%v verdict=%q ok=%v", tok.Attested, tok.AttestVerdict, ok)
	}

	// Failing report -> BYOA, not attested, verdict downgraded.
	dc = svc.StartDeviceCode("dsh", store.TierManaged)
	if v, err := svc.SubmitAttestation(dc.Code, dc.Secret, bad); err != nil || v.Status != attest.VerdictDowngraded {
		t.Fatalf("submit bad report: verdict=%+v err=%v", v, err)
	}
	plain, issued, err = svc.ApproveDeviceCode(dc.Code, user, "old laptop")
	if err != nil || issued != store.TierBYOA {
		t.Fatalf("approve bad: issued=%v err=%v", issued, err)
	}
	tok, _, ok = svc.ResolveToken(plain)
	if !ok || tok.Attested || tok.AttestVerdict != attest.VerdictDowngraded {
		t.Fatalf("bad token: attested=%v verdict=%q ok=%v", tok.Attested, tok.AttestVerdict, ok)
	}

	// No report at all while one is required -> BYOA.
	dc = svc.StartDeviceCode("dsh", store.TierManaged)
	plain, issued, err = svc.ApproveDeviceCode(dc.Code, user, "silent laptop")
	if err != nil || issued != store.TierBYOA {
		t.Fatalf("approve without report: issued=%v err=%v", issued, err)
	}
	tok, _, ok = svc.ResolveToken(plain)
	if !ok || tok.AttestVerdict != attest.VerdictDowngraded {
		t.Fatalf("silent token: verdict=%q ok=%v", tok.AttestVerdict, ok)
	}

	// No policy for the agent type -> requested tier, unverified.
	dc = svc.StartDeviceCode("claude-code", store.TierManaged)
	plain, issued, err = svc.ApproveDeviceCode(dc.Code, user, "other laptop")
	if err != nil || issued != store.TierManaged {
		t.Fatalf("approve without policy: issued=%v err=%v", issued, err)
	}
	tok, _, ok = svc.ResolveToken(plain)
	if !ok || tok.Attested || tok.AttestVerdict != attest.VerdictUnverified {
		t.Fatalf("unverified token: attested=%v verdict=%q ok=%v", tok.Attested, tok.AttestVerdict, ok)
	}

	// Wrong polling secret cannot submit a report.
	dc = svc.StartDeviceCode("dsh", store.TierManaged)
	if _, err := svc.SubmitAttestation(dc.Code, "poll_wrongsecret", good); err == nil {
		t.Fatal("attestation with the wrong secret must fail")
	}
	// A report submitted after approval is too late.
	if _, err := svc.SubmitAttestation(dc.Code, dc.Secret, good); err != nil {
		t.Fatalf("submit before approval: %v", err)
	}
	if _, _, err := svc.ApproveDeviceCode(dc.Code, user, "late laptop"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := svc.SubmitAttestation(dc.Code, dc.Secret, good); err == nil {
		t.Fatal("attestation after approval must fail")
	}
}
