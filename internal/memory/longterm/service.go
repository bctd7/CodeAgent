package longterm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// 同目录实例在本进程内串行；跨进程和外部编辑由调用方协调。
var directoryLocks sync.Map

type Service struct {
	root    *os.Root
	gate    chan struct{}
	options Options
}

func Open(directory string, options Options) (*Service, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, fmt.Errorf("longterm: directory required")
	}
	var err error
	options.Limits, err = defaultLimits(options.Limits)
	if err != nil {
		return nil, err
	}
	if options.RequestOptions.MaxTokens < 0 || options.Model.MaxTokens < 0 {
		return nil, fmt.Errorf("longterm: negative output cap")
	}
	if options.Stream != nil && (strings.TrimSpace(options.Model.ID) == "" || strings.TrimSpace(options.Model.Provider) == "" || options.Model.API == "") {
		return nil, fmt.Errorf("longterm: explicit model required for consolidation")
	}
	if options.RequestOptions.MaxTokens == 0 {
		options.RequestOptions.MaxTokens = 4096
	}
	if options.Model.MaxTokens > 0 && options.RequestOptions.MaxTokens > options.Model.MaxTokens {
		options.RequestOptions.MaxTokens = options.Model.MaxTokens
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
	gate, _ := directoryLocks.LoadOrStore(key, make(chan struct{}, 1))
	s := &Service{root: root, gate: gate.(chan struct{}), options: options}
	if err := s.lock(context.Background()); err != nil {
		root.Close()
		return nil, err
	}
	s.unlock()
	return s, nil
}

// Close 不与操作并发；不移除共享锁，避免影响同目录其他实例。
func (s *Service) Close() error { return s.root.Close() }

func (s *Service) lock(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("longterm: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		s.unlock()
		return err
	}
	// 已发布事务先恢复；恢复过程不能被普通取消打断。
	if err := s.recoverPending(); err != nil {
		s.unlock()
		return err
	}
	if err := ctx.Err(); err != nil {
		s.unlock()
		return err
	}
	return nil
}
func (s *Service) unlock() { <-s.gate }

func defaultLimits(l Limits) (Limits, error) {
	defaults := []struct {
		value    *int
		fallback int
	}{
		{&l.Turns, 5}, {&l.ToolCalls, 24}, {&l.InputBytes, 256000}, {&l.OutputBytes, 256000},
		{&l.FileBytes, 64000}, {&l.Files, 256}, {&l.IndexLines, 200}, {&l.IndexRunes, 25000},
	}
	for _, item := range defaults {
		if *item.value < 0 {
			return Limits{}, fmt.Errorf("longterm: negative resource limit")
		}
		if *item.value == 0 {
			*item.value = item.fallback
		}
	}
	return l, nil
}
