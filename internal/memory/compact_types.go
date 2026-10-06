package memory

import (
	"fmt"

	"CodeAgent/internal/llm"
)

// TokenEstimator 为指定模型估算传入的完整上下文，必须确定、非负并计入消息封装开销。
// 后续对话的系统提示词和工具定义开销由下方预算字段单独提供。
// nil 时只累加模型可见内容的 UTF-8 字节数和封装开销，作为保守的文本近似；
// 这不是分词器，也不宣称能准确估算中文 token。保留尾部中的图片必须注入估算器，
// 能估算图片不代表能总结图片；待总结前缀中的可见图片会在模型调用前被拒绝。
type TokenEstimator func(llm.Model, llm.RequestContext) (int, error)

type CompactionBudget struct {
	SystemTokens        int
	ToolsTokens         int
	ReserveOutputTokens int // 后续对话的输出预留，与摘要输出上限分开。
	SafetyTokens        int
	KeepRecentTokens    int // 至少保留的近期尾部预算，向前回退到安全边界。
}

// CompactionInput 是调用方明确选择的当前原文投影，Sources 按模型组装顺序排列，
// ID 必须稳定且唯一，不要求是连续数据库区间。连续压缩时，将上次 Summary 作为
// PreviousSummary，按上次 KeptSourceIDs 回读原文并追加新消息作为 Sources。
// 保留的最新用户来源可能已作为背景纳入旧摘要，仍需原样留在当前 Sources 中。
// RequestOptions.MaxTokens 明确限制单次或最终摘要的输出，Model 必须提供已知的 ContextWindow。
// Stream 沿用长期记忆 API 的合作取消契约，所有阶段共享 ctx 及其截止时间。
// 本包不使用隐式模型、不重试、不存储、不执行工具。超大输入最多进行四次分块总结
// 和一次合并，并预先为各块输出分配较小的上限。
type CompactionInput struct {
	Sources         []Source
	PreviousSummary string
	Model           llm.Model
	RequestOptions  llm.RequestOptions
	Stream          StreamFunc
	Budget          CompactionBudget
	Estimate        TokenEstimator
	MaxChunks       int // 0 默认取 4，可明确设置为 1..4；后备路径另加一次合并调用。
}

// CompactionResult 仅是候选，不会自动安装检查点。只有 Compacted 为 true 且 error 为 nil
// 时才填充边界字段、KeptSourceIDs 及 Summary。TokensBefore/After 包含 SystemTokens 和 ToolsTokens。
// 调用方必须按 KeptSourceIDs 回读原文组装保留消息，不能仅靠 FirstKeptIndex 截取尾部。
// 估算时将 Summary 作为一条用户文本消息放在这些保留消息前，锚点只计一次；调用方若使用其他
// 表示方式，必须在安装之前重新估算其实际请求。失败或无需压缩时不返回可安装的来源列表。
type CompactionResult struct {
	Compacted          bool
	Summary            string
	CoveredThroughID   string   // 本次归入摘要的前缀末端，包含该条；不表示删除范围。
	FirstKeptID        string   // 连续近期尾部的起点 ID，仅作索引辅助。
	FirstKeptIndex     int      // 连续近期尾部在当前 Sources 中的起点，仅作索引辅助。
	KeptSourceIDs      []string // 按组装顺序：可选的最新用户锚点，再接完整连续尾部；来源不重复。
	TokensBefore       int
	TokensAfter        int
	InputLimit         int              // Model.ContextWindow 减输出预留和安全余量。
	SummaryInputTokens int              // 未分块的摘要请求估算，启用后备时仍保留该值。
	Usage              llm.Usage        // 摘要调用的实际累计用量，不用于验证 TokensAfter。
	Calls              []CompactionCall // 各次调用快照，仅可用于适用快照的用量校准。
}

type CompactionCall struct {
	Stage         string // "single"、"chunk" 或 "merge"，按实际调用顺序排列。
	FirstSourceID string
	LastSourceID  string
	InputTokens   int
	OutputLimit   int
	Usage         llm.Usage
}

type CompactionFailure string

const (
	CompactionInvalidInput   CompactionFailure = "invalid_input"
	CompactionCannotCompact  CompactionFailure = "cannot_compact"
	CompactionInputTooLarge  CompactionFailure = "summary_input_too_large"
	CompactionEstimateFailed CompactionFailure = "estimate_failed"
	CompactionModelFailed    CompactionFailure = "model_failed"
	CompactionRejected       CompactionFailure = "summary_rejected"
)

// CompactionError 支持 errors.As；取消时返回 context 自身的错误。
// 失败结果可以携带估算和用量，但不会携带可安装的候选摘要。
type CompactionError struct {
	Kind  CompactionFailure
	Cause error
}

func (e *CompactionError) Error() string {
	return fmt.Sprintf("memory: compaction %s: %v", e.Kind, e.Cause)
}

func (e *CompactionError) Unwrap() error { return e.Cause }
