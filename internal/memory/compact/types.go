// Package compact 提供 Pi 式上下文压缩；触发、持久化和结果应用由调用方负责。
// 输入、计划和回调参数在整个使用期间共享只读，不提供并发修改保护。
package compact

import (
	"context"
	"errors"
	"fmt"

	"CodeAgent/internal/llm"
)

// Source 是调用方已选定的模型可见消息，按当前历史路径排列。
type Source struct {
	ID      string
	Message llm.Message
}

type Settings struct {
	ReserveTokens    int // 影响摘要输出上限；自动触发阈值由调用方使用。
	KeepRecentTokens int // 连续尾部的近似保留目标，不是硬下限。
}

// DefaultSettings 显式提供默认值，不覆盖调用方设置的零值。
func DefaultSettings() Settings {
	return Settings{ReserveTokens: 16384, KeepRecentTokens: 20000}
}

type Input struct {
	Sources         []Source // 上次保留的原文尾部，加本次新增消息。
	PreviousSummary string   // 已覆盖的更早历史，不应再次放入 Sources。
	Settings        Settings
}

// Plan 由 Prepare 生成；各区段共享输入引用，生成后须保持只读。
// History、TurnPrefix、Retained 按顺序完整划分本次 Sources。
type Plan struct {
	prepared              bool
	Ready                 bool // 有可总结前缀，与自动触发阈值无关。
	PreviousSummary       string
	History               []Source
	TurnPrefix            []Source
	Retained              []Source
	IsSplitTurn           bool
	FirstKeptEntryID      string
	FirstKeptIndex        int
	EstimatedTokensBefore int // 仅旧摘要和消息的近似值，不含系统提示及工具定义。
	Settings              Settings
}

// StreamFunc 必须响应 ctx；返回的 reader.Close 必须解除 Result 的阻塞。
type StreamFunc func(context.Context, llm.Request) (llm.StreamReader, error)

type Options struct {
	Model          llm.Model
	RequestOptions llm.RequestOptions // MaxTokens 为额外输出上限；0 使用 Pi 派生上限。
	Stream         StreamFunc
}

// Result 仅在 Summarize 成功时填充 Summary 和保留边界。
// 失败仍可携带已产生的 Usage；调用方须检查 error 和 Compacted。
type Result struct {
	Compacted             bool
	Summary               string
	FirstKeptEntryID      string
	FirstKeptIndex        int
	EstimatedTokensBefore int
	Usage                 llm.Usage
}

type ErrorKind string

const (
	InvalidInput  ErrorKind = "invalid_input"
	InputTooLarge ErrorKind = "input_too_large"
	ModelFailed   ErrorKind = "model_failed"
	Rejected      ErrorKind = "summary_rejected"
)

type Error struct {
	Kind ErrorKind
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("compact: %s: %v", e.Kind, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

func failure(kind ErrorKind, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &Error{Kind: kind, Err: err}
}
