//go:build linux || darwin

package filechange

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func platformMode(mode fs.FileMode) fs.FileMode { return mode }

// 非阻塞打开可避免检查之后被换成 FIFO 时永久等待；打开后还会再次检查普通文件身份。
func readFlags() int { return os.O_RDONLY | unix.O_NONBLOCK | unix.O_NOFOLLOW }

func reparse(info fs.FileInfo) bool { return info.Mode()&fs.ModeSymlink != 0 }

func fileIdentity(f *os.File) (string, uint64, error) {
	var info unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &info); err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%d:%d", info.Dev, info.Ino), uint64(info.Nlink), nil
}

func privateDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err := checkDirectoryPath(path); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func privateFile(f *os.File) error { return f.Chmod(0600) }

func lockJournal(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrBusy
	}
	return err
}

func syncDirectory(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func replaceFile(parent *os.Root, from, to, expectedID string) error {
	return parent.Rename(from, to)
}

// unlinkat(flags=0) 无论目标如何变化都不会删除目录；不使用 os.Remove 的 rmdir 回退。
func removeFile(parent *os.Root, name, expectedID string) error {
	if filepath.Base(name) != name || name == "." || name == ".." || expectedID == "" {
		return ErrInvalid
	}
	f, err := parent.OpenFile(name, readFlags(), 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := verifyOpened(parent, name, f); err != nil {
		return err
	}
	id, _, err := fileIdentity(f)
	if err != nil {
		return err
	}
	if id != expectedID {
		return fmt.Errorf("%w: deletion target identity changed", ErrConflict)
	}
	dir, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return unix.Unlinkat(int(dir.Fd()), name, 0)
}
