package pluginpol_test

import (
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/attest"
	"github.com/myjnathan/agentgate/internal/pluginpol"
)

func hashA() string { return strings.Repeat("a", 64) }
func hashB() string { return strings.Repeat("b", 64) }

func ev(id, version, hash, source string) attest.PluginEvidence {
	return attest.PluginEvidence{ID: id, Version: version, Hash: hash, Source: source}
}

func allowPolicy(id string, rules ...pluginpol.PluginRule) *pluginpol.PluginPolicy {
	return &pluginpol.PluginPolicy{
		ID: id, Name: id, Mode: pluginpol.ModeAllowlist,
		Scope: pluginpol.Scope{Level: pluginpol.ScopeGlobal},
		Rules: rules, Action: pluginpol.ActionDowngrade, Enabled: true,
	}
}

func denyPolicy(id string, rules ...pluginpol.PluginRule) *pluginpol.PluginPolicy {
	return &pluginpol.PluginPolicy{
		ID: id, Name: id, Mode: pluginpol.ModeDenylist,
		Scope: pluginpol.Scope{Level: pluginpol.ScopeGlobal},
		Rules: rules, Action: pluginpol.ActionDowngrade, WarnUnknown: true, Enabled: true,
	}
}

func polMap(ps ...*pluginpol.PluginPolicy) map[string]*pluginpol.PluginPolicy {
	m := map[string]*pluginpol.PluginPolicy{}
	for _, p := range ps {
		m[p.ID] = p
	}
	return m
}

func TestMatchesConstraint(t *testing.T) {
	// matchesConstraint is unexported; exercise it through
	// matchRule via Evaluate on a single-rule allowlist.
	allow := func(constraint string) *pluginpol.PluginPolicy {
		return allowPolicy("p", pluginpol.PluginRule{ID: "foo", VersionConstraint: constraint})
	}
	cases := []struct {
		constraint string
		version    string
		wantOK     bool
	}{
		{"", "1.0.0", true},       // any version
		{"1.2.3", "1.2.3", true},  // exact
		{"1.2.3", "1.2.4", false}, // exact mismatch
		{">=1.2.0", "1.2.0", true},
		{">=1.2.0", "1.1.9", false},
		{"<=2.0.0", "2.0.0", true},
		{"<=2.0.0", "2.0.1", false},
		{">1.0.0", "1.0.0", false},
		{">1.0.0", "1.0.1", true},
		{"<1.0.0", "0.9.9", true},
		{"<1.0.0", "1.0.0", false},
		{"!=1.0.0", "1.0.1", true},
		{"!=1.0.0", "1.0.0", false},
		{"v1.2.3", "1.2.3", true}, // leading v tolerated
	}
	for _, c := range cases {
		reasons, _, _ := pluginpol.EvaluateReport(
			&attest.Report{PluginDetails: []attest.PluginEvidence{ev("foo", c.version, "", "")}},
			polMap(allow(c.constraint)), "", nil, time.Now())
		if c.wantOK && len(reasons) != 0 {
			t.Errorf("constraint %q version %q: got reasons %v, want none", c.constraint, c.version, reasons)
		}
		if !c.wantOK && len(reasons) == 0 {
			t.Errorf("constraint %q version %q: got no reasons, want a failure", c.constraint, c.version)
		}
	}
}

func TestEvaluateAllowlistReasons(t *testing.T) {
	now := time.Now()
	p := allowPolicy("p",
		pluginpol.PluginRule{ID: "good", VersionConstraint: ">=1.0.0"},
		pluginpol.PluginRule{ID: "pinned", VersionConstraint: "2.0.0", Hash: hashA()},
		pluginpol.PluginRule{ID: "fromstore", Source: pluginpol.SourcePlatform},
	)
	run := func(details ...attest.PluginEvidence) []string {
		reasons, _, _ := pluginpol.EvaluateReport(
			&attest.Report{PluginDetails: details}, polMap(p), "", nil, now)
		return reasons
	}
	if r := run(ev("good", "1.5.0", "", "")); len(r) != 0 {
		t.Errorf("allowed plugin: reasons %v", r)
	}
	if r := run(ev("good", "0.9.0", "", "")); !hasReason(r, attest.ReasonPluginVersionMismatch) {
		t.Errorf("version mismatch: reasons %v, want plugin_version_mismatch", r)
	}
	if r := run(ev("pinned", "2.0.0", hashB(), "")); !hasReason(r, attest.ReasonPluginHashMismatch) {
		t.Errorf("hash mismatch: reasons %v, want plugin_hash_mismatch", r)
	}
	if r := run(ev("pinned", "2.0.0", hashA(), "")); len(r) != 0 {
		t.Errorf("exact triple: reasons %v", r)
	}
	if r := run(ev("fromstore", "", "", pluginpol.SourceMarketplace)); !hasReason(r, attest.ReasonPluginUnknownSource) {
		t.Errorf("source mismatch: reasons %v, want plugin_unknown_source", r)
	}
	if r := run(ev("stranger", "1.0.0", "", "")); !hasReason(r, attest.ReasonPluginNotAllowed) {
		t.Errorf("not on list: reasons %v, want plugin_not_allowed", r)
	}
	// Two violations of different kinds: both codes, each once.
	if r := run(ev("good", "0.9.0", "", ""), ev("stranger", "1.0.0", "", "")); len(r) != 2 ||
		!hasReason(r, attest.ReasonPluginVersionMismatch) || !hasReason(r, attest.ReasonPluginNotAllowed) {
		t.Errorf("two violations: reasons %v, want version + not_allowed", r)
	}
}

func TestEvaluateAllowlistNameOnlyCompat(t *testing.T) {
	// Old agents report names only: a version-constrained rule
	// must not fail them (no evidence), and the compat path still
	// enforces the name list.
	now := time.Now()
	p := allowPolicy("p",
		pluginpol.PluginRule{ID: "good", VersionConstraint: ">=9.9.9", Hash: hashA()},
	)
	// Name-only report: allowed (unverified), no version/hash reason.
	reasons, _, _ := pluginpol.EvaluateReport(
		&attest.Report{Plugins: []string{"good"}}, polMap(p), "", nil, now)
	if len(reasons) != 0 {
		t.Errorf("name-only good: reasons %v, want none", reasons)
	}
	// Name-only stranger still fails the name list.
	reasons, _, _ = pluginpol.EvaluateReport(
		&attest.Report{Plugins: []string{"stranger"}}, polMap(p), "", nil, now)
	if !hasReason(reasons, attest.ReasonPluginNotAllowed) {
		t.Errorf("name-only stranger: reasons %v, want plugin_not_allowed", reasons)
	}
	// Details, when present, win over the name list.
	reasons, _, _ = pluginpol.EvaluateReport(
		&attest.Report{Plugins: []string{"good"}, PluginDetails: []attest.PluginEvidence{ev("good", "1.0.0", hashB(), "")}},
		polMap(p), "", nil, now)
	if !hasReason(reasons, attest.ReasonPluginVersionMismatch) {
		t.Errorf("details win: reasons %v, want plugin_version_mismatch", reasons)
	}
}

func TestEvaluateDenylist(t *testing.T) {
	now := time.Now()
	p := denyPolicy("p",
		pluginpol.PluginRule{ID: "bad"},
		pluginpol.PluginRule{ID: "old", VersionConstraint: "<2.0.0"},
	)
	run := func(details ...attest.PluginEvidence) ([]string, []string) {
		reasons, unknown, _ := pluginpol.EvaluateReport(
			&attest.Report{PluginDetails: details}, polMap(p), "", nil, now)
		return reasons, unknown
	}
	// Name-only blocked plugin: blocked (pure name rule).
	if r, _ := run(ev("bad", "", "", "")); !hasReason(r, attest.ReasonBlockedPlugin) {
		t.Errorf("blocked name: reasons %v, want blocked_plugin", r)
	}
	// Version-scoped deny only blocks the bad version.
	if r, _ := run(ev("old", "2.0.0", "", "")); len(r) != 0 {
		t.Errorf("good version of old: reasons %v, want none", r)
	}
	if r, _ := run(ev("old", "1.9.9", "", "")); !hasReason(r, attest.ReasonBlockedPlugin) {
		t.Errorf("bad version of old: reasons %v, want blocked_plugin", r)
	}
	// Version-scoped deny + name-only report: cannot prove the
	// version, so not blocked (but it is covered, not unknown).
	if r, u := run(ev("old", "", "", "")); len(r) != 0 || len(u) != 0 {
		t.Errorf("name-only old: reasons %v unknown %v, want none", r, u)
	}
	// Unknown plugin: allowed by default, flagged for the alert.
	if r, u := run(ev("newthing", "1.0.0", "", "")); len(r) != 0 || len(u) != 1 || u[0] != "newthing" {
		t.Errorf("unknown: reasons %v unknown %v, want unknown=[newthing]", r, u)
	}
	// A clean plugin list: nothing.
	if r, u := run(); len(r) != 0 || len(u) != 0 {
		t.Errorf("empty report: reasons %v unknown %v", r, u)
	}
}

func TestResolveOnlyTighten(t *testing.T) {
	now := time.Now()
	globalAllow := allowPolicy("g",
		pluginpol.PluginRule{ID: "a"},
		pluginpol.PluginRule{ID: "b"},
	)
	deptAllow := &pluginpol.PluginPolicy{
		ID: "d", Name: "d", Mode: pluginpol.ModeAllowlist,
		Scope: pluginpol.Scope{Level: pluginpol.ScopeDepartment, Department: "研发部"},
		Rules: []pluginpol.PluginRule{{ID: "a"}}, Action: pluginpol.ActionDowngrade, Enabled: true,
	}
	deptDeny := &pluginpol.PluginPolicy{
		ID: "dd", Name: "dd", Mode: pluginpol.ModeDenylist,
		Scope: pluginpol.Scope{Level: pluginpol.ScopeDepartment, Department: "研发部"},
		Rules: []pluginpol.PluginRule{{ID: "a"}}, Action: pluginpol.ActionDowngrade, WarnUnknown: true, Enabled: true,
	}
	otherDept := &pluginpol.PluginPolicy{
		ID: "o", Name: "o", Mode: pluginpol.ModeAllowlist,
		Scope: pluginpol.Scope{Level: pluginpol.ScopeDepartment, Department: "市场部"},
		Rules: []pluginpol.PluginRule{{ID: "c"}}, Action: pluginpol.ActionDowngrade, Enabled: true,
	}
	// Department allowlist narrows the global one: b is gone for
	// 研发部 but stays for everyone else.
	reasons, _, _ := pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("b", "", "", "")}},
		polMap(globalAllow, deptAllow), "研发部", nil, now)
	if !hasReason(reasons, attest.ReasonPluginNotAllowed) {
		t.Errorf("dept narrows global: reasons %v, want plugin_not_allowed for b", reasons)
	}
	reasons, _, _ = pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("b", "", "", "")}},
		polMap(globalAllow, deptAllow), "市场部", nil, now)
	if len(reasons) != 0 {
		t.Errorf("other dept keeps b: reasons %v", reasons)
	}
	// A department denylist can remove a globally allowed plugin:
	// deny always wins, lower scope cannot re-allow.
	reasons, _, _ = pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("a", "", "", "")}},
		polMap(globalAllow, deptDeny), "研发部", nil, now)
	if !hasReason(reasons, attest.ReasonBlockedPlugin) {
		t.Errorf("dept deny wins: reasons %v, want blocked_plugin for a", reasons)
	}
	// A department policy does not apply to another department.
	reasons, _, _ = pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("a", "", "", "")}},
		polMap(globalAllow, deptDeny), "市场部", nil, now)
	if len(reasons) != 0 {
		t.Errorf("dept deny scoped: reasons %v, want none for 市场部", reasons)
	}
	// A policy for an unrelated department never applies at all.
	reasons, _, _ = pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("c", "", "", "")}},
		polMap(globalAllow, otherDept), "研发部", nil, now)
	if !hasReason(reasons, attest.ReasonPluginNotAllowed) {
		t.Errorf("unrelated dept policy: reasons %v, want plugin_not_allowed for c", reasons)
	}
}

func TestResolveDisabledAndExpired(t *testing.T) {
	now := time.Now()
	disabled := allowPolicy("off", pluginpol.PluginRule{ID: "a"})
	disabled.Enabled = false
	reasons, _, _ := pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("z", "", "", "")}},
		polMap(disabled), "", nil, now)
	if len(reasons) != 0 {
		t.Errorf("disabled policy: reasons %v, want none", reasons)
	}
	expired := allowPolicy("exp",
		pluginpol.PluginRule{ID: "a", ValidUntil: now.Add(-time.Hour)})
	reasons, _, _ = pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("a", "", "", "")}},
		polMap(expired), "", nil, now)
	if !hasReason(reasons, attest.ReasonPluginNotAllowed) {
		t.Errorf("expired rule: reasons %v, want plugin_not_allowed", reasons)
	}
	notYet := allowPolicy("soon",
		pluginpol.PluginRule{ID: "a", ValidFrom: now.Add(time.Hour)})
	reasons, _, _ = pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("a", "", "", "")}},
		polMap(notYet), "", nil, now)
	if !hasReason(reasons, attest.ReasonPluginNotAllowed) {
		t.Errorf("not-yet-valid rule: reasons %v, want plugin_not_allowed", reasons)
	}
}

func TestResolveActionStrictest(t *testing.T) {
	now := time.Now()
	observe := allowPolicy("o", pluginpol.PluginRule{ID: "a"})
	observe.Action = pluginpol.ActionObserve
	_, _, action := pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("a", "", "", "")}},
		polMap(observe), "", nil, now)
	if action != pluginpol.ActionObserve {
		t.Errorf("single observe policy: action %q", action)
	}
	warn := denyPolicy("w", pluginpol.PluginRule{ID: "b"})
	warn.Action = pluginpol.ActionWarn
	_, _, action = pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("a", "", "", "")}},
		polMap(observe, warn), "", nil, now)
	if action != pluginpol.ActionWarn {
		t.Errorf("observe+warn: action %q, want warn", action)
	}
	// No applicable policy: no action.
	_, _, action = pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("a", "", "", "")}},
		polMap(), "", nil, now)
	if action != "" {
		t.Errorf("no policy: action %q, want empty", action)
	}
}

func TestEvaluateReportNil(t *testing.T) {
	reasons, unknown, action := pluginpol.EvaluateReport(nil, polMap(), "", nil, time.Now())
	if len(reasons) != 0 || len(unknown) != 0 || action != "" {
		t.Errorf("nil report: %v %v %q", reasons, unknown, action)
	}
	// Nil policies map: no appraisal.
	reasons, _, _ = pluginpol.EvaluateReport(
		&attest.Report{PluginDetails: []attest.PluginEvidence{ev("a", "", "", "")}},
		nil, "", nil, time.Now())
	if len(reasons) != 0 {
		t.Errorf("nil policies: reasons %v", reasons)
	}
}

func TestValidate(t *testing.T) {
	base := func() pluginpol.PluginPolicy {
		return pluginpol.PluginPolicy{
			ID: "p", Name: "p", Mode: pluginpol.ModeAllowlist,
			Scope: pluginpol.Scope{Level: pluginpol.ScopeGlobal},
			Rules: []pluginpol.PluginRule{{ID: "a"}}, Action: pluginpol.ActionDowngrade, Enabled: true,
		}
	}
	bad := func(mut func(*pluginpol.PluginPolicy)) {
		p := base()
		mut(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("policy %+v: want validation error", p)
		}
	}
	bad(func(p *pluginpol.PluginPolicy) { p.Mode = "whatever" })
	bad(func(p *pluginpol.PluginPolicy) { p.Action = "whatever" })
	bad(func(p *pluginpol.PluginPolicy) { p.Scope = pluginpol.Scope{Level: pluginpol.ScopeDepartment} })
	bad(func(p *pluginpol.PluginPolicy) { p.Scope = pluginpol.Scope{Level: pluginpol.ScopeGroup} })
	bad(func(p *pluginpol.PluginPolicy) {
		p.Scope = pluginpol.Scope{Level: pluginpol.ScopeGlobal, Department: "x"}
	})
	bad(func(p *pluginpol.PluginPolicy) { p.Rules = []pluginpol.PluginRule{{ID: ""}} })
	bad(func(p *pluginpol.PluginPolicy) {
		p.Rules = []pluginpol.PluginRule{{ID: "a", VersionConstraint: ">=banana"}}
	})
	bad(func(p *pluginpol.PluginPolicy) {
		p.Rules = []pluginpol.PluginRule{{ID: "a", VersionConstraint: "=>1.0.0"}}
	})
	bad(func(p *pluginpol.PluginPolicy) { p.Rules = []pluginpol.PluginRule{{ID: "a", Hash: "xyz"}} })
	bad(func(p *pluginpol.PluginPolicy) {
		p.Rules = []pluginpol.PluginRule{{ID: "a", Hash: strings.Repeat("a", 63)}}
	})
	bad(func(p *pluginpol.PluginPolicy) { p.Rules = []pluginpol.PluginRule{{ID: "a", Source: "torrent"}} })
	bad(func(p *pluginpol.PluginPolicy) {
		p.Rules = []pluginpol.PluginRule{{ID: "a", ValidFrom: time.Now(), ValidUntil: time.Now().Add(-time.Hour)}}
	})
	// Denylist without warn_unknown is rejected.
	bad(func(p *pluginpol.PluginPolicy) { p.Mode = pluginpol.ModeDenylist; p.WarnUnknown = false })
	// Denylist with warn_unknown passes.
	p := base()
	p.Mode = pluginpol.ModeDenylist
	p.WarnUnknown = true
	if err := p.Validate(); err != nil {
		t.Errorf("denylist with warn_unknown: %v", err)
	}
	// The base allowlist passes.
	if err := base().Validate(); err != nil {
		t.Errorf("base policy: %v", err)
	}
}

func hasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}
