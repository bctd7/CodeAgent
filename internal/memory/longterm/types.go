// Package longterm 提供独立的 Markdown 长期记忆，不负责会话监听或模型选择。
// 调用期间输入、选项及流快照共享只读；Go 不强制冻结，也不保证并发修改安全。
package longterm

import (
	"context"
	"errors"

	"CodeAgent/internal/llm"
)

var (
	ErrNoModel       = errors.New("longterm: no model callback configured")
	ErrBudget        = errors.New("longterm: consolidation budget exceeded")
	ErrBatchConflict = errors.New("longterm: batch ID reused with different sources or cursor")
	ErrLegacyBatch   = errors.New("longterm: legacy batch digest requires explicit migration")
	ErrCommitPending = errors.New("longterm: published commit requires recovery")
)

// StreamFunc 必须合作响应 context 取消；本包不会发起默认或隐式模型请求。
type StreamFunc func(context.Context, llm.Request) (llm.StreamReader, error)

type Limits struct {
	Turns       int
	ToolCalls   int
	InputBytes  int
	OutputBytes int
	FileBytes   int
	Files       int
	IndexLines  int
	IndexRunes  int
}

// Options 中的模型及传输选项仅存在内存中，不写入任何记忆文件。
type Options struct {
	Model          llm.Model
	RequestOptions llm.RequestOptions
	Stream         StreamFunc
	Limits         Limits
}

// Source 的 ID 和消息由可信调用方提供。工具消息代表已发生的执行结果；
// 助手的计划、工具调用提议和推测均不能视为执行证明。
type Source struct {
	ID      string
	Message llm.Message
}

// Batch.ID 在同一记忆目录内唯一。Cursor 由调用方定义，只在成功或无变更时推进。
type Batch struct {
	ID      string
	Cursor  string
	Sources []Source
}

type Outcome struct {
	Changed   bool
	Duplicate bool
	Cursor    string
	Turns     int
	ToolCalls int
	Usage     llm.Usage
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

// Entry 是可信调用方直接管理正文时使用的数据；Sources 不由模型自行声明。
type Entry struct {
	Path    string
	Summary string
	Type    string
	Content string
	Sources []string
}
