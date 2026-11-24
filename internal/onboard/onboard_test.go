package onboard_test

import (
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/onboard"
)

func TestPhases(t *testing.T) {
	for _, p := range []string{onboard.PhaseObserve, onboard.PhaseWarn, onboard.PhaseBlock} {
		if !onboard.ValidPhase(p) {
			t.Fatalf("%q should be a valid phase", p)
		}
		if got, ok := onboard.ParsePhase(p); !ok || got != p {
			t.Fatalf("ParsePhase(%q) = %q, %v", p, got, ok)
		}
	}
	for _, bad := range []string{"", "blockk", "OBSERVE", "dry-run"} {
		if onboard.ValidPhase(bad) {
			t.Fatalf("%q should not be a valid phase", bad)
		}
		if _, ok := onboard.ParsePhase(bad); ok {
			t.Fatalf("ParsePhase(%q) should fail", bad)
		}
	}
	if onboard.DefaultPhase != onboard.PhaseObserve {
		t.Fatalf("fresh installs must default to observe, got %q", onboard.DefaultPhase)
	}
}

func TestTemplates(t *testing.T) {
	ts := onboard.Templates()
	if len(ts) != 2 {
		t.Fatalf("want exactly 2 built-in templates, got %d", len(ts))
	}
	if ts[0].ID != "general-conservative" {
		t.Fatalf("first template must be the general conservative one, got %q", ts[0].ID)
	}
	if len(ts[0].CustomWords) != 0 {
		t.Fatalf("general template must add no custom words, got %v", ts[0].CustomWords)
	}
	med, ok := onboard.TemplateByID("medical-device")
	if !ok {
		t.Fatal("medical-device template missing")
	}
	for _, want := range []string{"注册证号", "临床数据", "设计输出", "XH-200"} {
		found := false
		for _, w := range med.CustomWords {
			if w == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("medical template missing custom word %q", want)
		}
	}
	if med.ReportCadence != "weekly" {
		t.Fatalf("medical template report cadence = %q, want weekly", med.ReportCadence)
	}
	if _, ok := onboard.TemplateByID("no-such-template"); ok {
		t.Fatal("unknown template ID should not resolve")
	}
}

func TestConservativePreset(t *testing.T) {
	p := onboard.Conservative
	if p.Phase != onboard.PhaseObserve {
		t.Fatalf("conservative preset phase = %q, want observe", p.Phase)
	}
	if p.TopSecretExceptions {
		t.Fatal("conservative preset must never enable top-secret exceptions")
	}
	if p.ReportCadence != "weekly" {
		t.Fatalf("conservative preset report cadence = %q, want weekly", p.ReportCadence)
	}
	want := map[string]bool{"managed": true, "byoa": true}
	for _, tier := range p.RedactTiers {
		if !want[tier] {
			t.Fatalf("unexpected redact tier %q", tier)
		}
		delete(want, tier)
	}
	if len(want) != 0 {
		t.Fatalf("conservative preset must redact both tiers, missing %v", want)
	}
}

func TestSetupCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		code := onboard.NewSetupCode()
		if len(code) != 14 || code[4] != '-' || code[9] != '-' {
			t.Fatalf("setup code %q has wrong shape (want XXXX-XXXX-XXXX)", code)
		}
		if seen[code] {
			t.Fatalf("duplicate setup code %q", code)
		}
		seen[code] = true
		// Only unambiguous characters may appear.
		for _, r := range code {
			if r != '-' && !strings.ContainsRune("ABCDEFGHJKMNPQRSTUVWXYZ23456789", r) {
				t.Fatalf("setup code %q contains ambiguous char %q", code, r)
			}
		}
		h1, h2 := onboard.SetupCodeHash(code), onboard.SetupCodeHash(code)
		if string(h1) != string(h2) || len(h1) != 32 {
			t.Fatal("setup code hash must be deterministic SHA-256")
		}
		if string(onboard.SetupCodeHash(code+"x")) == string(h1) {
			t.Fatal("different code must hash differently")
		}
	}
}
