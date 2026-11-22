package attest

import "testing"

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int // sign only
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0},
		{"1.2", "1.2.0", 0},
		{"1.10.0", "1.9.9", 1},
		{"2.0.0", "10.0.0", -1},
		{"0.1.0", "0.0.9", 1},
		{"1.2.3-beta", "1.2.3", -1},
		{"1.2.3", "1.2.3-beta", 1},
		{"1.2.3-alpha", "1.2.3-beta", 0}, // same numbers; prerelease ordering beyond "is prerelease" is out of scope
		{"", "0.1.0", -1},                // empty version is 0.0.0: fail-closed against any minimum
		{"garbage", "0.0.1", -1},
		{"1.2.3+build.5", "1.2.3", 0},
	}
	for _, c := range cases {
		got := CompareVersion(c.a, c.b)
		sign := 0
		if got < 0 {
			sign = -1
		} else if got > 0 {
			sign = 1
		}
		if sign != c.want {
			t.Errorf("CompareVersion(%q, %q) = %d, want sign %d", c.a, c.b, got, c.want)
		}
	}
	if !ValidVersion("1.2.3") || !ValidVersion("v0.1.0") || !ValidVersion("1.0.0-rc.1") {
		t.Error("well-formed versions must validate")
	}
	for _, bad := range []string{"", "1..2", "1.x", "a.b.c", "-1.2.3"} {
		if ValidVersion(bad) {
			t.Errorf("version %q must not validate", bad)
		}
	}
}

func TestEvaluate(t *testing.T) {
	policy := Policy{
		AgentType:           "dsh",
		MinVersion:          "1.2.0",
		AllowedConfigHashes: []string{"aabbcc"},
		AllowedPlugins:      []string{"files", "calendar"},
		BlockedPlugins:      []string{"shell-exec"},
		RequireGuard:        true,
		RequireReport:       true,
	}
	good := &Report{AgentType: "dsh", Version: "1.2.0", ConfigHash: "AABBCC",
		Plugins: []string{"files"}, GuardApplied: true, OS: "darwin"}
	if v := Evaluate(good, policy); v.Status != VerdictPass {
		t.Fatalf("good report: got %+v, want pass", v)
	}
	// Version below minimum.
	bad := *good
	bad.Version = "1.1.9"
	if v := Evaluate(&bad, policy); v.Status != VerdictDowngraded || !hasReason(v, ReasonVersionBelowMinimum) {
		t.Errorf("old version: got %+v, want downgraded/version_below_minimum", v)
	}
	// Config hash not on the allow-list.
	bad = *good
	bad.ConfigHash = "deadbeef"
	if v := Evaluate(&bad, policy); v.Status != VerdictDowngraded || !hasReason(v, ReasonConfigHashNotAllow) {
		t.Errorf("unknown config hash: got %+v", v)
	}
	// No config hash at all while one is pinned.
	bad = *good
	bad.ConfigHash = ""
	if v := Evaluate(&bad, policy); v.Status != VerdictDowngraded || !hasReason(v, ReasonConfigHashMissing) {
		t.Errorf("missing config hash: got %+v", v)
	}
	// Blocked plugin.
	bad = *good
	bad.Plugins = []string{"files", "shell-exec"}
	if v := Evaluate(&bad, policy); v.Status != VerdictDowngraded || !hasReason(v, ReasonBlockedPlugin) {
		t.Errorf("blocked plugin: got %+v", v)
	}
	// Plugin outside the allow-list.
	bad = *good
	bad.Plugins = []string{"files", "random-tool"}
	if v := Evaluate(&bad, policy); v.Status != VerdictDowngraded || !hasReason(v, ReasonPluginNotAllowed) {
		t.Errorf("unlisted plugin: got %+v", v)
	}
	// Guard rules not applied.
	bad = *good
	bad.GuardApplied = false
	if v := Evaluate(&bad, policy); v.Status != VerdictDowngraded || !hasReason(v, ReasonGuardNotApplied) {
		t.Errorf("guard off: got %+v", v)
	}
	// Several failures at once: all reasons are reported.
	bad = *good
	bad.Version = "0.9.0"
	bad.GuardApplied = false
	if v := Evaluate(&bad, policy); v.Status != VerdictDowngraded || len(v.Reasons) < 2 {
		t.Errorf("multiple failures: got %+v, want >= 2 reasons", v)
	}
	// No report while one is required.
	if v := Evaluate(nil, policy); v.Status != VerdictDowngraded || !hasReason(v, ReasonReportMissing) {
		t.Errorf("missing report: got %+v", v)
	}
	// No report while none is required: unverified, not downgraded.
	loose := policy
	loose.RequireReport = false
	if v := Evaluate(nil, loose); v.Status != VerdictUnverified {
		t.Errorf("missing optional report: got %+v, want unverified", v)
	}
	// Empty allow-lists mean "any value accepted".
	open := Policy{AgentType: "other", MinVersion: "0.1.0", RequireReport: true}
	any := &Report{AgentType: "other", Version: "9.9.9", ConfigHash: "whatever",
		Plugins: []string{"anything"}, GuardApplied: false}
	if v := Evaluate(any, open); v.Status != VerdictPass {
		t.Errorf("open policy: got %+v, want pass", v)
	}
	// Reasons never carry the offending value.
	bad = *good
	bad.ConfigHash = "deadbeef"
	if v := Evaluate(&bad, policy); len(v.Reasons) == 1 && v.Reasons[0] == "deadbeef" {
		t.Error("reasons must be category codes, never the offending value")
	}
}

func TestReportAndPolicyValidate(t *testing.T) {
	if err := (Report{AgentType: "dsh", Version: "1.0.0"}).Validate(); err != nil {
		t.Errorf("minimal report must validate: %v", err)
	}
	if err := (Report{AgentType: "dsh", ConfigHash: "zz"}).Validate(); err == nil {
		t.Error("non-hex config hash must fail validation")
	}
	if err := (Report{AgentType: "dsh", Plugins: []string{""}}).Validate(); err == nil {
		t.Error("empty plugin name must fail validation")
	}
	if err := (Policy{AgentType: "dsh", MinVersion: "1.x"}).Validate(); err == nil {
		t.Error("invalid min_version must fail validation")
	}
	if err := (Policy{AgentType: "dsh", MinVersion: "1.2.0"}).Validate(); err != nil {
		t.Errorf("valid policy must validate: %v", err)
	}
}

func hasReason(v Verdict, want string) bool {
	for _, r := range v.Reasons {
		if r == want {
			return true
		}
	}
	return false
}
