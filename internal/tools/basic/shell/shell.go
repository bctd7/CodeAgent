package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/tools/basic/common"
)

const (
	progressInterval = 100 * time.Millisecond
	pipeWaitDelay    = 2 * time.Second
)

var shellSchema = []byte(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "Shell command to execute"
    },
    "timeout": {
      "type": "number",
      "description": "Timeout in seconds (optional, no default timeout)"
    }
  },
  "required": ["command"]
}`)

type shellError struct {
	message string
	cause   error
}

func (e *shellError) Error() string {
	return e.message
}

func (e *shellError) Unwrap() error {
	return e.cause
}

type OutputSnapshot struct {
	Content        string
	Truncation     common.TruncateResult
	FullOutputPath string
	LastLineBytes  int
}

type OutputAccumulator struct {
	mu sync.Mutex

	maxLines        int
	maxBytes        int
	maxRollingBytes int
	tempFilePrefix  string

	leftover  []byte
	rawChunks [][]byte

	tailText          string
	tailBytes         int
	totalRawBytes     int
	totalDecodedBytes int
	totalLines        int
	completedLines    int
	currentLineBytes  int
	hasOpenLine       bool
	finished          bool

	tempFile     *os.File
	tempFilePath string
	tempFileErr  error
}

type shellOutputWriter struct {
	acc     *OutputAccumulator
	tracker *progressTracker
}

func (w *shellOutputWriter) Write(p []byte) (int, error) {
	w.acc.Append(p)
	if w.tracker != nil {
		w.tracker.markDirty()
	}
	return len(p), nil
}

var _ io.Writer = (*shellOutputWriter)(nil)

type progressTracker struct {
	mu         sync.Mutex
	acc        *OutputAccumulator
	onProgress func(llm.ContentBlock)
	interval   time.Duration

	dirty  bool
	stopCh chan struct{}
	doneCh chan struct{}
}

// New 创建当前操作系统平台默认的 Shell 工具（Windows 为 powershell，Unix 为 bash）。
func New(absWd string) agent.Tool {
	shellName, _, _, _, _ := resolvePlatformShell()
	if shellName == "powershell" {
		return NewPowerShell(absWd)
	}
	return NewBash(absWd)
}

// NewPowerShell 创建 PowerShell 工具。
func NewPowerShell(absWd string) agent.Tool {
	return createShellTool(absWd, "powershell", "Execute a PowerShell command in the workspace directory.")
}

// NewBash 创建 Bash 工具。
func NewBash(absWd string) agent.Tool {
	return createShellTool(absWd, "bash", "Execute a bash command in the workspace directory.")
}

func createShellTool(absWd, toolName, description string) agent.Tool {
	return agent.Tool{
		Tool: llm.Tool{
			Name:        toolName,
			Description: description,
			Schema:      shellSchema,
		},
		ParallelSafe: false,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			if err := ctx.Err(); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return nil, &shellError{message: "Command context deadline exceeded", cause: err}
				}
				return nil, &shellError{message: "Command aborted", cause: err}
			}

			command, timeoutSec, hasTimeout, err := parseShellArgs(args)
			if err != nil {
				return nil, err
			}

			fi, err := os.Stat(absWd)
			if err != nil || !fi.IsDir() {
				return nil, fmt.Errorf("工作目录不存在: %s", absWd)
			}

			shellPath, fullArgs, isPowerShell, err := prepareShellCommand(toolName, command)
			if err != nil {
				return nil, err
			}

			var (
				runCtx context.Context
				cancel context.CancelFunc
			)
			if hasTimeout {
				runCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
			} else {
				runCtx, cancel = context.WithCancel(ctx)
			}
			defer cancel()

			cmd := exec.CommandContext(runCtx, shellPath, fullArgs...)
			cmd.Dir = absWd
			cmd.Env = os.Environ()
			if isPowerShell {
				cmd.Env = append(cmd.Env, "PYTHONIOENCODING=utf-8")
			}
			setSysProcAttr(cmd)

			cmd.Cancel = func() error {
				if cmd.Process != nil {
					return killProcessTree(cmd.Process.Pid)
				}
				return nil
			}
			cmd.WaitDelay = pipeWaitDelay

			prefix := "codeagent-" + toolName + "-"
			acc := newOutputAccumulator(prefix, common.DefaultMaxLines, common.DefaultMaxBytes)
			tracker := newProgressTracker(acc, onProgress, progressInterval)

			writer := &shellOutputWriter{acc: acc, tracker: tracker}
			cmd.Stdout = writer
			cmd.Stderr = writer

			if err := cmd.Start(); err != nil {
				return nil, fmt.Errorf("启动进程失败: %w", err)
			}

			tracker.start()

			waitErr := cmd.Wait()

			parentErr := ctx.Err()
			runErr := runCtx.Err()

			// 执行已完成，及时停止超时计时器
			cancel()
			tracker.stop()
			acc.Finish()
			snapshot := acc.Snapshot(true)
			acc.CloseTempFile()

			// 优先检查父 context 的 ctx.Err()，同时处理 context.Canceled 和 context.DeadlineExceeded
			if parentErr != nil {
				formatted := formatShellOutput(snapshot, acc, "")
				if errors.Is(parentErr, context.DeadlineExceeded) {
					msg := appendStatus(formatted, "Command context deadline exceeded")
					return nil, &shellError{message: msg, cause: parentErr}
				}
				msg := appendStatus(formatted, "Command aborted")
				return nil, &shellError{message: msg, cause: parentErr}
			}

			// 父 context 未结束时，再判断工具自身超时
			if hasTimeout && errors.Is(runErr, context.DeadlineExceeded) {
				formatted := formatShellOutput(snapshot, acc, "")
				msg := appendStatus(formatted, fmt.Sprintf("Command timed out after %v seconds", timeoutSec))
				return nil, &shellError{message: msg, cause: runErr}
			}

			if waitErr != nil {
				formatted := formatShellOutput(snapshot, acc, "")
				if exitErr, ok := waitErr.(*exec.ExitError); ok {
					msg := appendStatus(formatted, fmt.Sprintf("Command exited with code %d", exitErr.ExitCode()))
					return nil, &shellError{message: msg, cause: waitErr}
				}
				msg := appendStatus(formatted, fmt.Sprintf("Command execution failed: %v", waitErr))
				return nil, &shellError{message: msg, cause: waitErr}
			}

			formatted := formatShellOutput(snapshot, acc, "(no output)")
			return []llm.ContentBlock{
				{
					Type: llm.TypeText,
					Text: formatted,
				},
			}, nil
		},
	}
}

func prepareShellCommand(toolName, command string) (shellPath string, fullArgs []string, isPowerShell bool, err error) {
	_, p, args, _, resolveErr := resolvePlatformShell()
	if resolveErr != nil {
		return "", nil, false, resolveErr
	}

	if toolName == "powershell" {
		isPowerShell = true
		shellPath = p
		// 确保输出 UTF-8、自身错误设为 Stop 并在退出时保留原生命令退出码
		wrappedCommand := fmt.Sprintf("try { [Console]::OutputEncoding=[System.Text.Encoding]::UTF8 } catch {}\n$ErrorActionPreference = 'Stop'\n%s\nif ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }", command)
		fullArgs = append(args, wrappedCommand)
		return shellPath, fullArgs, isPowerShell, nil
	}

	isPowerShell = false
	shellPath = p
	fullArgs = append(args, command)
	return shellPath, fullArgs, isPowerShell, nil
}

func parseShellArgs(args map[string]any) (command string, timeoutSec float64, hasTimeout bool, err error) {
	rawCmd, ok := args["command"]
	if !ok {
		return "", 0, false, errors.New("参数 command 必须是字符串")
	}
	command, ok = rawCmd.(string)
	if !ok {
		return "", 0, false, errors.New("参数 command 必须是字符串")
	}

	rawTimeout, exists := args["timeout"]
	if !exists || rawTimeout == nil {
		return command, 0, false, nil
	}

	switch v := rawTimeout.(type) {
	case float64:
		timeoutSec = v
	case float32:
		timeoutSec = float64(v)
	case int:
		timeoutSec = float64(v)
	case int64:
		timeoutSec = float64(v)
	case int32:
		timeoutSec = float64(v)
	case json.Number:
		f, numErr := v.Float64()
		if numErr != nil {
			return "", 0, false, errors.New("timeout 必须为正数（秒）")
		}
		timeoutSec = f
	default:
		return "", 0, false, errors.New("timeout 必须为正数（秒）")
	}

	if timeoutSec <= 0 || math.IsNaN(timeoutSec) || math.IsInf(timeoutSec, 0) {
		return "", 0, false, errors.New("timeout 必须为正数（秒）")
	}

	return command, timeoutSec, true, nil
}

func appendStatus(text, status string) string {
	if text != "" {
		return text + "\n\n" + status
	}
	return status
}

func formatShellOutput(snapshot OutputSnapshot, acc *OutputAccumulator, emptyText string) string {
	trunc := snapshot.Truncation
	text := snapshot.Content
	if text == "" {
		text = emptyText
	}
	if trunc.Truncated {
		startLine := trunc.TotalLines - trunc.OutputLines + 1
		endLine := trunc.TotalLines
		logPath := snapshot.FullOutputPath
		if acc.tempFileErr != nil {
			logPath = fmt.Sprintf("%s (注意: 临时日志保存失败: %v，内容可能不完整)", logPath, acc.tempFileErr)
		}
		if trunc.LastLinePartial {
			lastLineSize := common.FormatSize(snapshot.LastLineBytes)
			text += fmt.Sprintf("\n\n[Showing last %s of line %d (line is %s). Full output: %s]",
				common.FormatSize(trunc.OutputBytes), endLine, lastLineSize, logPath)
		} else if trunc.TruncatedBy == "lines" {
			text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Full output: %s]",
				startLine, endLine, trunc.TotalLines, logPath)
		} else {
			text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Full output: %s]",
				startLine, endLine, trunc.TotalLines, common.FormatSize(acc.maxBytes), logPath)
		}
	}
	return text
}

func splitIncompleteUTF8(b []byte) (valid []byte, leftover []byte) {
	if len(b) == 0 {
		return nil, nil
	}
	n := len(b)
	for i := n - 1; i >= 0 && i >= n-4; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return b, nil
			}
			return b[:i], b[i:]
		}
	}
	return b, nil
}

func newOutputAccumulator(prefix string, maxLines, maxBytes int) *OutputAccumulator {
	if maxLines <= 0 {
		maxLines = common.DefaultMaxLines
	}
	if maxBytes <= 0 {
		maxBytes = common.DefaultMaxBytes
	}
	maxRollingBytes := maxBytes * 2
	if maxRollingBytes < 1 {
		maxRollingBytes = 1
	}

	return &OutputAccumulator{
		maxLines:        maxLines,
		maxBytes:        maxBytes,
		maxRollingBytes: maxRollingBytes,
		tempFilePrefix:  prefix,
	}
}

func (acc *OutputAccumulator) Append(data []byte) {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	if acc.finished {
		return
	}

	acc.totalRawBytes += len(data)

	// 1. 临时文件写入原始数据字节
	if acc.tempFile != nil || acc.shouldUseTempFileLocked() {
		acc.ensureTempFileLocked()
		if acc.tempFile != nil && acc.tempFileErr == nil {
			if _, err := acc.tempFile.Write(data); err != nil {
				acc.tempFileErr = err
			}
		}
	} else if len(data) > 0 {
		chunk := make([]byte, len(data))
		copy(chunk, data)
		acc.rawChunks = append(acc.rawChunks, chunk)
	}

	// 2. 解码 UTF-8 文本并维护滚动展示窗口
	combined := append(acc.leftover, data...)
	validBytes, leftover := splitIncompleteUTF8(combined)
	acc.leftover = leftover
	if len(validBytes) > 0 {
		acc.appendDecodedTextLocked(string(validBytes))
	}
}

func (acc *OutputAccumulator) appendDecodedTextLocked(text string) {
	if len(text) == 0 {
		return
	}
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}
	bytesCount := len(text)
	acc.totalDecodedBytes += bytesCount
	acc.tailText += text
	acc.tailBytes += bytesCount

	if acc.tailBytes > acc.maxRollingBytes*2 {
		acc.trimTailLocked()
	}

	newlines := 0
	lastNewline := -1
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			newlines++
			lastNewline = i
		}
	}

	if newlines == 0 {
		acc.currentLineBytes += bytesCount
		acc.hasOpenLine = true
	} else {
		acc.completedLines += newlines
		tail := text[lastNewline+1:]
		acc.currentLineBytes = len(tail)
		acc.hasOpenLine = len(tail) > 0
	}

	acc.totalLines = acc.completedLines
	if acc.hasOpenLine {
		acc.totalLines++
	}
}

func (acc *OutputAccumulator) trimTailLocked() {
	b := []byte(acc.tailText)
	if len(b) <= acc.maxRollingBytes {
		acc.tailBytes = len(b)
		return
	}

	start := len(b) - acc.maxRollingBytes
	for start < len(b) && !utf8.RuneStart(b[start]) {
		start++
	}

	tailStr := string(b[start:])
	// 若换行符不是最后一个字符，从首个换行符后保留完整行；
	// 若换行符恰好位于末尾（单行超长），保留整段尾部文本，避免切成空字符串。
	if idx := strings.IndexByte(tailStr, '\n'); idx != -1 && idx < len(tailStr)-1 {
		tailStr = tailStr[idx+1:]
	}
	acc.tailText = tailStr
	acc.tailBytes = len(tailStr)
}

func (acc *OutputAccumulator) shouldUseTempFileLocked() bool {
	return acc.totalRawBytes > acc.maxBytes || acc.totalDecodedBytes > acc.maxBytes || acc.totalLines > acc.maxLines
}

func (acc *OutputAccumulator) ensureTempFileLocked() {
	if acc.tempFile != nil || acc.tempFileErr != nil {
		return
	}

	f, err := os.CreateTemp("", acc.tempFilePrefix+"*.log")
	if err != nil {
		acc.tempFileErr = err
		return
	}

	absPath, err := filepath.Abs(f.Name())
	if err == nil {
		acc.tempFilePath = absPath
	} else {
		acc.tempFilePath = f.Name()
	}
	acc.tempFile = f

	for _, chunk := range acc.rawChunks {
		if _, err := f.Write(chunk); err != nil {
			acc.tempFileErr = err
			break
		}
	}
	acc.rawChunks = nil
}

func (acc *OutputAccumulator) Finish() {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	if acc.finished {
		return
	}
	acc.finished = true

	if len(acc.leftover) > 0 {
		acc.appendDecodedTextLocked(string(acc.leftover))
		acc.leftover = nil
	}

	if acc.shouldUseTempFileLocked() {
		acc.ensureTempFileLocked()
	}
}

func (acc *OutputAccumulator) CloseTempFile() {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	if acc.tempFile != nil {
		if err := acc.tempFile.Sync(); err != nil && acc.tempFileErr == nil {
			acc.tempFileErr = err
		}
		if err := acc.tempFile.Close(); err != nil && acc.tempFileErr == nil {
			acc.tempFileErr = err
		}
		acc.tempFile = nil
	}
}

func (acc *OutputAccumulator) Snapshot(persistIfTruncated bool) OutputSnapshot {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	truncated := acc.totalLines > acc.maxLines || acc.totalDecodedBytes > acc.maxBytes || acc.totalRawBytes > acc.maxBytes
	if persistIfTruncated && truncated {
		acc.ensureTempFileLocked()
	}

	tailTruncation := truncateTail(acc.tailText, common.TruncateOptions{
		MaxLines: acc.maxLines,
		MaxBytes: acc.maxBytes,
	})

	truncation := tailTruncation
	if truncated {
		truncation.Truncated = true
		if truncation.TruncatedBy == "" {
			if acc.totalDecodedBytes > acc.maxBytes || acc.totalRawBytes > acc.maxBytes {
				truncation.TruncatedBy = "bytes"
			} else {
				truncation.TruncatedBy = "lines"
			}
		}
		truncation.TotalLines = acc.totalLines
		truncation.TotalBytes = acc.totalDecodedBytes
		if truncation.TotalBytes < acc.totalRawBytes {
			truncation.TotalBytes = acc.totalRawBytes
		}
	}

	return OutputSnapshot{
		Content:        truncation.Content,
		Truncation:     truncation,
		FullOutputPath: acc.tempFilePath,
		LastLineBytes:  acc.currentLineBytes,
	}
}

func newProgressTracker(acc *OutputAccumulator, onProgress func(llm.ContentBlock), interval time.Duration) *progressTracker {
	return &progressTracker{
		acc:        acc,
		onProgress: onProgress,
		interval:   interval,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

func (p *progressTracker) start() {
	if p.onProgress == nil {
		return
	}
	go func() {
		defer close(p.doneCh)
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		for {
			select {
			case <-p.stopCh:
				return
			case <-ticker.C:
				p.mu.Lock()
				if p.dirty {
					p.dirty = false
					snapshot := p.acc.Snapshot(true)
					p.mu.Unlock()

					p.onProgress(llm.ContentBlock{
						Type: llm.TypeText,
						Text: snapshot.Content,
					})
				} else {
					p.mu.Unlock()
				}
			}
		}
	}()
}

func (p *progressTracker) markDirty() {
	if p.onProgress == nil {
		return
	}
	p.mu.Lock()
	p.dirty = true
	p.mu.Unlock()
}

func (p *progressTracker) stop() {
	if p.onProgress == nil {
		return
	}
	close(p.stopCh)
	<-p.doneCh
}

// truncateStringToBytesFromEnd 从末尾截取最多 maxBytes 字节，
// 且保证在合法的 UTF-8 字符边界起始，避免截断损坏多字节字符。
func truncateStringToBytesFromEnd(str string, maxBytes int) string {
	b := []byte(str)
	if len(b) <= maxBytes {
		return str
	}
	start := len(b) - maxBytes
	for start < len(b) && !utf8.RuneStart(b[start]) {
		start++
	}
	return string(b[start:])
}

// truncateTail 保留文本末尾的内容，适用于 Shell 命令输出等查看尾部日志的场景。
// 统一默认文本上限为 2000 行、50 KiB，先达到者生效。
// 仅保留完整行，除非末行单行即超过字节上限，此时截取末尾部分并置 LastLinePartial 为 true。
func truncateTail(content string, opts common.TruncateOptions) common.TruncateResult {
	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = common.DefaultMaxLines
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = common.DefaultMaxBytes
	}

	totalBytes := len(content)
	lines := common.SplitLinesForCounting(content)
	totalLines := len(lines)

	if totalLines <= maxLines && totalBytes <= maxBytes {
		return common.TruncateResult{
			Content:     content,
			Truncated:   false,
			TruncatedBy: "",
			TotalLines:  totalLines,
			TotalBytes:  totalBytes,
			OutputLines: totalLines,
			OutputBytes: totalBytes,
		}
	}

	var collected []string
	outputBytesCount := 0
	truncatedBy := "lines"
	lastLinePartial := false

	for i := len(lines) - 1; i >= 0 && len(collected) < maxLines; i-- {
		line := lines[i]
		lineBytes := len(line)
		if len(collected) > 0 {
			lineBytes++
		}

		if outputBytesCount+lineBytes > maxBytes {
			truncatedBy = "bytes"
			if len(collected) == 0 {
				truncatedLine := truncateStringToBytesFromEnd(line, maxBytes)
				collected = append(collected, truncatedLine)
				outputBytesCount = len(truncatedLine)
				lastLinePartial = true
			}
			break
		}

		collected = append(collected, line)
		outputBytesCount += lineBytes
	}

	if len(collected) >= maxLines && outputBytesCount <= maxBytes {
		truncatedBy = "lines"
	}

	// 逆序反转得到正常顺序，避免每次向切片头部插入
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}

	outputContent := strings.Join(collected, "\n")
	return common.TruncateResult{
		Content:         outputContent,
		Truncated:       true,
		TruncatedBy:     truncatedBy,
		TotalLines:      totalLines,
		TotalBytes:      totalBytes,
		OutputLines:     len(collected),
		OutputBytes:     len(outputContent),
		LastLinePartial: lastLinePartial,
	}
}
