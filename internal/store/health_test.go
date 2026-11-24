package store

import (
	"os"
	"testing"
)

func TestLastSaveErrorTracksSaveFailures(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := st.LastSaveError(); got != nil {
		t.Fatalf("LastSaveError() = %v before any save; want nil", got)
	}
	if err := st.Save(); err != nil {
		t.Fatalf("Save on a fresh dir: %v", err)
	}
	if got := st.LastSaveError(); got != nil {
		t.Fatalf("LastSaveError() = %v after a good save; want nil", got)
	}

	// Remove the directory: the next save cannot write its tmp file.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := st.Save(); err == nil {
		t.Fatal("Save into a removed directory succeeded; want an error")
	}
	if got := st.LastSaveError(); got == nil {
		t.Fatal("LastSaveError() = nil after a failed save; want an error")
	}

	// Recreate it: saves succeed again and the error clears.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := st.Save(); err != nil {
		t.Fatalf("Save after recreating dir: %v", err)
	}
	if got := st.LastSaveError(); got != nil {
		t.Fatalf("LastSaveError() = %v after recovery; want nil", got)
	}
}
