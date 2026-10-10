// Package runtime 装配现有独立包并协调执行交接，不实现模型循环或存储业务。
package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/hooks"
	"CodeAgent/internal/instructions"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/mcp"
	"CodeAgent/internal/memory/compact"
	"CodeAgent/internal/memory/longterm"
	"CodeAgent/internal/permission"
	"CodeAgent/internal/provider"
	"CodeAgent/internal/session"
	sc "CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"
	"CodeAgent/internal/skills"
	"CodeAgent/internal/tools/extend/webfetch"
)

var (
	ErrClosed        = errors.New("runtime 已关闭")
	ErrBusy          = errors.New("runtime 尚有活动工作")
	ErrOwned         = errors.New("会话或文件已有执行者")
	ErrStaleApproval = errors.New("审批已经失效")
)

// ModelConfig 由宿主显式解析；嵌套选项在使用期间只读，凭据不持久化。
type ModelConfig struct {
	Model   llm.Model
	Options llm.RequestOptions
}

// Options 接收已初始化的 Store；资源关闭归属显式声明，不读取实际配置或数据库迁移。
type Options struct {
	Store                        *store.Store
	CloseStore                   bool
	OwnershipDir                 string
	ProjectDir                   string
	Models                       map[string]ModelConfig
	ModelRef                     string
	Stream                       agent.StreamFunc
	SystemPrompt                 string
	Hooks                        *hooks.Engine
	ModelHooks                   *hooks.ModelHooks
	Policy                       permission.Policy
	Tools                        []ToolBinding // nil 显式装配七个默认工具；空切片表示不启用基础工具。
	Instructions                 instructions.Options
	Skills                       []skills.Source
	Compact                      compact.Settings // 零值沿用 compact 的合法零值，不偷偷填充独立模型。
	MemoryDirectory, MemoryScope string
	MemoryLimits                 longterm.Limits
	MCP                          *mcp.Manager
	CloseMCP                     bool
	// MCPShared 借用外部已启动的 Manager；Host 不启动、刷新、重连或关闭其连接。
	MCPShared        bool
	WebFetch         *webfetch.WebFetchOptions
	Subagents        bool
	MaxParallelTools int
	RecordTimeout    time.Duration
	OnEvent          func(sessionID string, event Event)
}

type Host struct {
	mu              sync.Mutex
	catalogMu       sync.Mutex
	catalogChanging bool
	options         Options
	service         *session.Service
	runtimes        map[string]*Runtime
	opening         sync.WaitGroup
	closing         bool
	closeOnce       sync.Once
	closed          chan struct{}
	closeErr        error
	workspace       map[string]fileClaim
	fileWaits       map[*fileWait]struct{}
	fileParents     map[string]string
	observer        *observer
}

func NewHost(ctx context.Context, options Options) (*Host, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Store == nil || options.OwnershipDir == "" || options.ProjectDir == "" {
		return nil, fmt.Errorf("Store、OwnershipDir、ProjectDir 必须显式提供")
	}
	if options.MCPShared && options.CloseMCP {
		return nil, fmt.Errorf("共享 MCP Manager 不能由 Host 关闭")
	}
	project, err := realPath(options.ProjectDir)
	if err != nil {
		return nil, err
	}
	if err := directory(project); err != nil {
		return nil, err
	}
	options.ProjectDir = project
	options.OwnershipDir, err = realPath(options.OwnershipDir)
	if err != nil {
		return nil, err
	}
	if _, ok := options.Models[options.ModelRef]; !ok || options.ModelRef == "" {
		return nil, fmt.Errorf("必须选择已配置模型引用")
	}
	for ref, model := range options.Models {
		if err := validModel(ref, model); err != nil {
			return nil, err
		}
	}
	if options.Stream == nil {
		options.Stream = provider.Stream
	}
	if options.RecordTimeout == 0 {
		options.RecordTimeout = 5 * time.Second
	}
	if options.RecordTimeout < 0 || options.MaxParallelTools < 0 {
		return nil, fmt.Errorf("运行限制不能为负")
	}
	options.Policy.WorkingDir = project
	if options.Policy.WorkMode == "" {
		options.Policy.WorkMode = permission.Build
	}
	if options.Policy.ApprovalMode == "" {
		options.Policy.ApprovalMode = permission.Default
	}
	if options.Policy.PlanDir != "" {
		options.Policy.PlanDir, err = realPath(options.Policy.PlanDir)
		if err != nil {
			return nil, err
		}
	}
	if (options.MemoryDirectory == "") != (options.MemoryScope == "") {
		return nil, fmt.Errorf("记忆目录与 Scope 必须同时提供")
	}
	if options.MemoryDirectory != "" {
		options.MemoryDirectory, err = realPath(options.MemoryDirectory)
		if err != nil {
			return nil, err
		}
	}
	h := &Host{options: options, service: session.NewService(options.Store), runtimes: map[string]*Runtime{}, closed: make(chan struct{}), workspace: map[string]fileClaim{}}
	h.observer = newObserver(options.OnEvent)
	// Manager 自己汇总连接失败；保留成功服务器，不探测业务工具。
	if options.MCP != nil && !options.MCPShared {
		if err := options.MCP.Start(ctx); err != nil {
			h.emit("", Event{Diagnostic: err})
		}
	}
	return h, nil
}

func (h *Host) Create(ctx context.Context, title string) (sc.SessionInfo, error) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return sc.SessionInfo{}, ErrClosed
	}
	h.opening.Add(1)
	h.mu.Unlock()
	defer h.opening.Done()
	return h.service.CreateSessionWithModel(ctx, title, h.options.ProjectDir, sc.ModelSelection{Ref: h.options.ModelRef})
}

// Open 装配宿主选定的主会话执行实例，不是只读历史查询，也不恢复旧 Run。
// 子任务由父 Controller 持有并 Send；宿主不得把历史 child session ID 当作主会话传入。
// 当前 Session 元数据没有反向角色查询，本入口不能自行验证任意历史 ID 的角色。
func (h *Host) Open(ctx context.Context, id string) (*Runtime, error) {
	return h.open(ctx, id, "", nil)
}

func (h *Host) open(ctx context.Context, id, role string, scope []string) (_ *Runtime, err error) {
	h.mu.Lock()
	if h.catalogChanging {
		h.mu.Unlock()
		return nil, ErrBusy
	}
	if h.closing {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	if r, ok := h.runtimes[id]; ok {
		h.mu.Unlock()
		if r == nil {
			return nil, ErrBusy
		}
		return r, nil
	}
	h.runtimes[id] = nil
	h.opening.Add(1)
	h.mu.Unlock()
	defer h.opening.Done()
	defer func() {
		if err != nil {
			h.mu.Lock()
			delete(h.runtimes, id)
			h.mu.Unlock()
		}
	}()
	r, err := h.initialize(ctx, id, role, scope)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	closing := h.closing
	if !closing {
		h.runtimes[id] = r
	}
	h.mu.Unlock()
	if closing {
		_ = r.Close(context.Background())
		return nil, ErrClosed
	}
	return r, nil
}

func (h *Host) Opened() []*Runtime {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*Runtime
	for _, r := range h.runtimes {
		if r != nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (h *Host) Close(ctx context.Context) error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closing = true
		h.mu.Unlock()
		go func() {
			h.opening.Wait()
			var failures []error
			rs := h.Opened()
			for _, r := range rs {
				r.beginClose()
			}
			for _, r := range rs {
				failures = append(failures, r.Close(context.Background()))
			}
			if h.options.CloseMCP && h.options.MCP != nil {
				failures = append(failures, h.options.MCP.Close())
			}
			if h.options.CloseStore {
				failures = append(failures, h.options.Store.Close())
			}
			h.mu.Lock()
			h.closeErr = errors.Join(failures...)
			h.mu.Unlock()
			h.observer.stop()
			close(h.closed)
		}()
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.closed:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.closeErr
	}
}

func (h *Host) emit(id string, e Event) {
	h.observer.publish(id, e)
}
