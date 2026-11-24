//go:build windows

package ha

import (
	"os"
	"path/filepath"
)

// Acquire on Windows is a no-op: flock is a Unix semantic and there is
// no equivalent single-writer guard here. The lock file is still
// created as a marker. High-availability deployments are Linux-only
// (see docs/ha-design.md); Windows nodes are single-node only and
// must not share a data directory.
func Acquire(dir string, mode Mode) (*Lock, bool, error) {
	path := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	return &Lock{f: f, dir: dir}, false, nil
}
