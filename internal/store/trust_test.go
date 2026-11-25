package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/secretbox"
)

func TestParseTrustType(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want TrustType
		ok   bool
	}{
		{"signed_cloud", TrustSignedCloud, true},
		{"dedicated_cloud", TrustDedicatedCloud, true},
		{"local_model", TrustLocalModel, true},
		{"", "", false},
		{"private", "", false},
		{"SIGNED_CLOUD", "", false},
	} {
		got, ok := ParseTrustType(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("ParseTrustType(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTrustTypeJSONRoundTrip(t *testing.T) {
	for _, tt := range []TrustType{TrustSignedCloud, TrustDedicatedCloud, TrustLocalModel} {
		b, err := json.Marshal(tt)
		if err != nil {
			t.Fatal(err)
		}
		var back TrustType
		if err := json.Unmarshal(b, &back); err != nil || back != tt {
			t.Fatalf("round-trip %q -> %q (%v)", tt, back, err)
		}
	}
	// An explicit empty string normalizes to signed cloud.
	var back TrustType
	if err := json.Unmarshal([]byte(`""`), &back); err != nil || back != TrustSignedCloud {
		t.Fatalf("empty trust type should normalize to signed_cloud, got %q (%v)", back, err)
	}
	// An unknown value is rejected, not silently accepted.
	if err := json.Unmarshal([]byte(`"onprem"`), &back); err == nil {
		t.Fatal("unknown trust type should fail to unmarshal")
	}
}

func TestChannelTrustTypeOrDefault(t *testing.T) {
	// Channels stored before F17 have no trust type: they behave as
	// signed cloud, the previous default.
	ch := &Channel{}
	if got := ch.TrustTypeOrDefault(); got != TrustSignedCloud {
		t.Fatalf("empty trust type default = %q, want signed_cloud", got)
	}
	ch.TrustType = TrustLocalModel
	if got := ch.TrustTypeOrDefault(); got != TrustLocalModel {
		t.Fatalf("explicit trust type = %q, want local_model", got)
	}
}

// TestChannelKeysPersistAcrossSaveAndReload pins the 2026-11-25 fix
// for the F27 drill finding: channel API keys used to live only in
// memory (Channel.Keys / ChannelKey.Enc were json:"-"), so after any
// restart or volume restore every channel had zero keys and every
// model call failed with 502 channel_no_enabled_key. Keys must now
// survive a Save + reload as AES-GCM ciphertext — while the disabled
// state stays runtime-only (F12: a restart re-enables keys and the
// gateway re-proves them against the upstream).
func TestChannelKeysPersistAcrossSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secretbox.New("test-master-key-123456")
	enc1, _ := box.Encrypt("sk-live-key-one")
	enc2, _ := box.Encrypt("sk-live-key-two")
	st.WithLock(func(s *State) {
		s.Channels["ch_x"] = &Channel{ID: "ch_x", Name: "x", BaseURL: "https://llm.example.invalid/v1",
			Keys: []ChannelKey{
				{Enc: enc1, Masked: secretbox.Mask("sk-live-key-one"),
					Disabled: true, DisabledReason: "unauthorized", DisabledAt: time.Now()},
				{Enc: enc2, Masked: secretbox.Mask("sk-live-key-two")},
			}}
	})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	// The snapshot on disk holds ciphertext, never plaintext.
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-live-key-one") || strings.Contains(string(raw), "sk-live-key-two") {
		t.Fatal("state.json must not contain a plaintext channel key")
	}
	if !strings.Contains(string(raw), `"enc"`) {
		t.Fatal("state.json must persist the encrypted channel keys")
	}
	// Reload: keys come back decryptable; the disabled flag does not.
	st2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ch *Channel
	st2.Read(func(s *State) { ch = s.Channels["ch_x"] })
	if ch == nil || len(ch.Keys) != 2 {
		t.Fatalf("reloaded channel has %d keys, want 2", len(ch.Keys))
	}
	for i, want := range []string{"sk-live-key-one", "sk-live-key-two"} {
		plain, err := box.Decrypt(ch.Keys[i].Enc)
		if err != nil || plain != want {
			t.Fatalf("key %d decrypts to %q (%v), want %q", i, plain, err, want)
		}
		if ch.Keys[i].Disabled {
			t.Fatalf("key %d must reload enabled (disabled state is runtime-only)", i)
		}
		if ch.Keys[i].DisabledReason != "" || !ch.Keys[i].DisabledAt.IsZero() {
			t.Fatalf("key %d must reload without disabled reason/time", i)
		}
	}
	if ch.Keys[0].Masked != secretbox.Mask("sk-live-key-one") {
		t.Fatalf("masked value did not survive reload: %q", ch.Keys[0].Masked)
	}
	// A snapshot written before the fix has no "keys" field: it loads
	// as a keyless channel, exactly the old behaviour, until an admin
	// re-enters keys.
	legacy := strings.Replace(string(raw), `"keys":`, `"keys_old":`, 1)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st3, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var legacyCh *Channel
	st3.Read(func(s *State) { legacyCh = s.Channels["ch_x"] })
	if legacyCh == nil || len(legacyCh.Keys) != 0 {
		t.Fatalf("legacy snapshot must load as keyless, got %d keys", len(legacyCh.Keys))
	}
}
