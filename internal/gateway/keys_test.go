package gateway

import (
	"path/filepath"
	"testing"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/store"
)

// TestNextKeyRoundRobin covers the key selection adapted from
// new-api's GetNextEnabledKey (polling mode): the cursor advances on
// every pick, disabled keys are skipped by the ring scan, and an
// explicit false is returned when no key is enabled.
func TestNextKeyRoundRobin(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ch := &store.Channel{ID: "ch_rr", Name: "rr (test)",
		Keys:   []store.ChannelKey{{Enc: "e0", Masked: "****e0"}, {Enc: "e1", Masked: "****e1"}, {Enc: "e2", Masked: "****e2"}},
		Models: []string{"mock-rr"}, MaxDataLevel: store.LevelInternal}
	st.WithLock(func(s *store.State) { s.Channels[ch.ID] = ch })
	gw := &Gateway{st: st, audit: al, keyNext: map[string]int{}}

	// Round-robin: 0, 1, 2, then wraps to 0.
	want := []int{0, 1, 2, 0, 1}
	for i, w := range want {
		idx, enc, ok := gw.nextKey(ch)
		if !ok || idx != w || enc != "e"+string(rune('0'+w)) {
			t.Fatalf("pick %d: got idx=%d enc=%q ok=%v, want idx=%d", i, idx, enc, ok, w)
		}
	}

	// Disabling key 1 removes it from the rotation without disturbing
	// the others; the cursor keeps advancing past it.
	gw.disableKey(ch, 1, "test_disable")
	if got := ch.EnabledKeyCount(); got != 2 {
		t.Fatalf("enabled keys = %d, want 2", got)
	}
	// Cursor is at 2 after the five picks above (0,1,2,0,1 -> next 2).
	seen := map[int]bool{}
	for i := 0; i < 4; i++ {
		idx, _, ok := gw.nextKey(ch)
		if !ok {
			t.Fatalf("pick after disable %d: no key, want one", i)
		}
		if idx == 1 {
			t.Fatal("disabled key 1 was selected")
		}
		seen[idx] = true
	}
	if !seen[0] || !seen[2] {
		t.Fatalf("rotation after disable only saw %v, want both 0 and 2", seen)
	}

	// Disabling everything reports no key instead of falling back to a
	// dead one (the new-api bug fix this mirrors).
	gw.disableKey(ch, 0, "test_disable")
	gw.disableKey(ch, 2, "test_disable")
	if _, _, ok := gw.nextKey(ch); ok {
		t.Fatal("nextKey with all keys disabled returned a key")
	}

	// The disables were audited (and therefore alertable via F5).
	found := 0
	for _, ev := range al.All() {
		if ev.Kind == "channel" && ev.Action == "channel_key_disabled" {
			found++
			if ev.Detail["key_index"] == "" || ev.Detail["reason"] == "" {
				t.Fatalf("key_disabled audit missing detail: %+v", ev.Detail)
			}
		}
	}
	if found != 3 {
		t.Fatalf("channel_key_disabled events = %d, want 3", found)
	}
}

// TestNextKeyEmptyChannel covers a channel with no keys at all.
func TestNextKeyEmptyChannel(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ch := &store.Channel{ID: "ch_empty", Name: "empty (test)"}
	st.WithLock(func(s *store.State) { s.Channels[ch.ID] = ch })
	gw := &Gateway{st: st, audit: al, keyNext: map[string]int{}}
	if _, _, ok := gw.nextKey(ch); ok {
		t.Fatal("nextKey on a keyless channel returned a key")
	}
}
