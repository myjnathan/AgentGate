package attest

import (
	"strings"
	"testing"
)

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

// F20: a registered distribution baseline pins the binary hash.
// A report carrying the distributed binary passes; anything else
// (missing hash, or a hash the platform never distributed) is
// downgraded, and an empty list keeps the old "any binary" behaviour.
func TestEvaluateBinaryHashBaseline(t *testing.T) {
	const baseline = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	policy := Policy{
		AgentType:           "dsh",
		AllowedBinaryHashes: []string{baseline},
		RequireReport:       true,
	}
	good := &Report{AgentType: "dsh", Version: "1.0.0", BinaryHash: baseline}
	if v := Evaluate(good, policy); v.Status != VerdictPass {
		t.Fatalf("distributed binary: got %+v, want pass", v)
	}
	// Case-insensitive match (hex digests compare folded).
	upper := *good
	upper.BinaryHash = "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"
	if v := Evaluate(&upper, policy); v.Status != VerdictPass {
		t.Errorf("uppercase binary hash: got %+v, want pass", v)
	}
	bad := *good
	bad.BinaryHash = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	if v := Evaluate(&bad, policy); v.Status != VerdictDowngraded || !hasReason(v, ReasonBinaryHashNotAllow) {
		t.Errorf("unknown binary: got %+v, want downgraded/binary_hash_not_allowed", v)
	}
	bad = *good
	bad.BinaryHash = ""
	if v := Evaluate(&bad, policy); v.Status != VerdictDowngraded || !hasReason(v, ReasonBinaryHashMissing) {
		t.Errorf("missing binary hash: got %+v, want downgraded/binary_hash_missing", v)
	}
	// No baseline registered: any binary passes (old behaviour).
	open := Policy{AgentType: "other", RequireReport: true}
	if v := Evaluate(good, open); v.Status != VerdictPass {
		t.Errorf("no baseline: got %+v, want pass", v)
	}
	if err := (Policy{AgentType: "dsh", AllowedBinaryHashes: []string{"not-hex"}}).Validate(); err == nil {
		t.Error("non-hex binary hash must fail policy validation")
	}
	if err := (Policy{AgentType: "dsh", AllowedBinaryHashes: []string{baseline}}).Validate(); err != nil {
		t.Errorf("hex binary hash must validate: %v", err)
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

// TestReportPluginDetailsValidate covers the F38 triple evidence:
// a present field must be well-formed, a missing one is allowed
// (old agents report names only).
func TestReportPluginDetailsValidate(t *testing.T) {
	ok := Report{AgentType: "dsh", Version: "1.0.0",
		PluginDetails: []PluginEvidence{
			{ID: "git-helper", Version: "1.2.3", Hash: "aabbcc", Source: "platform"},
			{ID: "name-only"},
		}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid details must validate: %v", err)
	}
	bad := []Report{
		{AgentType: "dsh", PluginDetails: []PluginEvidence{{ID: ""}}},
		{AgentType: "dsh", PluginDetails: []PluginEvidence{{ID: "x", Version: "soon"}}},
		{AgentType: "dsh", PluginDetails: []PluginEvidence{{ID: "x", Hash: "zz"}}},
		{AgentType: "dsh", PluginDetails: []PluginEvidence{{ID: "x", Source: strings.Repeat("s", 65)}}},
	}
	for i, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("details case %d must fail validation", i)
		}
	}
	many := Report{AgentType: "dsh"}
	for i := 0; i < 101; i++ {
		many.PluginDetails = append(many.PluginDetails, PluginEvidence{ID: "x"})
	}
	if err := many.Validate(); err == nil {
		t.Error("101 plugin details must fail validation")
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
