//go:build windows

// Package ownership 只封装平台排他文件锁，不判断会话或业务执行状态。
package ownership

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func Busy(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }

func Lock(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}
func Unlock(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}
