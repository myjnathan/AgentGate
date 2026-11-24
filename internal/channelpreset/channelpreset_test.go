package channelpreset_test

import (
	"testing"

	"github.com/myjnathan/agentgate/internal/channelpreset"
)

func TestPresetsAreValidAndUnique(t *testing.T) {
	seen := map[string]bool{}
	ps := channelpreset.Presets()
	if len(ps) < 8 {
		t.Fatalf("expected at least 8 presets, got %d", len(ps))
	}
	for _, p := range ps {
		if seen[p.ID] {
			t.Errorf("duplicate preset id %q", p.ID)
		}
		seen[p.ID] = true
		if !channelpreset.Valid(p) {
			t.Errorf("preset %q is not valid: %+v", p.ID, p)
		}
		if p.NameEN == "" {
			t.Errorf("preset %q lacks an English name", p.ID)
		}
	}
	if !seen["custom"] {
		t.Error("custom escape-hatch preset missing")
	}
	if ps[len(ps)-1].ID != "custom" {
		t.Error("custom preset must stay last in display order")
	}
}

func TestPresetTrustAndLevelSemantics(t *testing.T) {
	// Signed-cloud vendors cap at internal by default (F17: a
	// signed cloud may carry confidential only after gateway
	// redaction, so the preset does not opt into it silently);
	// the local model is the only preset that may carry secret.
	for _, p := range channelpreset.Presets() {
		switch p.TrustType {
		case "signed_cloud":
			if p.MaxDataLevel != "internal" {
				t.Errorf("signed-cloud preset %q caps at %q, want internal", p.ID, p.MaxDataLevel)
			}
		case "local_model":
			if p.MaxDataLevel != "secret" {
				t.Errorf("local-model preset %q caps at %q, want secret", p.ID, p.MaxDataLevel)
			}
		default:
			t.Errorf("preset %q has unexpected trust type %q", p.ID, p.TrustType)
		}
	}
	ollama, ok := channelpreset.ByID("ollama")
	if !ok {
		t.Fatal("ollama preset missing")
	}
	if ollama.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("ollama base %q", ollama.BaseURL)
	}
	if _, ok := channelpreset.ByID("no-such-vendor"); ok {
		t.Error("unknown id must not resolve")
	}
}

func TestValidRejectsBadPresets(t *testing.T) {
	bad := []channelpreset.Preset{
		{ID: "x", Name: "X", TrustType: "mystery_cloud", MaxDataLevel: "internal", BaseURL: "https://x.invalid/v1", Models: []string{"m"}},
		{ID: "x", Name: "X", TrustType: "signed_cloud", MaxDataLevel: "ultra", BaseURL: "https://x.invalid/v1", Models: []string{"m"}},
		{ID: "x", Name: "X", TrustType: "signed_cloud", MaxDataLevel: "internal", BaseURL: "https://x.invalid/v1"},
		{ID: "x", Name: "X", TrustType: "signed_cloud", MaxDataLevel: "internal", BaseURL: "http://x.invalid/v1", Models: []string{"m"}},
		{ID: "x", Name: "X", TrustType: "signed_cloud", MaxDataLevel: "internal", BaseURL: "https://x.invalid/v1", Models: []string{"m"}},
	}
	// The last entry is valid; the rest must fail.
	for i, p := range bad[:len(bad)-1] {
		if channelpreset.Valid(p) {
			t.Errorf("bad preset %d accepted: %+v", i, p)
		}
	}
	if !channelpreset.Valid(bad[len(bad)-1]) {
		t.Errorf("good preset rejected: %+v", bad[len(bad)-1])
	}
}
