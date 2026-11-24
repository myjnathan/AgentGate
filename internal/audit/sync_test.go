package audit

import (
	"path/filepath"
	"testing"
)

// With sync on, appends still land durably and the chain verifies
// after a fresh open — the fsync must not change what is written,
// only how durably it is written.
func TestLogSyncWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.SetSync(true)
	l.Append(Event{Actor: "alice", ActorID: "usr_1", Kind: "auth", Action: "login"})
	l.Append(Event{Actor: "alice", ActorID: "usr_1", Kind: "model_call", Action: "chat"})
	if got := l.LastError(); got != nil {
		t.Fatalf("LastError() = %v after synced appends; want nil", got)
	}

	// A fresh open re-reads both events and the chain verifies.
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.All()); got != 2 {
		t.Fatalf("reopened log has %d events; want 2", got)
	}
	if !reopened.Verify() {
		t.Fatal("reopened log failed chain verification")
	}

	// Turning sync back off keeps appending normally.
	reopened.SetSync(false)
	reopened.Append(Event{Actor: "bob", ActorID: "usr_2", Kind: "auth", Action: "login"})
	if got := len(reopened.All()); got != 3 {
		t.Fatalf("log has %d events after sync-off append; want 3", got)
	}
	if got := reopened.LastError(); got != nil {
		t.Fatalf("LastError() = %v after sync-off append; want nil", got)
	}

	// A sync failure still surfaces through LastError: point the log
	// at a directory so the append (and therefore the write) fails.
	broken, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	broken.SetSync(true)
	broken.Append(Event{Actor: "alice", ActorID: "usr_1", Kind: "auth", Action: "login"})
	if got := broken.LastError(); got == nil {
		t.Fatal("LastError() = nil after a failed synced append; want an error")
	}
}
