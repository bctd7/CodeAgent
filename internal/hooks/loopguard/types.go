// Package loopguard 根据可信调用方提交的已完成工具事件，给出局部循环的启发式建议。
// 本包不执行工具、不读取模型思考、不取消任务，也不自动接入 agent。
package loopguard

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	MaxWindowSize      = 64
	MaxIdentifierBytes = 256
	MaxJSONBytes       = 64 * 1024
	MaxJSONDepth       = 64
)

// Config 的阈值以参与匹配的已完成事件数计量，停止建议阈值固定为警告阈值 + 1。
// 零值不是默认配置；请从 DefaultConfig 开始修改。
type Config struct {
	WindowSize           int
	RepeatThreshold      int
	AlternatingThreshold int
}

func DefaultConfig() Config {
	return Config{WindowSize: 20, RepeatThreshold: 3, AlternatingThreshold: 4}
}

func (c Config) validate() error {
	if c.WindowSize < 5 || c.WindowSize > MaxWindowSize {
		return fmt.Errorf("loopguard: WindowSize 必须为 5..%d", MaxWindowSize)
	}
	if c.RepeatThreshold < 2 || c.RepeatThreshold >= c.WindowSize {
		return errors.New("loopguard: RepeatThreshold 必须 >= 2 且小于 WindowSize")
	}
	if c.AlternatingThreshold < 4 || c.AlternatingThreshold%2 != 0 || c.AlternatingThreshold >= c.WindowSize {
		return errors.New("loopguard: AlternatingThreshold 必须是 >= 4 且小于 WindowSize 的偶数")
	}
	return nil
}

type Status string

const (
	Success   Status = "success"
	Failure   Status = "failure"
	Exception Status = "exception"
)

// ResultFormat 明确结果的比较方式；不猜测文本是否为 JSON。
type ResultFormat string

const (
	TextResult        ResultFormat = "text"
	JSONResult        ResultFormat = "json"
	FingerprintResult ResultFormat = "fingerprint"
)

// Result 的 Data 是稳定的语义内容，或调用方生成的稳定指纹（应含算法/版本前缀）。
// TextResult 逐字节比较；JSONResult 规范化对象键序；FingerprintResult 比较指纹字节。
// 三种格式互不等价。Data 不被修改或保留，内部只保存 SHA-256。
type Result struct {
	Status Status
	Format ResultFormat
	Data   []byte
}

// Event 必须是一次已完成调用的摘要，按调用方确定的顺序且恰好提交一次。
// 调用方负责配对实际执行参数与结果、剔除时间戳/耗时等元数据，并判断真实进展。
// Arguments 为 JSON 对象；nil/空字节表示 {}。数字应来自原始 JSON 或 json.Number。
// SourceID 仅用于证据定位，不参与比较；不提供时仍有 Detector 内单调递增的序号。
// AllowRepeat 用于可信调用方允许的轮询等事件；Progress 优先于 AllowRepeat。
// 工具结果中的文字或模型输出不能自行设置这两个标志。
type Event struct {
	RunID       string
	SourceID    string
	ToolName    string
	Arguments   json.RawMessage
	Result      Result
	AllowRepeat bool
	Progress    bool
}

type Level string

const (
	Normal        Level = "normal"
	Warning       Level = "warning"
	StopSuggested Level = "stop_suggested"
)

type Reason string

const (
	NoPattern       Reason = "no_pattern"
	AllowedRepeat   Reason = "allowed_repeat"
	ProgressReset   Reason = "progress_reset"
	RepeatedFailure Reason = "repeated_failure"
	RepeatedResult  Reason = "repeated_result"
	Alternating     Reason = "alternating"
)

type Decision struct {
	Level    Level
	Reason   Reason
	Evidence *Evidence // 只有 Warning/StopSuggested 有证据；返回独立副本。
}

// Evidence 只描述当前有限窗口内的匹配后缀，不是完整历史或绝对无进展证明。
// Events 按提交顺序排列，只含参与匹配的事件；允许的轮询会造成序号间隙。
type Evidence struct {
	RunID         string
	WindowSize    int
	Count         int
	WarningAt     int
	StopAt        int
	FirstSequence uint64
	LastSequence  uint64
	Events        []EventEvidence
}

type EventEvidence struct {
	Sequence      uint64
	SourceID      string
	ToolName      string
	ArgumentsHash string // 规范化参数的 SHA-256，小写十六进制。
	Status        Status
	ResultFormat  ResultFormat
	ResultHash    string // 稳定内容/指纹的 SHA-256，小写十六进制。
}

var (
	ErrRunMismatch   = errors.New("loopguard: 事件 RunID 与 Detector 绑定的 RunID 不同")
	ErrUninitialized = errors.New("loopguard: 必须使用 New 创建 Detector")
)
