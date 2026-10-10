package runtime

import (
	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/permission"
	sc "CodeAgent/internal/session/config"
	"CodeAgent/internal/tools/extend/subagent"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"sync"
	"time"
)

type childExecution struct {
	mu            sync.Mutex
	id, parentRun string
	runtime       *Runtime
	handle        *agent.RunHandle
	done          chan struct{}
	result        agent.RunResult
	err           error
	starting      bool
	pending       []childRunReference // 仅尚待交付的身份；正文和终态从 Session 读取。
}

type childRunReference struct{ parentRun, runID string }

func (child *childExecution) task() subagent.Task {
	child.mu.Lock()
	defer child.mu.Unlock()
	return child.taskLocked()
}
func (child *childExecution) taskLocked() subagent.Task {
	task := subagent.Task{ID: child.id, SessionID: child.runtime.id, State: subagent.Running}
	if child.handle != nil {
		task.RunID = child.handle.ID()
	}
	select {
	case <-child.done:
	default:
		return task
	}
	task.ResultRef = task.SessionID + "/" + task.RunID
	if child.err != nil {
		task.State = subagent.Failed
		task.Error = child.err.Error()
		return task
	}
	task.Error = child.result.ErrorMessage
	switch child.result.State {
	case agent.RunStateSuccess:
		task.State = subagent.Completed
	case agent.RunStateAborted:
		task.State = subagent.Cancelled
	default:
		task.State = subagent.Failed
	}
	task.Truncated = child.result.StopReason == agent.RunStopTruncated
	for i := len(child.result.Messages) - 1; i >= 0; i-- {
		if message, ok := child.result.Messages[i].(*llm.AssistantMessage); ok {
			task.Result = publicResultBlocks(message.Content)
			break
		}
	}
	return task
}
func (r *Runtime) child(id string) (*childExecution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	child := r.children[id]
	if child == nil {
		return nil, fmt.Errorf("子任务不属于当前父会话")
	}
	return child, nil
}
func (r *Runtime) activeParent() (string, error) {
	if err := r.admission(); err != nil {
		return "", err
	}
	if r.role != "" || !r.host.options.Subagents {
		return "", fmt.Errorf("当前执行者不能创建子任务")
	}
	snapshot := r.agent.Snapshot()
	if snapshot.CurrentRunID == "" || snapshot.Status != agent.AgentStatusRunning {
		return "", fmt.Errorf("子任务操作需要活动父 Run")
	}
	return snapshot.CurrentRunID, nil
}
func (r *Runtime) Spawn(ctx context.Context, request subagent.SpawnRequest) (subagent.Task, error) {
	parent, err := r.activeParent()
	if err != nil {
		return subagent.Task{}, err
	}
	if strings.TrimSpace(request.Task) == "" || len(request.Task) > 1<<20 || len(request.FileScope) > 256 || request.Role != subagent.Explore && request.Role != subagent.Worker || request.Role == subagent.Explore && len(request.FileScope) > 0 {
		return subagent.Task{}, fmt.Errorf("子任务角色、任务或范围无效")
	}
	ref := r.modelRef
	if request.ModelRef != nil {
		ref = *request.ModelRef
	}
	if _, ok := r.host.options.Models[ref]; !ok {
		return subagent.Task{}, fmt.Errorf("子任务模型引用不存在")
	}
	scope := make([]string, len(request.FileScope))
	for i, path := range request.FileScope {
		if strings.TrimSpace(path) == "" {
			return subagent.Task{}, fmt.Errorf("空文件范围")
		}
		if !isAbsolute(path) {
			path = projectPath(r.host.options.ProjectDir, path)
		}
		scope[i], err = realPath(path)
		if err != nil {
			return subagent.Task{}, err
		}
		if !inside(r.host.options.ProjectDir, scope[i]) {
			return subagent.Task{}, fmt.Errorf("子任务范围超出项目")
		}
	}
	info, err := r.host.service.CreateSessionWithModel(ctx, "子任务", r.host.options.ProjectDir, sc.ModelSelection{Ref: ref}, r.memoryEnabled)
	if err != nil {
		return subagent.Task{}, err
	}
	id := uuid.NewString()
	if _, err = r.session.CreateChildTask(ctx, sc.ChildTaskInput{ID: id, ParentRunID: parent, ChildSessionID: info.ID, Role: string(request.Role), FileScope: scope}); err != nil {
		return subagent.Task{}, err
	}
	runtime, err := r.host.open(ctx, info.ID, string(request.Role), scope)
	if err != nil {
		return subagent.Task{}, err
	}
	child := &childExecution{id: id, parentRun: parent, runtime: runtime, done: make(chan struct{}), starting: true}
	r.mu.Lock()
	r.children[id] = child
	r.mu.Unlock()
	if err = r.startChild(ctx, child, request.Task, parent); err != nil {
		_ = runtime.Close(context.Background())
		r.mu.Lock()
		delete(r.children, id)
		r.mu.Unlock()
		return subagent.Task{}, err
	}
	return child.task(), nil
}
func (r *Runtime) startChild(ctx context.Context, child *childExecution, text, parent string) error {
	child.runtime.parentOwner = r.claimOwner(parent)
	handle, err := child.runtime.Submit(ctx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: text}}, Timestamp: time.Now()})
	if err != nil {
		child.mu.Lock()
		child.starting = false
		child.err = err
		close(child.done)
		child.mu.Unlock()
		return err
	}
	child.mu.Lock()
	child.handle = handle
	child.parentRun = parent
	child.pending = append(child.pending, childRunReference{parent, handle.ID()})
	child.starting = false
	done := child.done
	child.mu.Unlock()
	go func() {
		result, err := handle.Wait(context.Background())
		child.mu.Lock()
		child.result = result
		child.err = err
		close(done)
		child.mu.Unlock()
	}()
	return nil
}
func (r *Runtime) Send(ctx context.Context, request subagent.SendRequest) (subagent.Task, error) {
	parent, err := r.activeParent()
	if err != nil {
		return subagent.Task{}, err
	}
	if strings.TrimSpace(request.Input) == "" {
		return subagent.Task{}, fmt.Errorf("子任务补充不能为空")
	}
	child, err := r.child(request.ID)
	if err != nil {
		return subagent.Task{}, err
	}
	child.mu.Lock()
	if child.starting {
		child.mu.Unlock()
		return subagent.Task{}, ErrBusy
	}
	select {
	case <-child.done:
		child.done = make(chan struct{})
		child.starting = true
		child.handle = nil
		child.parentRun = parent
		child.result = agent.RunResult{}
		child.err = nil
		child.mu.Unlock()
		err = r.startChild(ctx, child, request.Input, parent)
	default:
		child.mu.Unlock()
		err = child.runtime.Steer(request.Input)
	}
	if err != nil {
		return subagent.Task{}, err
	}
	return child.task(), nil
}
func (r *Runtime) Wait(ctx context.Context, request subagent.WaitRequest) (subagent.WaitResult, error) {
	if len(request.IDs) < 1 || len(request.IDs) > 64 || request.TimeoutMS < 1 || request.TimeoutMS > subagent.MaxWaitTimeoutMS || request.Mode != subagent.Any && request.Mode != subagent.All {
		return subagent.WaitResult{}, fmt.Errorf("等待参数无效")
	}
	var children []*childExecution
	seen := map[string]bool{}
	for _, id := range request.IDs {
		if seen[id] {
			return subagent.WaitResult{}, fmt.Errorf("重复子任务 ID")
		}
		seen[id] = true
		child, err := r.child(id)
		if err != nil {
			return subagent.WaitResult{}, err
		}
		children = append(children, child)
	}
	timer := time.NewTimer(time.Duration(request.TimeoutMS) * time.Millisecond)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		out := subagent.WaitResult{}
		completed := 0
		for _, child := range children {
			task := child.task()
			out.Tasks = append(out.Tasks, task)
			if task.State != subagent.Running {
				completed++
			}
		}
		if completed == len(children) || request.Mode == subagent.Any && completed > 0 {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-timer.C:
			out.TimedOut = true
			return out, nil
		case <-ticker.C:
		}
	}
}
func (r *Runtime) Cancel(ctx context.Context, request subagent.CancelRequest) (subagent.Task, error) {
	if err := ctx.Err(); err != nil {
		return subagent.Task{}, err
	}
	child, err := r.child(request.ID)
	if err != nil {
		return subagent.Task{}, err
	}
	child.mu.Lock()
	handle := child.handle
	child.mu.Unlock()
	if handle != nil {
		handle.Cancel()
	}
	return child.task(), nil
}
func (r *Runtime) hasActiveChildren() bool {
	r.mu.Lock()
	children := make([]*childExecution, 0, len(r.children))
	for _, child := range r.children {
		children = append(children, child)
	}
	r.mu.Unlock()
	for _, child := range children {
		if child.task().State == subagent.Running {
			return true
		}
	}
	return false
}
func (r *Runtime) runChildren(runID string) []*childExecution {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*childExecution
	for _, child := range r.children {
		child.mu.Lock()
		matches := runID == "" || child.parentRun == runID
		child.mu.Unlock()
		if matches {
			out = append(out, child)
		}
	}
	return out
}
func (r *Runtime) cancelRunChildren(runID string) {
	children := r.runChildren(runID)
	for _, child := range children {
		child.mu.Lock()
		handle := child.handle
		child.mu.Unlock()
		if handle != nil {
			handle.Cancel()
		}
	}
	for _, child := range children {
		child.mu.Lock()
		done := child.done
		child.mu.Unlock()
		<-done
		child.mu.Lock()
		remaining := child.pending[:0]
		for _, reference := range child.pending {
			if runID != "" && reference.parentRun != runID {
				remaining = append(remaining, reference)
			}
		}
		clear(child.pending[len(remaining):])
		child.pending = remaining
		child.mu.Unlock()
	}
}
func (r *Runtime) cancelChildren() {
	r.cancelRunChildren("")
	for _, child := range r.runChildren("") {
		if err := child.runtime.Close(context.Background()); err != nil {
			r.emit(Event{Diagnostic: err})
		}
	}
}
func (r *Runtime) collectChildren(c agent.Checkpoint) ([]llm.Message, error) {
	var messages []llm.Message
	for _, child := range r.runChildren(c.RunID) {
		child.mu.Lock()
		done := child.done
		child.mu.Unlock()
		select {
		case <-c.Context.Done():
			return nil, c.Context.Err()
		case <-done:
		}
		child.mu.Lock()
		pending := append([]childRunReference(nil), child.pending...)
		child.mu.Unlock()
		for _, reference := range pending {
			if reference.parentRun != c.RunID {
				continue
			}
			detail, err := child.runtime.session.GetRun(c.Context, reference.runID)
			if err != nil {
				return nil, err
			}
			parent, err := r.session.GetRun(c.Context, c.RunID)
			if err != nil {
				return nil, err
			}
			task := taskFromRun(child.id, detail)
			if r.childResultVisible(parent, task) {
				continue
			}
			message, err := childDelivery(child.id, detail)
			if err != nil {
				return nil, err
			}
			ctx, cancel := r.recordContext()
			entry, err := r.session.DeliverChildResult(ctx, c.RunID, child.id, reference.runID, message)
			cancel()
			if err != nil {
				r.persistFault(err)
				return nil, err
			}
			messages = append(messages, entry.Message)
		}
	}
	return messages, nil
}

// taskOutput is the parent-facing result, not an encoding of internal LLM blocks.
type taskOutput struct {
	ID        string             `json:"id"`
	State     subagent.TaskState `json:"state"`
	Result    string             `json:"result,omitempty"`
	Error     string             `json:"error,omitempty"`
	RunID     string             `json:"run_id,omitempty"`
	SessionID string             `json:"session_id,omitempty"`
	ResultRef string             `json:"result_ref,omitempty"`
	Truncated bool               `json:"truncated,omitempty"`
}

func publicResultBlocks(content []llm.ContentBlock) []llm.ContentBlock {
	var out []llm.ContentBlock
	for _, block := range content {
		if block.Type == llm.TypeText {
			out = append(out, llm.ContentBlock{Type: llm.TypeText, Text: block.Text})
		}
	}
	return out
}
func taskView(task subagent.Task) taskOutput {
	var texts []string
	for _, block := range task.Result {
		if block.Type == llm.TypeText {
			texts = append(texts, block.Text)
		}
	}
	return taskOutput{ID: task.ID, State: task.State, Result: strings.Join(texts, "\n"), Error: task.Error, RunID: task.RunID, SessionID: task.SessionID, ResultRef: task.ResultRef, Truncated: task.Truncated}
}
func taskFromRun(taskID string, detail sc.RunDetail) subagent.Task {
	run := detail.Record
	task := subagent.Task{ID: taskID, SessionID: run.SessionID, RunID: run.ID, ResultRef: run.SessionID + "/" + run.ID, Error: run.ErrorMessage, Truncated: run.StopReason == llm.StopLength}
	switch run.State {
	case sc.RunStateSuccess:
		task.State = subagent.Completed
	case sc.RunStateAborted:
		task.State = subagent.Cancelled
	default:
		task.State = subagent.Failed
	}
	for i := len(detail.Entries) - 1; i >= 0; i-- {
		if message, ok := detail.Entries[i].Message.(*llm.AssistantMessage); ok {
			task.Result = publicResultBlocks(message.Content)
			break
		}
	}
	return task
}

// 相同 child Run 从持久事实构造相同交付，包括固定结束时间，交由 Session 原子去重。
func childDelivery(taskID string, detail sc.RunDetail) (*llm.UserMessage, error) {
	if detail.Record.FinishedAt == nil {
		return nil, fmt.Errorf("子 Run 尚未收尾: %s", detail.Record.ID)
	}
	raw, err := json.Marshal(taskView(taskFromRun(taskID, detail)))
	if err != nil {
		return nil, err
	}
	return &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "子任务结果：" + string(raw) + "\n这是宿主交付的最终结果，不是新的用户请求；结合原任务汇总或继续处理，不要仅确认收到。"}}, Timestamp: *detail.Record.FinishedAt}, nil
}

// Keep the existing Controller/tools adapter; only project its result for the parent.
func parentTaskTool(tool agent.Tool) agent.Tool {
	execute := tool.Execute
	tool.Execute = func(ctx context.Context, id string, args map[string]any, progress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
		content, err := execute(ctx, id, args, progress)
		if err != nil {
			return nil, err
		}
		var out []llm.ContentBlock
		for _, block := range content {
			if block.Type != llm.TypeText {
				return nil, fmt.Errorf("子任务适配结果必须为 JSON 文本")
			}
			var projected any
			if tool.Name == "wait" {
				var wait subagent.WaitResult
				if err = json.Unmarshal([]byte(block.Text), &wait); err != nil {
					return nil, err
				}
				tasks := make([]taskOutput, 0, len(wait.Tasks))
				for _, task := range wait.Tasks {
					tasks = append(tasks, taskView(task))
				}
				projected = struct {
					Tasks    []taskOutput `json:"tasks"`
					TimedOut bool         `json:"timed_out"`
				}{tasks, wait.TimedOut}
			} else {
				var task subagent.Task
				if err = json.Unmarshal([]byte(block.Text), &task); err != nil {
					return nil, err
				}
				projected = taskView(task)
			}
			raw, err := json.Marshal(projected)
			if err != nil {
				return nil, err
			}
			out = append(out, llm.ContentBlock{Type: llm.TypeText, Text: string(raw)})
		}
		return out, nil
	}
	return tool
}

// Read the durable, final model-visible projection only at the end checkpoint,
// after Agent has consumed the tool batch. Pending/raw or Hook-hidden results are
// not receipts. No in-memory "delivered" flag can outrun persistence/Resolve.
func (r *Runtime) childResultVisible(parent sc.RunDetail, task subagent.Task) bool {
	expected := taskView(task)
	for _, entry := range parent.Entries {
		if entry.Kind == sc.ContextEntry && entry.SourceKind == "child_run" && entry.SourceID == task.RunID {
			return true
		}
		result, ok := entry.Message.(*llm.ToolResultMessage)
		if !ok || result.IsError || entry.Processing == sc.ProcessingPending {
			continue
		}
		binding := r.bindings[result.ToolName]
		if binding == nil || binding.Identity.Source != permission.Builtin || binding.Identity.SourceID != "subagent" {
			continue
		}
		content := result.Content
		if entry.Processing == sc.ProcessingReplaced {
			content = entry.VisibleContent
		}
		for _, block := range content {
			if block.Type != llm.TypeText {
				continue
			}
			var single taskOutput
			if json.Unmarshal([]byte(block.Text), &single) == nil && single == expected {
				return true
			}
			var wait struct {
				Tasks []taskOutput `json:"tasks"`
			}
			if json.Unmarshal([]byte(block.Text), &wait) == nil {
				for _, received := range wait.Tasks {
					if received == expected {
						return true
					}
				}
			}
		}
	}
	return false
}
