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

func TestAcquireStandbyWarmsUpWhileWaiting(t *testing.T) {
	dir := t.TempDir()
	first, _, err := Acquire(dir, ModeActive)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	warmups := 0
	type result struct {
		waited bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		second, waited, err := AcquireStandby(dir, 20*time.Millisecond, func() error {
			warmups++
			return nil
		})
		if err == nil {
			_ = second.Release()
		}
		done <- result{waited: waited, err: err}
	}()
	// Let the standby poll at least twice, then release: the next
	// poll must hand it the lock.
	time.Sleep(50 * time.Millisecond)
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("standby AcquireStandby: %v", r.err)
		}
		if !r.waited {
			t.Errorf("standby AcquireStandby on a held lock reported waited=false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("standby AcquireStandby did not unblock after release")
	}
	if warmups < 1 {
		t.Errorf("warmup ran %d times while waiting; want >= 1", warmups)
	}
}

func TestAcquireStandbyFreeLockSkipsWarmup(t *testing.T) {
	dir := t.TempDir()
	warmups := 0
	l, waited, err := AcquireStandby(dir, 20*time.Millisecond, func() error {
		warmups++
		return nil
	})
	if err != nil {
		t.Fatalf("AcquireStandby on a free lock: %v", err)
	}
	defer l.Release()
	if waited {
		t.Errorf("AcquireStandby on a free lock reported waited")
	}
	if warmups != 0 {
		t.Errorf("warmup ran %d times on a free lock; want 0", warmups)
	}
}

func TestAcquireStandbyNilWarmup(t *testing.T) {
	dir := t.TempDir()
	first, _, err := Acquire(dir, ModeActive)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		second, _, err := AcquireStandby(dir, 20*time.Millisecond, nil)
		if err == nil {
			_ = second.Release()
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AcquireStandby with nil warmup: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AcquireStandby with nil warmup did not unblock after release")
	}
}
