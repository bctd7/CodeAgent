package permission

import (
	"errors"
	"slices"
	"sync"
)

// 授权错误区分无效会话、失效确认和未知用户选择。
var (
	ErrInvalidSession    = errors.New("permission: session ID is required")
	ErrStaleConfirmation = errors.New("permission: confirmation is invalid or already consumed")
	ErrInvalidChoice     = errors.New("permission: invalid confirmation choice")
)

// Choice 来自真实用户的确认选择；拒绝本次不会生成永久禁止规则。
type Choice string

const (
	Once        Choice = "once"
	SessionTool Choice = "session_tool"
	Reject      Choice = "reject"
)

// Confirmation 是内存内的单次确认凭据，必须原样回传所属 Session 的 Grant。
// 不可通过 JSON 重建；私有凭据将确认绑定到完整请求快照。
type Confirmation struct {
	RequestID string
	SessionID string
	Tool      Tool
	ticket    *pendingRequest
}

type pendingRequest struct {
	request    Request
	workingDir string
}

type onceDecision struct {
	request    Request
	workingDir string
	decision   Decision
}

// Session 是单个会话的显式内存授权容器，不允许复制，不持久化，也不启动 goroutine。
// 所有方法并发安全；关闭会话时调用方必须 Clear 并丢弃容器。
type Session struct {
	mu      sync.Mutex
	id      string
	tools   map[Tool]struct{}
	pending map[string]*pendingRequest
	once    map[string]onceDecision
}

// NewSession 为一个明确会话创建隔离的授权容器，零值容器不会放行请求。
func NewSession(sessionID string) (*Session, error) {
	if sessionID == "" {
		return nil, ErrInvalidSession
	}
	return &Session{
		id: sessionID, tools: make(map[Tool]struct{}),
		pending: make(map[string]*pendingRequest), once: make(map[string]onceDecision),
	}, nil
}

// Evaluate 先重判当前规则，再处理本会话授权。Once 只解决 Ask，Reject 拒绝本次执行。
// Ask 的重复判断复用同一确认；请求快照或工作目录改变则使旧确认和一次性授权失效。
// 规则 Allow/Deny 也会清除该请求的待确认和一次性选择，防止残留复用。
func (session *Session) Evaluate(policy Policy, request Request) Result {
	session.mu.Lock()
	defer session.mu.Unlock()
	result := Evaluate(policy, request)
	if session.id == "" || request.SessionID != session.id {
		result.Decision, result.Reason = Deny, InvalidRequest
		return result
	}
	if result.Decision == Deny {
		delete(session.pending, request.RequestID)
		delete(session.once, request.RequestID)
		return result
	}
	if choice, exists := session.once[request.RequestID]; exists {
		delete(session.once, request.RequestID)
		if choice.workingDir == policy.WorkingDir && sameRequest(choice.request, request) &&
			(choice.decision == Deny || result.Decision == Ask) {
			delete(session.pending, request.RequestID)
			result.Decision = choice.decision
			result.Reason = OnceGranted
			if choice.decision == Deny {
				result.Reason = RequestRejected
			}
			return result
		}
	}
	if result.Decision != Ask {
		delete(session.pending, request.RequestID)
		return result
	}
	if _, exists := session.tools[request.Tool]; exists {
		delete(session.pending, request.RequestID)
		result.Decision, result.Reason = Allow, SessionToolGranted
		return result
	}
	pending := session.pending[request.RequestID]
	if pending == nil || pending.workingDir != policy.WorkingDir || !sameRequest(pending.request, request) {
		pending = &pendingRequest{request: snapshotRequest(request), workingDir: policy.WorkingDir}
		session.pending[request.RequestID] = pending
	}
	result.Confirmation = &Confirmation{
		RequestID: request.RequestID, SessionID: request.SessionID,
		Tool: request.Tool, ticket: pending,
	}
	return result
}

// Grant 接收一次真实用户选择，确认凭据不能重复提交或跨容器使用。
// 成功只表示保存选择，不表示工具已获准执行；必须再次 Evaluate 当前规则。
// SessionTool 精确授权本会话的来源和工具，参数可变；不扩大到整台服务器。
func (session *Session) Grant(confirmation Confirmation, choice Choice) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if choice != Once && choice != SessionTool && choice != Reject {
		return ErrInvalidChoice
	}
	pending := session.pending[confirmation.RequestID]
	if pending == nil || pending != confirmation.ticket || confirmation.SessionID != session.id ||
		confirmation.SessionID != pending.request.SessionID || confirmation.Tool != pending.request.Tool {
		return ErrStaleConfirmation
	}
	delete(session.pending, confirmation.RequestID)
	if choice == SessionTool {
		session.tools[pending.request.Tool] = struct{}{}
	} else {
		decision := Allow
		if choice == Reject {
			decision = Deny
		}
		session.once[confirmation.RequestID] = onceDecision{
			request: pending.request, workingDir: pending.workingDir, decision: decision,
		}
	}
	return nil
}

// Revoke 撤回当前会话一个具体来源工具的授权，同时使其待确认和一次性选择失效。
func (session *Session) Revoke(tool Tool) {
	session.mu.Lock()
	defer session.mu.Unlock()
	delete(session.tools, tool)
	for id, pending := range session.pending {
		if pending.request.Tool == tool {
			delete(session.pending, id)
		}
	}
	for id, choice := range session.once {
		if choice.request.Tool == tool {
			delete(session.once, id)
		}
	}
}

// Clear 撤回全部授权并使旧确认失效；不会修改调用方的显式禁止规则。
func (session *Session) Clear() {
	session.mu.Lock()
	defer session.mu.Unlock()
	clear(session.tools)
	clear(session.pending)
	clear(session.once)
}

// Discard 使一个已结束、取消或超时请求的确认失效，不影响其他并行审批。
func (session *Session) Discard(requestID string) {
	session.mu.Lock()
	defer session.mu.Unlock()
	delete(session.pending, requestID)
	delete(session.once, requestID)
}

func snapshotRequest(request Request) Request {
	request.FileTargets = slices.Clone(request.FileTargets)
	return request
}

func sameRequest(left, right Request) bool {
	return left.RequestID == right.RequestID && left.SessionID == right.SessionID &&
		left.Tool == right.Tool && left.Capability == right.Capability &&
		left.Arguments == right.Arguments && slices.Equal(left.FileTargets, right.FileTargets)
}
