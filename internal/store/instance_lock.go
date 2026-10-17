package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrInstanceLocked reports that another core process already holds the file
// state backend. 4.0 makes the single-instance semantics explicit: the second
// process must fail closed instead of interleaving writes with the first one.
var ErrInstanceLocked = errors.New("state file is already locked by another ChangeGuard instance")

// instanceLock holds an exclusive lock next to the state file.
type instanceLock struct {
	path     string
	closer   func() error
	released bool
}

// acquireInstanceLock locks <stateFile>.instance.lock. The lock is per state
// file, so two processes using different data files never block each other.
func acquireInstanceLock(statePath string) (*instanceLock, error) {
	lockPath := statePath + ".instance.lock"
	if dir := filepath.Dir(lockPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create state instance lock directory: %w", err)
		}
	}
	closer, err := lockFileExclusive(lockPath)
	if err != nil {
		if errors.Is(err, errLockBusy) {
			return nil, fmt.Errorf("%w: %s; stop the other core process, or use CHANGEGUARD_STORE_MODE=postgres when several instances must run", ErrInstanceLocked, lockPath)
		}
		return nil, fmt.Errorf("acquire state instance lock %s: %w", lockPath, err)
	}
	return &instanceLock{path: lockPath, closer: closer}, nil
}

// Release drops the lock. It is safe to call more than once.
func (l *instanceLock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	return l.closer()
}
