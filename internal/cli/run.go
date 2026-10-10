package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/permission"
	"CodeAgent/internal/runtime"
)

type approvalItem struct {
	runtime   *runtime.Runtime
	sessionID string
	approval  runtime.Approval
}

func (a *application) pending() []approvalItem {
	var items []approvalItem
	for _, r := range a.host.Opened() {
		for _, p := range r.Snapshot().Approvals {
			items = append(items, approvalItem{r, r.SessionID(), p})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].sessionID != items[j].sessionID {
			return items[i].sessionID < items[j].sessionID
		}
		return items[i].approval.ID < items[j].approval.ID
	})
	return items
}

type waitResult struct {
	result agent.RunResult
	err    error
}

func (a *application) run(parent context.Context, text string) RunOutput {
	var ctx context.Context
	var cancel context.CancelFunc
	if a.invocation.timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, a.invocation.timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	defer cancel()
	out := RunOutput{SessionID: a.current.SessionID(), State: agent.RunStateError, ExitCode: ExitFailure, MemoryShutdown: a.capabilities.MemoryShutdown}
	handle, err := a.current.Submit(ctx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: text}}, Timestamp: time.Now()})
	if err != nil {
		out.Error = a.safe(err.Error())
		if c := contextCode(ctx); c != ExitOK {
			out.ExitCode = c
			out.State = agent.RunStateAborted
		}
		return out
	}
	out.RunID = handle.ID()
	waitCtx, waitCancel := context.WithCancel(context.Background())
	defer waitCancel()
	finished := make(chan waitResult, 1)
	go func() { r, e := handle.Wait(waitCtx); finished <- waitResult{r, e} }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var prompt *approvalItem
	var answer <-chan inputResult
	answered := map[string]bool{}
	cause := ExitOK
	collect := func(w waitResult) {
		if w.result.State != "" {
			out.State = w.result.State
			out.Text = finalText(w.result.Messages)
			out.Error = w.result.ErrorMessage
			out.StopReason = w.result.StopReason
			out.Steps = w.result.Steps
			out.Usage = w.result.TotalUsage
		}
		if w.err != nil {
			out.Error = w.err.Error()
		}
		switch out.State {
		case agent.RunStateSuccess:
			out.ExitCode = ExitOK
		case agent.RunStateAborted:
			out.ExitCode = ExitCancelled
		case agent.RunStateDeferred:
			out.ExitCode = ExitDeferred
		default:
			out.ExitCode = ExitFailure
		}
		if cause != ExitOK {
			out.ExitCode = cause
			switch cause {
			case ExitApprovalRequired:
				out.Error = "approval_required: interactive approval is unavailable"
			case ExitTimeout:
				out.Error = "run timed out"
			case ExitCancelled:
				out.Error = "run cancelled"
			}
		}
	}
	for {
		// Snapshot is authoritative for all opened main/child sessions. A timer also
		// recovers approvals if every observer hint was lost or coalesced.
		pending := a.pending()
		present := map[string]bool{}
		unanswered := pending[:0]
		for _, item := range pending {
			present[item.approval.ID] = true
			if !answered[item.approval.ID] {
				unanswered = append(unanswered, item)
			}
		}
		for id := range answered {
			if !present[id] {
				delete(answered, id)
			}
		}
		pending = unanswered
		if len(pending) > 0 && prompt == nil {
			if !a.streams.Interactive || a.invocation.stdin {
				cause = ExitApprovalRequired
				break
			}
			item := pending[0]
			prompt = &item
			fmt.Fprintf(a.streams.Err, "Approval session=%s run=%s tool=%s hook_ask=%t reason=%s\n%s\nTargets: %v\n[once/session/reject] > ", item.sessionID, item.approval.RunID, item.approval.ToolName, item.approval.HookAsk, item.approval.Reason, a.safe(item.approval.Arguments), item.approval.FileTargets)
			if a.input == nil {
				a.input = newInput(a.streams)
			}
			answer = a.input.request(false)
		}
		select {
		case w := <-finished:
			collect(w)
			a.drainNotices()
			return out
		case <-ctx.Done():
			cause = contextCode(ctx)
		case <-a.hint:
			a.drainNotices()
			continue
		case <-ticker.C:
			a.drainNotices()
			continue
		case line := <-answer:
			a.input.consumed()
			answer = nil
			if line.err != nil {
				if !errors.Is(line.err, io.EOF) {
					out.Error = line.err.Error()
				}
				cause = ExitCancelled
				break
			}
			value := strings.TrimSpace(line.text)
			switch value {
			case "/exit":
				cause = ExitCancelled
			case "/help":
				fmt.Fprint(a.streams.Err, chatHelp())
				answer = a.input.request(false)
				continue
			case "/status":
				_ = a.writeStatus(a.streams.Err)
				answer = a.input.request(false)
				continue
			default:
				choices := map[string]permission.Choice{"once": permission.Once, "session": permission.SessionTool, "reject": permission.Reject}
				choice, ok := choices[value]
				if !ok {
					fmt.Fprintln(a.streams.Err, "enter once, session, reject, /status, /help, or /exit")
					answer = a.input.request(false)
					continue
				}
				if err = prompt.runtime.Approve(prompt.approval.ID, choice); err != nil && !errors.Is(err, runtime.ErrStaleApproval) {
					fmt.Fprintln(a.streams.Err, a.safe(err.Error()))
				} else {
					answered[prompt.approval.ID] = true
				}
				prompt = nil
				continue
			}
		}
		break
	}
	handle.Cancel()
	cancel()
	cleanup, stop := a.cleanupContext()
	defer stop()
	result, err := handle.Wait(cleanup)
	collect(waitResult{result, err})
	if result.State == "" {
		out.State = agent.RunStateAborted
	}
	return out
}
