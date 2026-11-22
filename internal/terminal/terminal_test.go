package terminal_test

import (
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/attest"
	"github.com/myjnathan/agentgate/internal/terminal"
)

func TestPolicySnapshotVersion(t *testing.T) {
	a := terminal.NewPolicySnapshot([]string{"b", "a"}, []string{"x"}, false, "internal")
	b := terminal.NewPolicySnapshot([]string{"a", "b"}, []string{"x"}, false, "internal")
	if a.Version != b.Version {
		t.Fatalf("word order must not change the version: %s vs %s", a.Version, b.Version)
	}
	if !a.Verify() {
		t.Fatalf("fresh snapshot must verify")
	}
	c := terminal.NewPolicySnapshot([]string{"a", "b", "c"}, []string{"x"}, false, "internal")
	if c.Version == a.Version {
		t.Fatalf("different content must change the version")
	}
	d := terminal.NewPolicySnapshot([]string{"a", "b"}, []string{"x"}, true, "internal")
	if d.Version == a.Version {
		t.Fatalf("dry-run must change the version")
	}
	// Tampered content fails verification.
	tampered := a
	tampered.BannedWords = []string{"a", "b", "evil"}
	if tampered.Verify() {
		t.Fatalf("tampered snapshot must not verify")
	}
	// Unknown offline level falls back to internal.
	e := terminal.NewPolicySnapshot(nil, nil, false, "bogus")
	if e.OfflineMaxLevel != terminal.DefaultOfflineMaxLevel {
		t.Fatalf("want offline max %q, got %q", terminal.DefaultOfflineMaxLevel, e.OfflineMaxLevel)
	}
	if e.OfflineMaxLevelOrDefault() != terminal.DefaultOfflineMaxLevel {
		t.Fatalf("want default offline max, got %q", e.OfflineMaxLevelOrDefault())
	}
}

func TestPolicySnapshotAllowsOffline(t *testing.T) {
	s := terminal.NewPolicySnapshot(nil, nil, false, "internal")
	for _, tc := range []struct {
		level string
		want  bool
	}{
		{"public", true},
		{"internal", true},
		{"confidential", false},
		{"secret", false},
		{"bogus", false},
	} {
		if got := s.AllowsOffline(tc.level); got != tc.want {
			t.Errorf("AllowsOffline(%q) = %v, want %v", tc.level, got, tc.want)
		}
	}
}

func TestHeartbeatValidation(t *testing.T) {
	valid := terminal.Heartbeat{AgentType: "dsh", Version: "0.1.0", ConfigHash: strings.Repeat("ab", 32),
		Plugins: []string{"p1"}, GuardApplied: true, OS: "linux"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid heartbeat rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*terminal.Heartbeat)
	}{
		{"empty version", func(h *terminal.Heartbeat) { h.Version = "" }},
		{"bad version", func(h *terminal.Heartbeat) { h.Version = "soon" }},
		{"bad config hash", func(h *terminal.Heartbeat) { h.ConfigHash = "zz" }},
		{"bad binary hash", func(h *terminal.Heartbeat) { h.BinaryHash = "not hex!" }},
		{"too many plugins", func(h *terminal.Heartbeat) {
			h.Plugins = make([]string, 101)
			for i := range h.Plugins {
				h.Plugins[i] = "p"
			}
		}},
		{"offline negative", func(h *terminal.Heartbeat) {
			h.Offline = &terminal.OfflineSummary{Events: -1}
		}},
		{"offline parts exceed events", func(h *terminal.Heartbeat) {
			h.Offline = &terminal.OfflineSummary{Events: 1, Blocked: 1, Unreachable: 1}
		}},
		{"offline window reversed", func(h *terminal.Heartbeat) {
			now := time.Now()
			h.Offline = &terminal.OfflineSummary{Events: 1, WindowStart: now, WindowEnd: now.Add(-time.Minute)}
		}},
	} {
		h := valid
		tc.mut(&h)
		if err := h.Validate(); err == nil {
			t.Errorf("%s: want validation error, got nil", tc.name)
		}
	}
}

func TestDriftReasons(t *testing.T) {
	rep := &attest.Report{AgentType: "dsh", Version: "0.0.9", GuardApplied: true}
	if got := terminal.DriftReasons(rep, nil); len(got) != 0 {
		t.Fatalf("no policy must mean no drift, got %v", got)
	}
	pol := &attest.Policy{AgentType: "dsh", MinVersion: "0.1.0", RequireReport: true}
	if got := terminal.DriftReasons(rep, pol); len(got) != 1 || got[0] != attest.ReasonVersionBelowMinimum {
		t.Fatalf("want [version_below_minimum], got %v", got)
	}
	rep.Version = "0.1.0"
	if got := terminal.DriftReasons(rep, pol); len(got) != 0 {
		t.Fatalf("passing report must have no drift, got %v", got)
	}
	// A blocked plugin is drift too, judged by the same appraisal.
	rep.Plugins = []string{"evil-plugin"}
	pol.BlockedPlugins = []string{"evil-plugin"}
	if got := terminal.DriftReasons(rep, pol); len(got) != 1 || got[0] != attest.ReasonBlockedPlugin {
		t.Fatalf("want [blocked_plugin], got %v", got)
	}
}

func TestTerminalStatus(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{0, terminal.StatusOnline},
		{4 * time.Minute, terminal.StatusOnline},
		{5*time.Minute + time.Second, terminal.StatusStale},
		{59 * time.Minute, terminal.StatusStale},
		{61 * time.Minute, terminal.StatusOffline},
		{25 * time.Hour, terminal.StatusOffline},
	} {
		rec := &terminal.Terminal{LastSeen: now.Add(-tc.age)}
		if got := rec.Status(now); got != tc.want {
			t.Errorf("age %v: want %s, got %s", tc.age, tc.want, got)
		}
	}
	rec := &terminal.Terminal{DriftReasons: []string{"version_below_minimum"}}
	if !rec.IsDrifted() {
		t.Fatalf("want drifted")
	}
	rec.DriftReasons = nil
	if rec.IsDrifted() {
		t.Fatalf("want not drifted")
	}
}
