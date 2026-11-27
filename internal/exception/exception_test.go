package exception_test

import (
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/exception"
	"github.com/myjnathan/agentgate/internal/store"
)

func TestValidateRequest(t *testing.T) {
	// Public/internal data never needs an exception: the gateway
	// does not block it on classification grounds.
	if err := exception.ValidateRequest(store.LevelInternal, "need", "mock-pro", 4, false); err == nil {
		t.Fatal("internal level must not need an exception")
	}
	if err := exception.ValidateRequest(store.LevelConfidential, "", "mock-pro", 4, false); err == nil {
		t.Fatal("empty reason must be rejected")
	}
	if err := exception.ValidateRequest(store.LevelConfidential, "need", "", 4, false); err == nil {
		t.Fatal("empty scope must be rejected")
	}
	if err := exception.ValidateRequest(store.LevelConfidential, "need", "mock-pro", 0, false); err == nil {
		t.Fatal("0 hours must be rejected")
	}
	if err := exception.ValidateRequest(store.LevelConfidential, "need", "mock-pro", 73, false); err == nil {
		t.Fatal("73 hours must be rejected")
	}
	// Top secret is closed by default, open with the opt-in.
	if err := exception.ValidateRequest(store.LevelSecret, "need", "mock-pro", 4, false); err == nil {
		t.Fatal("top-secret exception must be disabled by default")
	}
	if err := exception.ValidateRequest(store.LevelSecret, "need", "mock-pro", 4, true); err != nil {
		t.Fatalf("top-secret exception with opt-in: %v", err)
	}
	if err := exception.ValidateRequest(store.LevelConfidential, "need", "mock-pro", 72, false); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	if err := exception.ValidateRequest(store.LevelConfidential, strings.Repeat("x", 501), "mock-pro", 4, false); err == nil {
		t.Fatal("over-long reason must be rejected")
	}
}

func TestGrantRoundTrip(t *testing.T) {
	token, hash := exception.NewGrant()
	if !strings.HasPrefix(token, "exc_") || len(token) < 40 {
		t.Fatalf("grant token shape unexpected: %q", token)
	}
	if got := exception.HashGrant(token); got != hash {
		t.Fatal("hash of minted token must equal stored hash")
	}
	if exception.HashGrant(token+"x") == hash {
		t.Fatal("different token must hash differently")
	}
}

func TestFindValidGrant(t *testing.T) {
	now := time.Now()
	_, hash := exception.NewGrant()
	exp := now.Add(time.Hour)
	mk := func(status string, applicant string, level store.DataLevel, expires *time.Time) *store.Exception {
		return &store.Exception{ID: "exc_1", ApplicantID: applicant, DataLevel: level,
			Status: status, GrantHash: hash, ExpiresAt: expires}
	}
	excs := map[string]*store.Exception{"exc_1": mk(exception.StatusApproved, "u1", store.LevelConfidential, &exp)}
	if got := exception.FindValidGrant(excs, hash, "u1", store.LevelConfidential, now); got == nil {
		t.Fatal("valid grant must be found")
	}
	// Wrong owner: another employee cannot spend someone else's grant.
	if got := exception.FindValidGrant(excs, hash, "u2", store.LevelConfidential, now); got != nil {
		t.Fatal("grant must bind to its applicant")
	}
	// A confidential grant never waives a secret-level call.
	if got := exception.FindValidGrant(excs, hash, "u1", store.LevelSecret, now); got != nil {
		t.Fatal("grant must not cover a higher data level")
	}
	// Unknown hash, pending, used and expired grants are all inert.
	if got := exception.FindValidGrant(excs, "deadbeef", "u1", store.LevelConfidential, now); got != nil {
		t.Fatal("unknown hash must not match")
	}
	excs["exc_1"] = mk(exception.StatusPending, "u1", store.LevelConfidential, &exp)
	if got := exception.FindValidGrant(excs, hash, "u1", store.LevelConfidential, now); got != nil {
		t.Fatal("pending request has no grant yet")
	}
	excs["exc_1"] = mk(exception.StatusUsed, "u1", store.LevelConfidential, &exp)
	if got := exception.FindValidGrant(excs, hash, "u1", store.LevelConfidential, now); got != nil {
		t.Fatal("a used grant must not work twice")
	}
	past := now.Add(-time.Minute)
	excs["exc_1"] = mk(exception.StatusApproved, "u1", store.LevelConfidential, &past)
	if got := exception.FindValidGrant(excs, hash, "u1", store.LevelConfidential, now); got != nil {
		t.Fatal("expired grant must not work")
	}
}

func TestRefreshStatus(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute)
	e := &store.Exception{Status: exception.StatusApproved, ExpiresAt: &past}
	if !exception.RefreshStatus(e, now) || e.Status != exception.StatusExpired {
		t.Fatal("approved grant past expiry must flip to expired")
	}
	if exception.RefreshStatus(e, now) {
		t.Fatal("second refresh must be a no-op")
	}
	future := now.Add(time.Hour)
	e2 := &store.Exception{Status: exception.StatusApproved, ExpiresAt: &future}
	if exception.RefreshStatus(e2, now) || e2.Status != exception.StatusApproved {
		t.Fatal("unexpired grant must stay approved")
	}
	e3 := &store.Exception{Status: exception.StatusPending}
	if exception.RefreshStatus(e3, now) {
		t.Fatal("pending request must never flip to expired")
	}
}

func TestValidatePluginRequest(t *testing.T) {
	hash := strings.Repeat("a", 64)
	valid := func() error {
		return exception.ValidatePluginRequest("good", "1.2.3", hash, "platform", "need it", "dsh plugin", 4)
	}
	if err := valid(); err != nil {
		t.Errorf("valid plugin request: %v", err)
	}
	// Triple-only variants are fine: ID-only, no hash/source.
	if err := exception.ValidatePluginRequest("good", "", "", "", "need it", "dsh plugin", 4); err != nil {
		t.Errorf("ID-only plugin request: %v", err)
	}
	if err := exception.ValidatePluginRequest("", "1.2.3", hash, "platform", "need it", "dsh plugin", 4); err == nil {
		t.Error("empty plugin id must be rejected")
	}
	if err := exception.ValidatePluginRequest(strings.Repeat("x", 129), "", "", "", "need it", "dsh plugin", 4); err == nil {
		t.Error("overlong plugin id must be rejected")
	}
	if err := exception.ValidatePluginRequest("good", "not-a-version", hash, "platform", "need it", "dsh plugin", 4); err == nil {
		t.Error("invalid plugin version must be rejected")
	}
	if err := exception.ValidatePluginRequest("good", "1.2.3", "abc", "platform", "need it", "dsh plugin", 4); err == nil {
		t.Error("short plugin hash must be rejected")
	}
	if err := exception.ValidatePluginRequest("good", "1.2.3", strings.Repeat("z", 64), "platform", "need it", "dsh plugin", 4); err == nil {
		t.Error("non-hex plugin hash must be rejected")
	}
	if err := exception.ValidatePluginRequest("good", "1.2.3", hash, "side-load", "need it", "dsh plugin", 4); err == nil {
		t.Error("unknown plugin source must be rejected")
	}
	if err := exception.ValidatePluginRequest("good", "1.2.3", hash, "platform", "", "dsh plugin", 4); err == nil {
		t.Error("empty reason must be rejected")
	}
	if err := exception.ValidatePluginRequest("good", "1.2.3", hash, "platform", "need it", "", 4); err == nil {
		t.Error("empty scope must be rejected")
	}
	if err := exception.ValidatePluginRequest("good", "1.2.3", hash, "platform", "need it", "dsh plugin", 0); err == nil {
		t.Error("0 hours must be rejected")
	}
	if err := exception.ValidatePluginRequest("good", "1.2.3", hash, "platform", "need it", "dsh plugin", 73); err == nil {
		t.Error("73 hours must be rejected")
	}
}

func TestExceptedPlugins(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	mk := func(kind, status, applicant, id string, exp *time.Time) *store.Exception {
		return &store.Exception{ID: "exc_" + applicant + "_" + id + "_" + status, Kind: kind,
			Status: status, ApplicantID: applicant, PluginID: id, ExpiresAt: exp}
	}
	exceptions := map[string]*store.Exception{
		"ok":       mk(exception.KindPlugin, exception.StatusApproved, "u-alice", "bad", &future),
		"expired":  mk(exception.KindPlugin, exception.StatusApproved, "u-alice", "old", &past),
		"pending":  mk(exception.KindPlugin, exception.StatusPending, "u-alice", "new", &future),
		"revoked":  mk(exception.KindPlugin, exception.StatusRevoked, "u-alice", "gone", &future),
		"call":     mk(exception.KindCall, exception.StatusApproved, "u-alice", "bad", &future),
		"other":    mk(exception.KindPlugin, exception.StatusApproved, "u-bob", "bad", &future),
		"noexpiry": {ID: "noexpiry", Kind: exception.KindPlugin, Status: exception.StatusApproved, ApplicantID: "u-alice", PluginID: "open"},
	}
	got := exception.ExceptedPlugins(exceptions, "u-alice", now)
	if len(got) != 2 {
		t.Fatalf("want 2 waivers (bad + open), got %+v", got)
	}
	ids := map[string]bool{}
	for _, x := range got {
		ids[x.ID] = true
	}
	if !ids["bad"] || !ids["open"] {
		t.Errorf("waivers %+v, want bad and open", got)
	}
	// Grouped view: bob's waiver is separate, alice's is intact.
	byUser := exception.ExceptedPluginsByUser(exceptions, now)
	if len(byUser["u-bob"]) != 1 || byUser["u-bob"][0].ID != "bad" {
		t.Errorf("bob waivers %+v, want [bad]", byUser["u-bob"])
	}
	if len(byUser["u-alice"]) != 2 {
		t.Errorf("alice waivers %+v, want 2", byUser["u-alice"])
	}
	// Expiry is honoured even before the lazy status flip: an
	// approved record past its expiry never waives anything.
	if got := exception.ExceptedPlugins(exceptions, "u-alice", now.Add(2*time.Hour)); len(got) != 1 || got[0].ID != "open" {
		t.Errorf("after expiry: %+v, want only the unexpiring waiver", got)
	}
}
