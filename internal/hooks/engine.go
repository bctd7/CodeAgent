// 本文件负责初始化、事件分发、并行执行和决策归并。
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Engine 保存不可变配置快照，可供多个调用并发使用。
type Engine struct {
	hooks  map[Event][]compiledHook
	runner *CommandRunner
	// endBudget 限制 SessionEnd 的整体执行时间。
	endBudget time.Duration
}

// New 验证所有来源并构建配置快照，不在构建阶段执行命令。
func New(sources []Source, options Options) (*Engine, error) {
	runner, err := NewCommandRunner(options.Runner)
	if err != nil {
		return nil, err
	}
	if options.MaxHandlers == 0 {
		options.MaxHandlers = 256
	}
	if options.MaxHandlers < 1 {
		return nil, fmt.Errorf("%w: MaxHandlers must be positive", ErrConfiguration)
	}
	e := &Engine{hooks: make(map[Event][]compiledHook), runner: runner, endBudget: 1500 * time.Millisecond}
	seen := map[string]bool{}
	total := 0
	for _, source := range sources {
		if source.ID == "" || seen[source.ID] {
			return nil, fmt.Errorf("%w: source ID must be nonempty and unique", ErrConfiguration)
		}
		seen[source.ID] = true
		if source.Kind == "" {
			source.Kind = SettingsSource
		}
		compiled, err := compileSource(source, runner.options.InputBytes, options.MaxHandlers-total)
		if err != nil {
			return nil, err
		}
		total += len(compiled)
		for _, h := range compiled {
			e.hooks[h.event] = append(e.hooks[h.event], h)
			if h.event == SessionEnd && h.source.Kind == SettingsSource {
				e.endBudget = max(e.endBudget, min(h.timeout, 60*time.Second))
			}
		}
	}
	return e, nil
}

// dispatchResult 汇集执行结果与公共通知。
// 类型化事件入口先处理公共停止控制，再归并各事件专属决策。
type dispatchResult struct {
	Notices
	Control
	effects []effect
}

// SessionStart 返回会话开始时的附加上下文和公共停止控制。
func (e *Engine) SessionStart(ctx context.Context, in SessionStartInput) (SessionStartResult, error) {
	d, err := e.dispatch(ctx, in)
	r := SessionStartResult{Notices: d.Notices, Control: d.Control}
	if err == nil && !r.StopProcessing {
		r.AdditionalContext = collectContexts(d.effects)
	}
	return r, err
}

// UserPromptSubmit 归并提示拒绝结果；拒绝时不返回附加上下文。
func (e *Engine) UserPromptSubmit(ctx context.Context, in UserPromptSubmitInput) (UserPromptSubmitResult, error) {
	d, err := e.dispatch(ctx, in)
	r := UserPromptSubmitResult{Notices: d.Notices, Control: d.Control}
	if err != nil || r.StopProcessing {
		return r, err
	}
	for _, out := range d.effects {
		if out.decision.block {
			r.Rejected = true
			r.Reasons = appendNonempty(r.Reasons, out.decision.reason)
		}
	}
	if !r.Rejected {
		r.AdditionalContext = collectContexts(d.effects)
	}
	return r, nil
}

// PreToolUse 先归并权限，再返回可应用的输入改写。
func (e *Engine) PreToolUse(ctx context.Context, in PreToolUseInput) (PreToolUseResult, error) {
	d, err := e.dispatch(ctx, in)
	r := PreToolUseResult{Notices: d.Notices, Control: d.Control}
	if err != nil || r.StopProcessing {
		return r, err
	}
	// 先归并权限。被拒绝或延后的工具不会执行，
	// 因此不返回不会应用的输入改写。
	for _, out := range d.effects {
		if permissionRank(out.pre.permission) > permissionRank(r.Permission) {
			r.Permission = out.pre.permission
		}
	}
	for _, out := range d.effects {
		if out.pre.permission == r.Permission {
			r.Reasons = appendNonempty(r.Reasons, out.pre.reason)
		}
	}
	if r.Permission != Defer {
		r.AdditionalContext = collectContexts(d.effects)
	}
	if r.Permission != Deny && r.Permission != Defer {
		r.UpdatedInput = d.mergeToolInput()
	}
	return r, nil
}

// PostToolUse 归并反馈、附加上下文与工具输出改写。
func (e *Engine) PostToolUse(ctx context.Context, in PostToolUseInput) (PostToolUseResult, error) {
	d, err := e.dispatch(ctx, in)
	r := PostToolUseResult{Notices: d.Notices, Control: d.Control}
	if err != nil || r.StopProcessing {
		return r, err
	}
	r.Feedback = collectFeedback(d.effects)
	r.AdditionalContext = collectContexts(d.effects)
	r.UpdatedToolOutput = d.mergeToolOutput()
	return r, nil
}

// PostToolUseFailure 汇集失败事件的反馈和附加上下文。
func (e *Engine) PostToolUseFailure(ctx context.Context, in PostToolUseFailureInput) (PostToolUseFailureResult, error) {
	d, err := e.dispatch(ctx, in)
	r := PostToolUseFailureResult{Notices: d.Notices, Control: d.Control}
	if err == nil && !r.StopProcessing {
		r.Feedback = collectFeedback(d.effects)
		r.AdditionalContext = collectContexts(d.effects)
	}
	return r, err
}

// PostToolBatch 归并批量工具执行后的循环停止请求。
func (e *Engine) PostToolBatch(ctx context.Context, in PostToolBatchInput) (PostToolBatchResult, error) {
	d, err := e.dispatch(ctx, in)
	r := PostToolBatchResult{Notices: d.Notices, Control: d.Control}
	if err != nil || r.StopProcessing {
		return r, err
	}
	for _, out := range d.effects {
		if out.decision.block {
			r.StopLoop = true
			r.Reasons = appendNonempty(r.Reasons, out.decision.reason)
		}
	}
	r.AdditionalContext = collectContexts(d.effects)
	return r, nil
}

// Stop 归并继续当前轮次的请求，由调用方应用结果。
func (e *Engine) Stop(ctx context.Context, in StopInput) (StopResult, error) {
	d, err := e.dispatch(ctx, in)
	r := StopResult{Notices: d.Notices, Control: d.Control}
	if err != nil || r.StopProcessing {
		return r, err
	}
	r.AdditionalContext = collectContexts(d.effects)
	r.ContinueTurn = len(r.AdditionalContext) > 0
	for _, out := range d.effects {
		if out.decision.block {
			r.ContinueTurn = true
			r.Reasons = appendNonempty(r.Reasons, out.decision.reason)
		}
	}
	return r, nil
}

// SessionEnd 在会话结束预算内执行钩子，只返回执行通知。
func (e *Engine) SessionEnd(ctx context.Context, in SessionEndInput) (SessionEndResult, error) {
	d, err := e.dispatch(ctx, in)
	return SessionEndResult{Notices: d.Notices}, err
}

// dispatch 统一验证输入、选择钩子、执行并归并公共控制；错误时保留通知。
func (e *Engine) dispatch(ctx context.Context, input any) (dispatchResult, error) {
	if ctx == nil {
		return dispatchResult{}, fmt.Errorf("%w: nil context", ErrInput)
	}
	if err := ctx.Err(); err != nil {
		return dispatchResult{}, err
	}
	if e == nil {
		return dispatchResult{}, fmt.Errorf("%w: nil Engine", ErrInput)
	}
	event, err := prepareEvent(input, e.runner.options.InputBytes)
	if err != nil {
		return dispatchResult{}, err
	}
	selected := e.selectHooks(event)
	runCtx := ctx
	if event.name == SessionEnd {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, e.endBudget)
		defer cancel()
	}
	reports, effects, failures := e.execute(runCtx, event, selected)
	d := dispatchResult{Notices: Notices{Reports: reports}, effects: effects}
	for i := range reports {
		d.SystemMessages = appendNonempty(d.SystemMessages, effects[i].system)
		d.StopProcessing = d.StopProcessing || effects[i].control.StopProcessing
		d.StopReasons = append(d.StopReasons, effects[i].control.StopReasons...)
	}
	if err := ctx.Err(); err != nil {
		return dispatchResult{Notices: d.Notices}, err
	}
	if err := errors.Join(failures...); err != nil {
		return dispatchResult{Notices: d.Notices}, err
	}
	return d, nil
}

// selectHooks 按配置顺序匹配事件，并对设置来源的重复钩子去重。
func (e *Engine) selectHooks(event preparedEvent) []compiledHook {
	selected := make([]compiledHook, 0, len(e.hooks[event.name]))
	seen := map[string]bool{}
	for _, h := range e.hooks[event.name] {
		if h.matcher != nil && !h.matcher.MatchString(event.match) {
			continue
		}
		if h.source.Kind == SettingsSource {
			if seen[h.dedupKey] {
				continue
			}
			seen[h.dedupKey] = true
		}
		selected = append(selected, h)
	}
	return selected
}

// execute 并发运行已选钩子，按原始索引写回结果，保证返回顺序稳定。
func (e *Engine) execute(ctx context.Context, event preparedEvent, selected []compiledHook) ([]Report, []effect, []error) {
	reports := make([]Report, len(selected))
	effects := make([]effect, len(selected))
	failures := make([]error, len(selected))
	var wg sync.WaitGroup
	for i, h := range selected {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report := Report{Source: h.source.ID, HookID: h.id, Event: event.name, Command: h.handler.Command, StatusMessage: h.handler.StatusMessage}
			command, buildErr := buildCommand(h, event.common, event.raw)
			if buildErr != nil {
				report.Execution = Execution{Status: ProcessStartFailed, ExitCode: -1, Error: buildErr}
				failures[i] = buildErr
			} else {
				if event.name == SessionEnd {
					command.Timeout = min(command.Timeout, e.endBudget)
				}
				report.Execution = e.runner.Run(ctx, command)
			}
			out, interpretationErr := interpret(event.name, event.match, report.Execution, event.mcp)
			report.Error = interpretationErr
			if errors.Is(interpretationErr, ErrUnsupported) {
				failures[i] = interpretationErr
			}
			reports[i] = report
			effects[i] = out
		}()
	}
	wg.Wait()
	return reports, effects, failures
}

// mergeToolInput 按配置顺序采用最后一份明确提供的合法输入，整体替换。
func (d *dispatchResult) mergeToolInput() *json.RawMessage {
	var result *json.RawMessage
	for _, out := range d.effects {
		if out.pre.input != nil {
			result = out.pre.input
		}
	}
	return result
}

// mergeToolOutput 按配置顺序采用最后一份明确提供的合法输出。
// 同一钩子同时提供两个输出字段时，MCP 专用字段优先；JSON null 也是明确提供。
func (d *dispatchResult) mergeToolOutput() *json.RawMessage {
	var result *json.RawMessage
	for _, out := range d.effects {
		if out.post.output != nil {
			result = out.post.output
		}
		if out.post.mcpOutput != nil {
			result = out.post.mcpOutput
		}
	}
	return result
}

func permissionRank(p Permission) int {
	switch p {
	case Deny:
		return 4
	case Defer:
		return 3
	case Ask:
		return 2
	case Allow:
		return 1
	}
	return 0
}

func collectContexts(effects []effect) []string {
	var result []string
	for _, out := range effects {
		result = appendNonempty(result, out.context)
	}
	return result
}

func collectFeedback(effects []effect) []string {
	var result []string
	for _, out := range effects {
		result = appendNonempty(result, out.post.feedback)
	}
	return result
}

func appendNonempty(values []string, value string) []string {
	if value != "" {
		return append(values, value)
	}
	return values
}
