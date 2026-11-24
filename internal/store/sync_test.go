package store

import (
	"os"
	"path/filepath"
	"testing"
)

// With sync on, a save still round-trips through a fresh load, and
// the state file on disk is a complete snapshot.
func TestStoreSaveSync(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st.SetSync(true)
	st.WithLock(func(s *State) {
		s.Users["usr_test"] = &User{ID: "usr_test", Username: "testwarm", Role: "employee"}
	})
	if err := st.Save(); err != nil {
		t.Fatalf("Save with sync on: %v", err)
	}
	if got := st.LastSaveError(); got != nil {
		t.Fatalf("LastSaveError() = %v after a synced save; want nil", got)
	}

	reloaded, err := New(dir)
	if err != nil {
		t.Fatalf("New after synced save: %v", err)
	}
	var found bool
	reloaded.Read(func(s *State) {
		_, found = s.Users["usr_test"]
	})
	if !found {
		t.Fatal("synced save did not persist the new user")
	}

	// Turning sync back off keeps saving normally.
	reloaded.SetSync(false)
	reloaded.WithLock(func(s *State) {
		delete(s.Users, "usr_test")
	})
	if err := reloaded.Save(); err != nil {
		t.Fatalf("Save with sync off: %v", err)
	}
	if got := reloaded.LastSaveError(); got != nil {
		t.Fatalf("LastSaveError() = %v after a sync-off save; want nil", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("stale state.json.tmp left behind: %v", err)
	}
}
