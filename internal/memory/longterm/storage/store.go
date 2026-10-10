// Package storage 管理长期记忆目录、文档与事务，由 longterm 业务层统一调用。
package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

var (
	ErrBudget        = errors.New("longterm: consolidation budget exceeded")
	ErrCommitPending = errors.New("longterm: published commit requires recovery")
)

type Limits struct {
	FileBytes  int
	Files      int
	IndexLines int
	IndexRunes int
}

type Document struct {
	Path      string
	Content   string
	Truncated bool
}

type Match struct {
	Path string
	Line int
	Text string
}

// Entry 包含主题摘要、类型、正文与来源引用；业务层负责提供来源依据。
type Entry struct {
	Path    string
	Summary string
	Type    string
	Content string
	Sources []string
}

// 同目录整理在本进程内串行；已提交读取不等待模型执行。跨进程和外部编辑由调用方协调。
var directoryLocks sync.Map

type directoryLock struct {
	writer chan struct{}
	data   chan struct{}
}

type Store struct {
	root   *os.Root
	locks  *directoryLock
	limits Limits
}

// New 创建指定目录的存储对象，limits 由调用方补齐默认值并校验；加锁与提交恢复在实际数据操作时执行。
func New(directory string, limits Limits) (*Store, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, fmt.Errorf("longterm: directory required")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	for p := abs; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("longterm: symlink directory %q", p)
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	key := filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	locks, _ := directoryLocks.LoadOrStore(key, &directoryLock{writer: make(chan struct{}, 1), data: make(chan struct{}, 1)})
	return &Store{root: root, locks: locks.(*directoryLock), limits: limits}, nil
}

func acquire(ctx context.Context, gate chan struct{}) error {
	if ctx == nil {
		return fmt.Errorf("longterm: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-gate
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// committed 串行保护一次完整读取及必要的恢复，不获取整理锁。
// 已发布日志的恢复是完成既有提交，不创建新批次或回执。
func (s *Store) committed(ctx context.Context, read func(*Transaction) error) error {
	if err := acquire(ctx, s.locks.data); err != nil {
		return err
	}
	defer func() { <-s.locks.data }()
	if err := s.recoverPending(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return read(&Transaction{store: s, staged: map[string]*string{}, cache: map[string]string{}})
}

// List 列出已提交主题；读取视图只在本次调用内有效。
func (s *Store) List(ctx context.Context) (names []string, err error) {
	err = s.committed(ctx, func(v *Transaction) error {
		var e error
		names, e = v.Files()
		if e != nil {
			return e
		}
		for _, name := range names {
			if e = ctx.Err(); e != nil {
				return e
			}
			if _, e = v.Read(name); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// Read 读取已提交索引或主题，不使用整理任务的暂存视图。
func (s *Store) Read(ctx context.Context, name string) (doc Document, err error) {
	err = s.committed(ctx, func(v *Transaction) error {
		var e error
		if name == IndexName {
			doc, e = v.ReadCommittedIndex()
		} else {
			doc, e = v.Read(name)
		}
		return e
	})
	if err != nil {
		return Document{}, err
	}
	return doc, nil
}

// Search 一次检索中的文件来自同一已提交状态。
func (s *Store) Search(ctx context.Context, query string, limit int) (matches []Match, err error) {
	err = s.committed(ctx, func(v *Transaction) error {
		var e error
		matches, e = v.Search(ctx, query, limit)
		return e
	})
	if err != nil {
		return nil, err
	}
	return matches, nil
}

// Close 不与操作并发；保留共享锁，供同目录其他实例使用。
func (s *Store) Close() error { return s.root.Close() }

// 仅接受根目录普通 Markdown 文件，索引只允许服务生成。
func ValidPath(name string) error {
	if !utf8.ValidString(name) || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") || len(name) > 160 {
		return fmt.Errorf("longterm: invalid Markdown path %q", name)
	}
	stem := strings.TrimSuffix(name, ".md")
	if stem == "" {
		return fmt.Errorf("longterm: empty filename")
	}
	if strings.EqualFold(name, IndexName) && name != IndexName {
		return fmt.Errorf("longterm: reserved index name")
	}
	if name != IndexName && stem != strings.ToLower(stem) {
		return fmt.Errorf("longterm: topic filename must be lowercase")
	}
	upper := strings.ToUpper(stem)
	device := upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL" || len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '0' && upper[3] <= '9'
	// Windows 也识别上标 1/2/3 的 COM/LPT 设备名。
	if device || upper == "COM¹" || upper == "COM²" || upper == "COM³" || upper == "LPT¹" || upper == "LPT²" || upper == "LPT³" {
		return fmt.Errorf("longterm: reserved device name")
	}
	for _, r := range stem {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
			return fmt.Errorf("longterm: invalid filename %q", name)
		}
	}
	return nil
}

func (s *Store) readFile(name string, limit int) (string, error) {
	info, err := s.root.Lstat(name)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("longterm: non-regular file %q", name)
	}
	if info.Size() > int64(limit) {
		return "", fmt.Errorf("%w: file %q", ErrBudget, name)
	}
	f, err := s.root.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !opened.Mode().IsRegular() {
		return "", fmt.Errorf("longterm: non-regular file %q", name)
	}
	bound := int64(limit)
	if bound < int64(^uint64(0)>>1) {
		bound++
	}
	b, err := io.ReadAll(io.LimitReader(f, bound))
	if err != nil {
		return "", err
	}
	if len(b) > limit {
		return "", fmt.Errorf("%w: file %q", ErrBudget, name)
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("longterm: invalid UTF-8 in %q", name)
	}
	return string(b), nil
}

func (s *Store) topicNames() ([]string, error) {
	f, err := s.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, entry := range entries {
		if entry.Name() == IndexName || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if err := ValidPath(entry.Name()); err != nil {
			return nil, err
		}
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("longterm: non-regular topic %q", entry.Name())
		}
		names = append(names, entry.Name())
		if len(names) > s.limits.Files {
			return nil, fmt.Errorf("%w: topic count", ErrBudget)
		}
	}
	sort.Strings(names)
	return names, nil
}

// 临时文件 Sync 后 Rename；发布前检查取消，不沿链接覆盖目标。
func (s *Store) atomicWrite(ctx context.Context, name, content string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if info, err := s.root.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("longterm: non-regular destination %q", name)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := ".memory-" + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer s.root.Remove(tmp)
	_, err = io.WriteString(f, content)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.root.Rename(tmp, name)
}

func (s *Store) removeRegular(name string) error {
	info, err := s.root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("longterm: non-regular deletion %q", name)
	}
	return s.root.Remove(name)
}

func ValidID(id string, max int) bool {
	return utf8.ValidString(id) && strings.TrimSpace(id) != "" && len(id) <= max
}
