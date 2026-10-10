//go:build linux || darwin

package ownership

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func Busy(err error) bool { return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) }

func Lock(file *os.File) error   { return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func Unlock(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }
