//go:build linux || darwin

package ownership_lock

import (
	runtimeconfig "CodeAgent/internal/runtime/config"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func LockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return runtimeconfig.ErrSessionOwned
	}
	return err
}

func UnlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
