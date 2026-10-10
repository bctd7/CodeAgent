// Package longterm 提供独立的 Markdown 长期记忆，不负责会话监听或模型选择。
// 调用期间输入、选项及工具参数共享只读；Go 不强制冻结，也不保证并发修改安全。
package longterm

import (
	"context"
	"errors"
	"fmt"

	"CodeAgent/internal/llm"
	"CodeAgent/internal/memory/longterm/storage"
)

var (
	ErrNoExecutor    = errors.New("longterm: no consolidation executor configured")
	ErrBudget        = storage.ErrBudget
	ErrCommitPending = storage.ErrCommitPending
)

// ExecutionLimits 限制整理任务的执行资源，经 Task 交给执行方实时落实。
type ExecutionLimits struct {
	Turns       int // 实际尝试发起的模型调用上限，在每次调用前检查。
	ToolCalls   int // 工具调用上限，在启动每轮工具批次前检查剩余额度。
	InputBytes  int // 每轮 SystemPrompt、Messages、Tools 的 JSON 字节数，不含鉴权。
	OutputBytes int // 累计各轮最终 Content JSON 和工具结果正文 JSON 字节数，生成时检查增长。
}

// StorageLimits 是存储层的文件与索引预算。
type StorageLimits = storage.Limits

// Limits 将执行预算与存储预算分组；NewService 统一补齐两组的默认值。
type Limits struct {
	Execution ExecutionLimits
	Storage   StorageLimits
}

// ExecuteFunc 同步执行整个整理任务，合作响应取消，返回前结束全部模型、工具活动及资源收尾。
// 任意中间轮故障、工具错误、超限或收尾失败都返回 error，取消保留 errors.Is 语义。
// 仅使用本任务工具；不重入同目录 Consolidate，不保留 CallTool 给后台使用。
type ExecuteFunc func(context.Context, Task) (ExecutionResult, error)

// Options 配置整理执行器与资源预算；模型与传输配置由执行方持有。
type Options struct {
	Execute ExecuteFunc
	Limits  Limits
}

// Task 由 longterm 准备；执行方选择模型、配置传输，负责模型循环、结果配对和实时执行预算。
// 任务及其子字段共享只读，记忆工具按模型原始顺序串行调用。
type Task struct {
	SystemPrompt    string
	Messages        []llm.Message
	Tools           []llm.Tool
	ExecutionLimits ExecutionLimits
	CallTool        func(context.Context, llm.ContentBlock) (*llm.ToolResultMessage, error)
}

// ExecutionResult 表达整个任务的执行状态，尚不表示记忆已提交；零值不表示成功。
// 仅 error 为 nil 且 StopReason 为 StopStop 才可提交；失败也可返回已知统计。
type ExecutionResult struct {
	StopReason llm.StopReason
	Turns      int       // 实际尝试发起的模型调用数，正常完成须为正且不超过预算。
	Usage      llm.Usage // 全任务已报告用量之和，由执行方累计。
}

// Source 的 ID 和消息由可信调用方提供。工具消息代表已发生的执行结果；
// 助手的计划、工具调用提议和推测均不能视为执行证明。
type Source struct {
	ID      string
	Message llm.Message
}

// Batch.ID 在同一记忆目录内唯一，同一 ID 始终代表同一批来源；成功后只按 ID 去重。
type Batch struct {
	ID      string
	Sources []Source
}

// ConsolidationResult 是记忆整理的结果；Changed 表示提交后的主题或索引变化。
// 失败时 Execution 和 ToolCalls 仍保留已知统计，不表示记忆已保存。
type ConsolidationResult struct {
	Changed   bool
	Duplicate bool
	Execution ExecutionResult
	ToolCalls int // 本包实际工具回调入口次数，包括失败尝试。
}

// Document、Match、Entry 复用存储层的数据类型。
type Document = storage.Document
type Match = storage.Match
type Entry = storage.Entry

func defaultLimits(l Limits) (Limits, error) {
	defaults := []struct {
		value    *int
		fallback int
	}{
		{&l.Execution.Turns, 5}, {&l.Execution.ToolCalls, 24}, {&l.Execution.InputBytes, 256000}, {&l.Execution.OutputBytes, 256000},
		{&l.Storage.FileBytes, 64000}, {&l.Storage.Files, 256}, {&l.Storage.IndexLines, 200}, {&l.Storage.IndexRunes, 25000},
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
