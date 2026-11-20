package auth_test

import (
	"strings"
	"testing"

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

	if _, err := svc.ApproveDeviceCode(dc.Code, user, "test laptop"); err != nil {
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
	if _, err := svc.ApproveDeviceCode("AG-NOPE", user, "x"); err == nil {
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
