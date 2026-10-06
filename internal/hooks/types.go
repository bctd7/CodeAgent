package hooks

import (
	"context"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
)

// Hook 输入使用隔离副本，不包含数据库连接、凭据或可变 Agent。
// 只有返回值中允许替换的字段会被应用，直接修改输入不影响执行。
type Completion struct {
	SessionID string
	Result    agent.RunResult
}
type ToolRequest struct {
	SessionID, RunID string
	StepIndex        int
	Call             llm.ContentBlock
}

// 空 Reason 允许执行；非空 Reason 拒绝并形成未执行工具结果。
type ToolDecision struct {
	Reason    string
	Arguments *map[string]any
}
type CompletionHook func(context.Context, Completion) error
type BeforeToolHook func(context.Context, ToolRequest) (ToolDecision, error)
type Failure struct {
	Point, SessionID, RunID string
	Error                   error
}

type ModelRequest struct {
	SessionID, RunID string
	StepIndex        int
	Context          llm.RequestContext
}

type ModelResponse struct {
	SessionID, RunID string
	StepIndex        int
	Response         llm.AssistantMessage
}

type ToolResponse struct {
	SessionID, RunID string
	StepIndex        int
	Result           llm.ToolResultMessage
}

// 替换值为 nil 表示不修改，非 nil 的空值表示主动清空。
// 请求只允许替换系统提示词和消息，工具前只允许替换参数，工具后只允许替换内容。
// 模型设置、工具名称、调用 ID 和真实成功或失败状态不通过返回值修改。
type BeforeModelRequestHook func(context.Context, ModelRequest) (*llm.RequestContext, error)
type AfterModelResponseHook func(context.Context, ModelResponse) error
type AfterToolHook func(context.Context, ToolResponse) (*[]llm.ContentBlock, error)
