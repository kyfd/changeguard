//go:build windows

package store

import (
	"errors"
	"syscall"
)

// Windows 不在 syscall 包中导出这两个错误码，按 Win32 定义的值比较：
// ERROR_SHARING_VIOLATION=32、ERROR_LOCK_VIOLATION=33。
const (
	errorSharingViolation = syscall.Errno(32)
	errorLockViolation    = syscall.Errno(33)
)

var errLockBusy = errors.New("instance lock busy")

// lockFileExclusive opens the lock file with dwShareMode=0: a second process
// cannot open the same file at all, which is an exclusive inter-process lock on
// Windows without extra dependencies. The handle stays open until release, and
// the OS closes it if the process dies.
func lockFileExclusive(path string) (func() error, error) {
	pointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(
		pointer,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, errorSharingViolation) || errors.Is(err, errorLockViolation) {
			return nil, errLockBusy
		}
		return nil, err
	}
	return func() error { return syscall.CloseHandle(handle) }, nil
}
