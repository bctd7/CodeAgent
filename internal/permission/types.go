package permission

// WorkMode 表示工作范围，与审批模式独立。
type WorkMode string

const (
	Plan  WorkMode = "plan"
	Build WorkMode = "build"
)

// ApprovalMode 只决定内置能力是否需要确认，不扩大工作范围。
type ApprovalMode string

const (
	Default     ApprovalMode = "default"
	AutoEdit    ApprovalMode = "auto_edit"
	AutoApprove ApprovalMode = "auto_approve"
)

// Decision 表示规则或当前会话授权的判断。
type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
)

// SourceKind 必须由可信注册表确定；外置工具（包括 MCP）使用 External。
type SourceKind string

const (
	Builtin  SourceKind = "builtin"
	External SourceKind = "external"
)

// Capability 必须来自可信内置实现，不能来自模型或 MCP readOnlyHint。
type Capability string

const (
	ReadOnly  Capability = "read_only"
	FileWrite Capability = "file_write"
	Shell     Capability = "shell"
	Unknown   Capability = "unknown"
)

// Tool 标识具体工具；SourceID 是可信来源身份，不是可冒用的展示名称。
// 来源重新连接后若身份发生变化，调用方也必须更换 SourceID。
type Tool struct {
	Source   SourceKind
	SourceID string
	ToolID   string
}

// Policy 是可信调用方构造的当前策略快照，不从项目文件或模型参数加载。
type Policy struct {
	WorkMode     WorkMode
	ApprovalMode ApprovalMode
	// WorkingDir 和 PlanDir 是调用方解析符号链接、junction 后的真实绝对目录。
	WorkingDir string
	PlanDir    string
	// DeniedTools 精确匹配用户明确禁止的工具；没有通配、数字优先级或配置合并。
	DeniedTools []Tool
}

// Request 是可信调用方构造的完整执行快照，不能直接反序列化模型提供的权限 flags。
type Request struct {
	// RequestID 在所属会话中唯一且不得重新分配给另一次执行。
	RequestID  string
	SessionID  string
	Tool       Tool
	Capability Capability
	// Arguments 保存所有执行参数的不可变完整表示，只用于一次性授权的精确关联。
	// 调用方负责稳定序列化；本包不解析 shell，也不根据命令字符串猜测危险性。
	Arguments string
	// FileTargets 覆盖此次普通文件写入的所有目标，不可隐藏额外效果。
	// 调用方先解析真实目标（含不存在文件的已有父目录），再传绝对路径或
	// 相对 WorkingDir 的等价真实路径。本包只规范化，不读取文件系统。
	FileTargets []string
}

// Reason 是可用于界面和审计的稳定原因码。
type Reason string

const (
	InvalidRequest       Reason = "invalid_request"
	InvalidPolicy        Reason = "invalid_policy"
	ExplicitDeny         Reason = "explicit_deny"
	PlanRestriction      Reason = "plan_restriction"
	PlanMarkdown         Reason = "plan_markdown"
	BuiltinReadOnly      Reason = "builtin_read_only"
	ExternalConfirmation Reason = "external_confirmation"
	WriteConfirmation    Reason = "write_confirmation"
	ShellConfirmation    Reason = "shell_confirmation"
	UnknownConfirmation  Reason = "unknown_confirmation"
	WorkspaceAutoEdit    Reason = "workspace_auto_edit"
	BuiltinAutoApprove   Reason = "builtin_auto_approve"
	OnceGranted          Reason = "once_granted"
	SessionToolGranted   Reason = "session_tool_granted"
	RequestRejected      Reason = "request_rejected"
)

// Result 关联原请求；只有 Session.Evaluate 的 Ask 才附带可回传的确认凭据。
type Result struct {
	RequestID    string
	SessionID    string
	Decision     Decision
	Reason       Reason
	Confirmation *Confirmation
}
