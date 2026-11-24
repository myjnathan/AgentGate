//go:build !windows

package ha

import (
	"errors"
	"testing"
	"time"
)

func TestLockContention(t *testing.T) {
	dir := t.TempDir()
	first, waited, err := Acquire(dir, ModeActive)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if waited {
		t.Errorf("first acquire on a free lock reported waited")
	}
	defer first.Release()

	// A second active node fails fast with ErrLocked.
	_, _, err = Acquire(dir, ModeActive)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second active Acquire = %v; want ErrLocked", err)
	}

	// After release the lock is takeable again.
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, _, err := Acquire(dir, ModeActive)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	defer second.Release()
}

func TestStandbyWaitsForRelease(t *testing.T) {
	dir := t.TempDir()
	first, _, err := Acquire(dir, ModeActive)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	type result struct {
		waited bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		second, waited, err := Acquire(dir, ModeStandby)
		if err == nil {
			_ = second.Release()
		}
		done <- result{waited: waited, err: err}
	}()
	// Let the standby goroutine reach the blocking flock, then
	// release: the kernel must hand the lock over.
	time.Sleep(100 * time.Millisecond)
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("standby Acquire: %v", r.err)
		}
		if !r.waited {
			t.Errorf("standby acquire on a held lock reported waited=false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("standby acquire did not unblock after release")
	}
}

func TestStandbyTakesFreeLockImmediately(t *testing.T) {
	dir := t.TempDir()
	l, waited, err := Acquire(dir, ModeStandby)
	if err != nil {
		t.Fatalf("standby Acquire on a free lock: %v", err)
	}
	defer l.Release()
	if waited {
		t.Errorf("standby acquire on a free lock reported waited")
	}
}
