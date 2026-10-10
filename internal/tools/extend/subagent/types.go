// Package subagent 提供可注入的子代理工具适配层；实际执行由 Controller 负责。
package subagent

import (
	"context"
	"fmt"
	"strings"

	"CodeAgent/internal/llm"
)

type Role string

const (
	Explore Role = "explore"
	Worker  Role = "worker"
)

// TaskState 是子任务对外状态；与 Agent 实例的关闭状态无关。
type TaskState string

const (
	Running   TaskState = "running"
	Completed TaskState = "completed"
	Failed    TaskState = "failed"
	Cancelled TaskState = "cancelled"
)

// Task 是业务实现返回的快照。结果复用现有模型内容类型，不暴露整个会话历史。
// 发起取消后尚未完成收尾时仍可为 running；终态由业务实现确认。
type Task struct {
	ID        string             `json:"id"`
	State     TaskState          `json:"state"`
	Result    []llm.ContentBlock `json:"result,omitempty"`
	Error     string             `json:"error,omitempty"`
	RunID     string             `json:"run_id,omitempty"`
	SessionID string             `json:"session_id,omitempty"`
	ResultRef string             `json:"result_ref,omitempty"`
	Truncated bool               `json:"truncated,omitempty"`
}

type SpawnRequest struct {
	Task string `json:"task"`
	Role Role   `json:"role"`
	// ModelRef 沿用仓库配置引用语义，不接受凭据或地址；nil 由 Runtime 继承当前模型。
	// 工具层不解析模型是否存在，也不选择默认模型。
	ModelRef *string `json:"model_ref,omitempty"`
	// FileScope 是 worker 可修改的文件列表；省略时由首次修改取得整个 Run 的占用。
	FileScope []string `json:"file_scope,omitempty"`
}

type SendRequest struct {
	ID    string `json:"id"`
	Input string `json:"input"`
}

type WaitMode string

const (
	Any WaitMode = "any"
	All WaitMode = "all"
)

// MaxWaitTimeoutMS 限制一次模型工具等待为至多五分钟。
const MaxWaitTimeoutMS int64 = 300000

type WaitRequest struct {
	IDs       []string `json:"ids"`
	Mode      WaitMode `json:"mode"`
	TimeoutMS int64    `json:"timeout_ms"`
}

// WaitResult 必须包含全部请求 ID 的快照，明确保留仍运行和各终态。
// TimedOut 表示仅等待超时，不意味着取消任一任务；父 context 取消通过 error 返回。
type WaitResult struct {
	Tasks    []Task `json:"tasks"`
	TimedOut bool   `json:"timed_out"`
}

type CancelRequest struct {
	ID string `json:"id"`
}

// Controller 必须绑定调用者作用域，并验证每个 ID 归属；不能依赖模型传入父会话来授权。
// 实现须支持并发调用、合作响应 context；返回的数据须为稳定快照，不得由后台继续修改。
// 此包只转发请求，不创建 Session、管理任务生命周期或代替实现进行权限批准。
type Controller interface {
	// Spawn 接受任务后立即返回 ID 和 running 快照，不等待任务完成。
	// 独立子 Session、共享目录协调、权限继承和禁止子代理继续 spawn 由实现负责。
	Spawn(context.Context, SpawnRequest) (Task, error)
	// Send 在运行中任务的安全节点消费补充指令；终态任务可沿用上下文继续。
	Send(context.Context, SendRequest) (Task, error)
	// Wait 任一或全部任务进入终态（含失败及取消）后返回；超时返回各快照和 TimedOut。
	// TimeoutMS 只限制本次等待，不能取消底层任务。
	Wait(context.Context, WaitRequest) (WaitResult, error)
	// Cancel 只请求取消当前任务并收尾，返回当时快照；不关闭 Runtime、删 Session 或回滚文件。
	Cancel(context.Context, CancelRequest) (Task, error)
}

func (r SpawnRequest) validate() error {
	if strings.TrimSpace(r.Task) == "" {
		return fmt.Errorf("参数 task 必须为非空字符串")
	}
	if r.Role != Explore && r.Role != Worker {
		return fmt.Errorf("参数 role 必须为 explore 或 worker")
	}
	if r.ModelRef != nil && strings.TrimSpace(*r.ModelRef) == "" {
		return fmt.Errorf("参数 model_ref 必须为非空字符串")
	}
	if len(r.Task) > 1<<20 || len(r.FileScope) > 256 {
		return fmt.Errorf("任务说明或文件范围超出上限")
	}
	if r.Role == Explore && len(r.FileScope) > 0 {
		return fmt.Errorf("explore 不接受修改文件范围")
	}
	for _, path := range r.FileScope {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("文件范围不能包含空路径")
		}
	}
	return nil
}

func (r SendRequest) validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Input) == "" {
		return fmt.Errorf("参数 id 和 input 必须为非空字符串")
	}
	return nil
}

func (r WaitRequest) validate() error {
	if len(r.IDs) == 0 || len(r.IDs) > 64 {
		return fmt.Errorf("参数 ids 必须包含至少一个任务 ID")
	}
	seen := make(map[string]bool, len(r.IDs))
	for _, id := range r.IDs {
		if strings.TrimSpace(id) == "" || seen[id] {
			return fmt.Errorf("参数 ids 不能包含空白或重复任务 ID")
		}
		seen[id] = true
	}
	if r.Mode != Any && r.Mode != All {
		return fmt.Errorf("参数 mode 必须为 any 或 all")
	}
	if r.TimeoutMS < 1 || r.TimeoutMS > MaxWaitTimeoutMS {
		return fmt.Errorf("参数 timeout_ms 必须为 1 至 %d 的整数", MaxWaitTimeoutMS)
	}
	return nil
}

func (r CancelRequest) validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("参数 id 必须为非空字符串")
	}
	return nil
}
