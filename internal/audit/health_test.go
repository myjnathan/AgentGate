package audit

import "testing"

// A directory as the log path: Open reads nothing from it, but every
// Append fails to open it for writing — a deterministic stand-in for
// a full or unwritable disk.
func TestLastErrorTracksAppendFailures(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(dir): %v", err)
	}
	if got := l.LastError(); got != nil {
		t.Fatalf("LastError() = %v before any append; want nil", got)
	}
	l.Append(Event{Actor: "alice", ActorID: "usr_1", Kind: "auth", Action: "login"})
	if got := l.LastError(); got == nil {
		t.Fatal("LastError() = nil after a failed append; want an error")
	}

	// A healthy log reports nil.
	ok := openTestLog(t)
	ok.Append(Event{Actor: "alice", ActorID: "usr_1", Kind: "auth", Action: "login"})
	if got := ok.LastError(); got != nil {
		t.Fatalf("LastError() = %v after a good append; want nil", got)
	}
}
