package runtime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	platform "runtime"
	"strings"
	"sync"

	runtimeconfig "CodeAgent/internal/runtime/config"
)

type ownership struct {
	key  string
	file *os.File
}

var localOwners = struct {
	sync.Mutex
	keys map[string]bool
}{keys: make(map[string]bool)}

// canonicalDatabasePath 消除相对路径、符号链接/目录联接与 Windows 大小写别名。
func canonicalDatabasePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("数据库路径不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	} else if os.IsNotExist(err) {
		parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
		if err != nil {
			return "", err
		}
		abs = filepath.Join(parent, filepath.Base(abs))
	} else {
		return "", err
	}
	if platform.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	return filepath.Clean(abs), nil
}

func acquireOwnership(ctx context.Context, database, sessionID string) (*ownership, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := database + "\x00" + sessionID
	localOwners.Lock()
	if localOwners.keys[key] {
		localOwners.Unlock()
		return nil, runtimeconfig.ErrSessionOwned
	}
	localOwners.keys[key] = true
	localOwners.Unlock()
	ok := false
	defer func() {
		if !ok {
			localOwners.Lock()
			delete(localOwners.keys, key)
			localOwners.Unlock()
		}
	}()
	dir := database + ".locks"
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%x.lock", sha256.Sum256([]byte(sessionID)))
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		unlockFile(f)
		f.Close()
		return nil, err
	}
	ok = true
	return &ownership{key: key, file: f}, nil
}

// release 不删除锁文件，防止另一拥有者锁住不同的文件对象。
func (o *ownership) release() error {
	err := unlockFile(o.file)
	closeErr := o.file.Close()
	localOwners.Lock()
	delete(localOwners.keys, o.key)
	localOwners.Unlock()
	if err != nil {
		return err
	}
	return closeErr
}
