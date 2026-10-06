//go:build windows

package ownership_lock

import (
	runtimeconfig "CodeAgent/internal/runtime/config"
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func LockFile(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return runtimeconfig.ErrSessionOwned
	}
	return err
}

func UnlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
