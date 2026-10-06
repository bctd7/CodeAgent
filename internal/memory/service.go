package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"CodeAgent/internal/llm"
)

// 相同目录的多个 Service 在本进程内共享访问串行锁，读写操作均使用。外部进程应由调用方协调。
var directoryLocks sync.Map

type Service struct {
	root    *os.Root
	gate    chan struct{}
	options Options
}

func Open(directory string, options Options) (*Service, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	// 根目录及祖先不接受符号链接；子文件另由 os.Root 限制访问范围。
	for p := abs; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("memory: symlink directory %q", p)
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
	options.Limits = defaultLimits(options.Limits)
	// 复制选项中的可变字段，调用方之后修改配置不会污染本次服务。
	o := options.RequestOptions
	if o.MaxTokens <= 0 {
		o.MaxTokens = 4096
	}
	options.RequestOptions = cloneOptions(o)
	return &Service{root: root, gate: gate.(chan struct{}), options: options}, nil
}

// Close 后不得继续使用 Service，也不得与正在执行的操作并发调用。
func (s *Service) Close() error { return s.root.Close() }

func (s *Service) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) unlock() { <-s.gate }

func defaultLimits(l Limits) Limits {
	if l.Turns <= 0 {
		l.Turns = 5
	}
	if l.ToolCalls <= 0 {
		l.ToolCalls = 24
	}
	if l.InputBytes <= 0 {
		l.InputBytes = 256000
	}
	if l.OutputBytes <= 0 {
		l.OutputBytes = 256000
	}
	if l.FileBytes <= 0 {
		l.FileBytes = 64000
	}
	if l.Files <= 0 {
		l.Files = 256
	}
	if l.IndexLines <= 0 {
		l.IndexLines = 200
	}
	if l.IndexRunes <= 0 {
		l.IndexRunes = 25000
	}
	return l
}

func cloneOptions(in llm.RequestOptions) llm.RequestOptions {
	if in.Headers != nil {
		headers := make(map[string]string, len(in.Headers))
		for k, v := range in.Headers {
			headers[k] = v
		}
		in.Headers = headers
	}
	if in.Temperature != nil {
		v := *in.Temperature
		in.Temperature = &v
	}
	if in.OpenAI != nil {
		v := *in.OpenAI
		in.OpenAI = &v
	}
	if in.Anthropic != nil {
		v := *in.Anthropic
		in.Anthropic = &v
	}
	return in
}

func cloneRequest(in llm.Request) (llm.Request, error) {
	c, err := (llm.RequestContext{SystemPrompt: in.SystemPrompt, Messages: in.Messages}).Clone()
	if err != nil {
		return llm.Request{}, err
	}
	in.Messages = c.Messages
	in.Options = cloneOptions(in.Options)
	in.Tools = append([]llm.Tool(nil), in.Tools...)
	for i := range in.Tools {
		in.Tools[i].Schema = append([]byte(nil), in.Tools[i].Schema...)
	}
	return in, nil
}
