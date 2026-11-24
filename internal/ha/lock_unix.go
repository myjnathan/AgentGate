//go:build !windows

package ha

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Acquire takes the single-writer lock on dir. In active mode it is
// non-blocking and returns ErrLocked (wrapped) when another instance
// already holds the lock. In standby mode it first tries
// non-blocking; if the lock is held it blocks until the holder
// releases it, then returns with waited=true.
func Acquire(dir string, mode Mode) (*Lock, bool, error) {
	path := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("ha: open lock file: %w", err)
	}
	fd := int(f.Fd())
	if mode == ModeActive {
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, false, fmt.Errorf("%w: %s", ErrLocked, dir)
			}
			return nil, false, fmt.Errorf("ha: lock %s: %w", path, err)
		}
		return &Lock{f: f, dir: dir}, false, nil
	}
	// Standby: a fast non-blocking attempt first, so a free lock is
	// taken immediately and waited reports only real contention.
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return &Lock{f: f, dir: dir}, false, nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		_ = f.Close()
		return nil, false, fmt.Errorf("ha: lock %s: %w", path, err)
	}
	// Held by a live primary: block until the kernel releases it on
	// the holder's death.
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("ha: lock %s: %w", path, err)
	}
	return &Lock{f: f, dir: dir}, true, nil
}
