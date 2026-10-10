// 本文件负责命令准备、有界执行、解释器适配及进程回收。
package hooks

import (
	hookprocess "CodeAgent/internal/hooks/process"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// RunnerOptions 设置输入与输出字节上限，以及可选的解释器路径。
type RunnerOptions struct {
	// 字节上限为零时采用默认值；超限输出会触发进程树终止。
	InputBytes, StdoutBytes, StderrBytes int
	// 显式指定解释器路径时，跳过自动查找。
	PowerShellPath, BashPath, SHPath string
}

// Command 描述一次进程调用，输入通过标准输入传入。
type Command struct {
	Command string
	// nil 表示通过 shell 执行；非 nil 的空切片表示直接执行命令。
	Args  []string
	Shell string
	// Directory 必须是绝对路径；Input 经标准输入发送给子进程。
	Directory string
	Input     []byte
	// Environment 覆盖继承环境；UnsetEnvironment 先移除指定变量。
	Environment      []string
	UnsetEnvironment []string
	// Timeout 必须为正，与调用方 context 共同约束执行时间。
	Timeout time.Duration
}

// CommandRunner 负责受限执行、取消与进程树清理。
type CommandRunner struct{ options RunnerOptions }

// ProcessStatus 区分正常退出、启动失败、超时、取消和输出超限等情况。
type ProcessStatus string

const (
	ProcessExited         ProcessStatus = "exited"
	ProcessStartFailed    ProcessStatus = "start_failed"
	ProcessTimedOut       ProcessStatus = "timed_out"
	ProcessCanceled       ProcessStatus = "canceled"
	ProcessOutputExceeded ProcessStatus = "output_exceeded"
	ProcessFailed         ProcessStatus = "process_failed"
)

// Execution 保存原始进程事实，输出内容仍需由协议层解释。
type Execution struct {
	Status     ProcessStatus
	Executable string
	// ExitCode 在没有可用退出码时为 -1，须结合 Status 和 Error 判断。
	ExitCode       int
	Stdout, Stderr []byte
	Duration       time.Duration
	Error          error
}

// NewCommandRunner 为零值上限填入默认值，并拒绝非正上限。
func NewCommandRunner(options RunnerOptions) (*CommandRunner, error) {
	if options.InputBytes == 0 {
		options.InputBytes = 4 << 20
	}
	if options.StdoutBytes == 0 {
		options.StdoutBytes = 1 << 20
	}
	if options.StderrBytes == 0 {
		options.StderrBytes = 256 << 10
	}
	if options.InputBytes < 1 || options.StdoutBytes < 1 || options.StderrBytes < 1 {
		return nil, fmt.Errorf("%w: runner limits must be positive", ErrConfiguration)
	}
	return &CommandRunner{options}, nil
}

// buildCommand 结合事件、插件路径和配置，生成待执行命令及环境覆盖。
func buildCommand(h compiledHook, common CommonInput, input []byte) (Command, error) {
	projectDir := common.ProjectDir
	if projectDir == "" {
		projectDir = common.CWD
	}
	variables := map[string]string{"CLAUDE_PROJECT_DIR": projectDir}
	if h.source.PluginRoot != "" {
		variables["CLAUDE_PLUGIN_ROOT"] = h.source.PluginRoot
	}
	if h.source.PluginData != "" {
		variables["CLAUDE_PLUGIN_DATA"] = h.source.PluginData
	}
	c := Command{Command: h.handler.Command, Args: h.handler.Args, Shell: h.handler.Shell, Directory: common.CWD, Input: input, Timeout: h.timeout}
	command, err := expandPathPlaceholders(c.Command, variables)
	if err != nil {
		return c, err
	}
	if c.Args != nil {
		c.Command = command
		c.Args = make([]string, len(h.handler.Args))
		for i, arg := range h.handler.Args {
			c.Args[i], err = expandPathPlaceholders(arg, variables)
			if err != nil {
				return c, err
			}
		}
	}
	// shell 模式保留命令原文，通过环境变量展开路径；
	// 直接执行模式使用参数字面值，不把参数作为 shell 源码。
	for _, name := range pathVariables {
		if value, ok := variables[name]; ok {
			c.Environment = append(c.Environment, name+"="+value)
		} else {
			c.UnsetEnvironment = append(c.UnsetEnvironment, name)
		}
	}
	return c, nil
}

// Run 只返回进程执行事实，JSON 和退出码 2 的策略含义由协议层解释。
// 返回前回收整个子进程树，正常退出也执行清理。
func (r *CommandRunner) Run(ctx context.Context, command Command) (result Execution) {
	started := time.Now()
	result.ExitCode = -1
	result.Status = ProcessStartFailed
	defer func() { result.Duration = time.Since(started) }()
	if ctx == nil {
		result.Error = fmt.Errorf("%w: nil context", ErrInput)
		return
	}
	if err := ctx.Err(); err != nil {
		result.Status = ProcessCanceled
		result.Error = err
		return
	}
	if len(command.Input) > r.options.InputBytes || command.Timeout <= 0 || !filepath.IsAbs(command.Directory) {
		result.Error = fmt.Errorf("%w: input limit, timeout or absolute cwd", ErrInput)
		return
	}
	path, args, err := r.resolve(command)
	if err != nil {
		result.Error = err
		return
	}
	result.Executable = path
	runCtx, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()
	cmd := exec.Command(path, args...)
	cmd.Dir = command.Directory
	cmd.Env = mergeEnvironment(filterEnvironment(os.Environ(), command.UnsetEnvironment), command.Environment)
	cmd.Stdin = bytes.NewReader(command.Input)
	signal := make(chan struct{}, 1)
	stdout := &limitedOutput{limit: r.options.StdoutBytes, signal: signal}
	stderr := &limitedOutput{limit: r.options.StderrBytes, signal: signal}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 500 * time.Millisecond
	tree, err := hookprocess.Start(cmd)
	if err != nil {
		result.Error = err
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr, cleanupErr error
	select {
	case waitErr = <-done:
		result.Status = ProcessExited
	case <-signal:
		result.Status = ProcessOutputExceeded
		cleanupErr = tree.Kill()
		waitErr = <-done
	case <-runCtx.Done():
		result.Status = ProcessTimedOut
		if ctx.Err() != nil {
			result.Status = ProcessCanceled
		}
		cleanupErr = tree.Kill()
		waitErr = <-done
	}
	// 关闭或终止进程树时，也回收父进程正常退出后遗留的后代进程。
	cleanupErr = errors.Join(cleanupErr, tree.Close())
	var outExceeded, errExceeded bool
	result.Stdout, outExceeded = stdout.snapshot()
	result.Stderr, errExceeded = stderr.snapshot()
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		result.Status = ProcessCanceled
		result.Error = errors.Join(ctx.Err(), cleanupErr)
		return
	}
	if outExceeded || errExceeded {
		result.Status = ProcessOutputExceeded
		result.Error = errors.Join(ErrOutputLimit, cleanupErr)
		return
	}
	if result.Status == ProcessTimedOut || runCtx.Err() != nil {
		result.Status = ProcessTimedOut
		result.Error = errors.Join(context.DeadlineExceeded, cleanupErr)
		return
	}
	if cleanupErr != nil {
		result.Status = ProcessFailed
		result.Error = cleanupErr
		return
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			result.Status = ProcessFailed
			result.Error = waitErr
		}
	}
	return
}

// resolve 区分直接执行和 shell 执行，并选择相应解释器。
func (r *CommandRunner) resolve(c Command) (string, []string, error) {
	if strings.TrimSpace(c.Command) == "" || strings.ContainsRune(c.Command, 0) {
		return "", nil, fmt.Errorf("%w: empty command or NUL", ErrInput)
	}
	if c.Args != nil {
		for _, a := range c.Args {
			if strings.ContainsRune(a, 0) {
				return "", nil, fmt.Errorf("%w: NUL in argument", ErrInput)
			}
		}
		name := c.Command
		if !filepath.IsAbs(name) && (strings.ContainsRune(name, filepath.Separator) || strings.ContainsRune(name, '/')) {
			name = filepath.Join(c.Directory, name)
		}
		path, err := exec.LookPath(name)
		if err != nil {
			return "", nil, err
		}
		if runtime.GOOS == "windows" {
			ext := strings.ToLower(filepath.Ext(path))
			if ext == ".cmd" || ext == ".bat" {
				return "", nil, fmt.Errorf("%w: Windows exec form requires a real executable", ErrUnsupported)
			}
		}
		return path, append([]string(nil), c.Args...), nil
	}
	shell := c.Shell
	if shell == "" {
		if runtime.GOOS == "windows" {
			if path := gitBash(r.options.BashPath); path != "" {
				return path, []string{"-c", c.Command}, nil
			}
			shell = "powershell"
		} else {
			path, err := lookup(r.options.SHPath, "sh")
			if err != nil {
				return "", nil, err
			}
			return path, []string{"-c", c.Command}, nil
		}
	}
	switch shell {
	case "powershell":
		path, err := lookup(r.options.PowerShellPath, "pwsh", "powershell")
		if err != nil {
			return "", nil, err
		}
		body := powerShellPlaceholders(c.Command)
		// 控制台使用 UTF-8 传输 JSON，尤其需要兼容 Windows PowerShell 5.1。
		script := "$utf8 = [System.Text.UTF8Encoding]::new($false); [Console]::InputEncoding = $utf8; [Console]::OutputEncoding = $utf8; $OutputEncoding = $utf8; " + body
		return path, []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script}, err
	case "bash":
		if runtime.GOOS == "windows" {
			path := gitBash(r.options.BashPath)
			if path == "" {
				return "", nil, fmt.Errorf("Git Bash unavailable")
			}
			return path, []string{"-c", c.Command}, nil
		}
		path, err := lookup(r.options.BashPath, "bash")
		return path, []string{"-c", c.Command}, err
	default:
		return "", nil, fmt.Errorf("%w: shell %q", ErrUnsupported, shell)
	}
}

func lookup(explicit string, names ...string) (string, error) {
	if explicit != "" {
		return exec.LookPath(explicit)
	}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("interpreter unavailable: %v", names)
}

func gitBash(explicit string) string {
	if explicit != "" {
		path, _ := exec.LookPath(explicit)
		return path
	}
	// 查找 Git Bash 时，排除 Windows 的 WSL bash 启动器。
	if path, err := exec.LookPath("bash.exe"); err == nil && !strings.Contains(strings.ToLower(filepath.Clean(path)), "windows\\system32") {
		return path
	}
	if git, err := exec.LookPath("git.exe"); err == nil {
		for _, rel := range []string{"../bin/bash.exe", "../../bin/bash.exe"} {
			path := filepath.Clean(filepath.Join(filepath.Dir(git), rel))
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path
			}
		}
	}
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs")} {
		if base != "" {
			path := filepath.Join(base, "Git", "bin", "bash.exe")
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path
			}
		}
	}
	return ""
}

// mergeEnvironment 在基础环境上应用覆盖值。
func mergeEnvironment(base, overrides []string) []string {
	values := map[string]int{}
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range append(append([]string(nil), base...), overrides...) {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if index, ok := values[key]; ok {
			result[index] = entry
		} else {
			values[key] = len(result)
			result = append(result, entry)
		}
	}
	return result
}

func filterEnvironment(base, names []string) []string {
	result := make([]string, 0, len(base))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		remove := false
		for _, name := range names {
			if key == name || (runtime.GOOS == "windows" && strings.EqualFold(key, name)) {
				remove = true
				break
			}
		}
		if !remove {
			result = append(result, entry)
		}
	}
	return result
}

var pathVariables = [...]string{"CLAUDE_PROJECT_DIR", "CLAUDE_PLUGIN_ROOT", "CLAUDE_PLUGIN_DATA"}

// expandPathPlaceholders 将受支持的路径占位符替换为参数字面值。
func expandPathPlaceholders(text string, variables map[string]string) (string, error) {
	if strings.Contains(text, "${user_config.") {
		return "", fmt.Errorf("%w: plugin user_config", ErrUnsupported)
	}
	pairs := make([]string, 0, 2*len(pathVariables))
	for _, name := range pathVariables {
		placeholder := "${" + name + "}"
		value, exists := variables[name]
		if strings.Contains(text, placeholder) && !exists {
			return "", fmt.Errorf("%w: missing %s", ErrUnsupported, name)
		}
		if exists {
			pairs = append(pairs, placeholder, value)
		}
	}
	// NewReplacer 只扫描一次原文，替换进去的路径不会再次被当成占位符。
	return strings.NewReplacer(pairs...).Replace(text), nil
}

// powerShellPlaceholders 将路径占位符转换为 PowerShell 环境变量引用。
func powerShellPlaceholders(text string) string {
	pairs := make([]string, 0, 2*len(pathVariables))
	for _, name := range pathVariables {
		pairs = append(pairs, "${"+name+"}", "${env:"+name+"}")
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// limitedOutput 并发收集有界输出，超限时通知执行器终止进程。
type limitedOutput struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	signal   chan struct{}
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	remaining := w.limit - w.buffer.Len()
	if n > remaining {
		w.exceeded = true
		select {
		case w.signal <- struct{}{}:
		default:
		}
		p = p[:max(remaining, 0)]
	}
	_, _ = w.buffer.Write(p)
	return n, nil
}

func (w *limitedOutput) snapshot() ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buffer.Bytes()), w.exceeded
}
