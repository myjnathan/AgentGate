// Package ha implements the phase-1 building blocks for F27 high
// availability: a single-writer lock on the data directory (active /
// standby roles) and a readiness probe for load balancers. Phase 2
// adds durable writes (AGENTGATE_FSYNC=1, opt-in fsync on both write
// paths) and standby warm-up (a standby re-reads state.json read-only
// while it waits for the lock). See docs/ha-design.md for the full
// design review, including the honest limits of each phase (shared
// volume is a single point of failure; without fsync a hard power loss
// can lose the last write window).
package ha

import (
	"errors"
	"fmt"
	"os"
)

// Mode selects how this process treats the data-directory lock.
type Mode string

const (
	// ModeActive is the default: fail fast if another instance already
	// holds the data-directory lock.
	ModeActive Mode = "active"
	// ModeStandby waits for the lock: the process parks until the
	// current holder releases it (process death releases the lock,
	// including SIGKILL), then starts as the new active node.
	ModeStandby Mode = "standby"
)

// ParseMode parses AGENTGATE_HA_MODE. Empty means active.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "", string(ModeActive):
		return ModeActive, nil
	case string(ModeStandby):
		return ModeStandby, nil
	default:
		return "", fmt.Errorf("ha: unknown AGENTGATE_HA_MODE %q (want active|standby)", s)
	}
}

// ErrLocked reports that another instance already holds the
// data-directory lock. A second active node must not start; use
// standby mode to wait for takeover instead.
var ErrLocked = errors.New("ha: data directory is locked by another instance")

// Lock is the held single-writer lock on a data directory. It is
// released by Release; the operating system also releases it when the
// process exits, for any reason (including SIGKILL).
type Lock struct {
	f   *os.File
	dir string
}

// Dir returns the data directory this lock guards.
func (l *Lock) Dir() string { return l.dir }

// Release drops the lock. It is safe to call on a nil lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	// Closing the file releases the flock (the lock is tied to the
	// open file description, not to the pathname).
	return l.f.Close()
}

// lockFileName is the lock file inside the data directory. Do not
// delete it by hand while a node is running: flock locks live on the
// inode, so deleting the file and letting a new process recreate it
// would hand out a second, independent lock and defeat the guard.
const lockFileName = "agentgate.lock"
