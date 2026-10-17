//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package store

import (
	"errors"
	"runtime"
)

var errLockBusy = errors.New("instance lock busy")

// lockFileExclusive fails closed on platforms without a verified exclusive-lock
// implementation, so the file backend never runs without the single-instance
// guarantee it claims.
func lockFileExclusive(string) (func() error, error) {
	return nil, errors.New("the file state backend has no verified instance lock on " + runtime.GOOS + "; use CHANGEGUARD_STORE_MODE=postgres")
}
