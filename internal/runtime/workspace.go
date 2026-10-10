package runtime

import (
	"CodeAgent/internal/runtime/ownership"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	platform "runtime"
	"sort"
	"strings"
	"time"
)

type fileClaim struct {
	owner string
	file  *os.File
}

// 一次占用请求的短期等待边；按实时路径占用查询阻塞者，不缓存过期持有者。
type fileWait struct {
	owner, path string
	ctx         context.Context
}

// 调用者持有 h.mu。父 Run 必须收齐子 Run，故活动子执行也是父的生命周期依赖。
func (h *Host) fileWaitReaches(from, target string, visited map[string]bool) bool {
	if from == target {
		return true
	}
	if visited[from] {
		return false
	}
	visited[from] = true
	for wait := range h.fileWaits {
		if wait.owner == from && wait.ctx.Err() == nil {
			if claim, ok := h.workspace[wait.path]; ok && claim.owner != from && h.fileWaitReaches(claim.owner, target, visited) {
				return true
			}
		}
	}
	for child, parent := range h.fileParents {
		if parent == from && h.fileWaitReaches(child, target, visited) {
			return true
		}
	}
	return false
}

func directory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("不是目录: %s", path)
	}
	return nil
}

// realPath 解析真实已有祖先，允许尚不存在的文件；不创建验证目标。
func realPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(absolute)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			if platform.GOOS == "windows" {
				resolved = strings.ToLower(resolved)
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(absolute)
		if parent == absolute {
			return "", err
		}
		tail = append(tail, filepath.Base(absolute))
		absolute = parent
	}
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func isAbsolute(path string) bool          { return filepath.IsAbs(path) }
func projectPath(root, path string) string { return filepath.Join(root, path) }

func (h *Host) lockFile(kind, key string) (*os.File, error) {
	dir := filepath.Join(h.options.OwnershipDir, kind)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// 摘要只形成 OS 锁文件名，不作为消息、来源或业务身份。
	name := fmt.Sprintf("%x.lock", sha256.Sum256([]byte(key)))
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := ownership.Lock(f); err != nil {
		_ = f.Close()
		if ownership.Busy(err) {
			return nil, fmt.Errorf("%w: %v", ErrOwned, err)
		}
		return nil, err
	}
	return f, nil
}

func closeLock(f *os.File) error { return errors.Join(ownership.Unlock(f), f.Close()) }

// claim 保留到 Run 收尾；等待不持有 Host 锁，取消不重放工具。
func (h *Host) claim(ctx context.Context, owner string, paths []string, parentOwners ...string) error {
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	wait := &fileWait{owner: owner, ctx: ctx}
	defer func() { h.mu.Lock(); delete(h.fileWaits, wait); h.mu.Unlock() }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h.mu.Lock()
		delete(h.fileWaits, wait)
		if len(parentOwners) > 0 && parentOwners[0] != "" {
			if h.fileParents == nil {
				h.fileParents = map[string]string{}
			}
			h.fileParents[owner] = parentOwners[0]
		}
		var added []string
		var err error
		blockedPath := ""
		for _, path := range paths {
			if previous, ok := h.workspace[path]; ok {
				if previous.owner != owner {
					err = ErrOwned
					blockedPath = path
					break
				}
				continue
			}
			var f *os.File
			f, err = h.lockFile("files", path)
			if err != nil {
				break
			}
			h.workspace[path] = fileClaim{owner, f}
			added = append(added, path)
		}
		if err != nil {
			for _, path := range added {
				_ = closeLock(h.workspace[path].file)
				delete(h.workspace, path)
			}
		}
		if blockedPath != "" {
			wait.path = blockedPath
			if h.fileWaits == nil {
				h.fileWaits = map[*fileWait]struct{}{}
			}
			h.fileWaits[wait] = struct{}{}
			if h.fileWaitReaches(h.workspace[blockedPath].owner, owner, map[string]bool{}) {
				delete(h.fileWaits, wait)
				err = fmt.Errorf("文件占用等待形成环，拒绝当前请求: %s", blockedPath)
			}
		}
		h.mu.Unlock()
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrOwned) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (h *Host) releaseClaims(owner string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.fileParents, owner)
	var failures []error
	for path, claim := range h.workspace {
		if claim.owner == owner {
			failures = append(failures, closeLock(claim.file))
			delete(h.workspace, path)
		}
	}
	return errors.Join(failures...)
}
