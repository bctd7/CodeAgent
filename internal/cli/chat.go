package cli

import (
	"CodeAgent/internal/runtime"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

func (a *application) chat(ctx context.Context) int {
	a.input = newInput(a.streams)
	code := ExitOK
	if a.capabilities.Memory {
		fmt.Fprintln(a.streams.Err, a.capabilities.MemoryShutdown)
	}
	for {
		if a.streams.Interactive && !a.invocation.stdin {
			fmt.Fprint(a.streams.Err, "> ")
		}
		text, err := a.input.read(ctx, false)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return code
			}
			if c := contextCode(ctx); c != ExitOK {
				return c
			}
			fmt.Fprintln(a.streams.Err, err)
			return ExitFailure
		}
		a.drainNotices()
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "/") {
			result, commandErr := a.command(ctx, text)
			if commandErr != nil {
				fmt.Fprintln(a.streams.Err, a.safe(commandErr.Error()))
				continue
			}
			if result.exit {
				return code
			}
			if result.cancel {
				fmt.Fprintln(a.streams.Err, "没有当前活动任务")
				continue
			}
			if result.value != nil {
				if text == "/model" && !a.invocation.json {
					err = a.writeModels()
				} else {
					err = a.writeValue(result.value)
				}
				if err != nil {
					return ExitFailure
				}
			}
			if result.prompt != "" {
				out := a.run(ctx, result.prompt)
				code = out.ExitCode
				if err = a.writeResult(out); err != nil {
					return ExitFailure
				}
				if code == ExitApprovalRequired || code == ExitCancelled || code == ExitTimeout || a.current.Snapshot().Fault != nil {
					return code
				}
			}
			continue
		}
		out := a.run(ctx, text)
		code = out.ExitCode
		if err = a.writeResult(out); err != nil {
			fmt.Fprintln(a.streams.Err, err)
			return ExitFailure
		}
		if code == ExitApprovalRequired || code == ExitCancelled || code == ExitTimeout || a.current.Snapshot().Fault != nil {
			return code
		}
	}
}
func (a *application) writeModels() error {
	if a.invocation.json {
		return a.writeValue(struct {
			Current string   `json:"current"`
			Models  []string `json:"models"`
		}{a.selectedRef, append([]string{}, a.modelRefs...)})
	}
	if _, err := fmt.Fprintf(a.streams.Out, "Models (current: %s):\n", a.safe(a.selectedRef)); err != nil {
		return err
	}
	for _, ref := range a.modelRefs {
		marker := " "
		if ref == a.selectedRef {
			marker = "*"
		}
		if _, err := fmt.Fprintf(a.streams.Out, "%s %s\n", marker, a.safe(ref)); err != nil {
			return err
		}
	}
	return nil
}
func (a *application) status() Status {
	snap := a.current.Snapshot()
	m := snap.Agent.CurrentModel
	out := Status{SessionID: snap.SessionID, State: snap.Agent.Status, RunID: snap.Agent.CurrentRunID, QueuedRunIDs: append([]string{}, snap.Agent.QueuedRunIDs...), Closing: snap.Closing, Model: ModelInfo{DisplayName: displayModelName(a.selectedRef, m.ID, m.DisplayName), Provider: m.Provider, ID: m.ID, API: m.API, ContextWindow: m.ContextWindow}, Approvals: []runtime.Approval{}, Capabilities: a.capabilities}
	// Approvals include child sessions too, each carrying its globally unique RunID.
	for _, item := range a.pending() {
		out.Approvals = append(out.Approvals, item.approval)
	}
	out.Model.Ref = a.selectedRef
	if snap.Fault != nil {
		out.Error = a.safe(snap.Fault.Error())
	}
	if a.manager != nil {
		out.Servers = a.manager.Statuses()
	}
	return out
}
func (a *application) writeStatus(out io.Writer) error {
	return json.NewEncoder(out).Encode(a.status())
}
func (a *application) writeValue(value any) error {
	if a.invocation.json {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.streams.Out, a.safe(string(data)))
		return err
	}
	if s, ok := value.(string); ok {
		_, err := fmt.Fprint(a.streams.Out, s)
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.streams.Out, a.safe(string(data)))
	return err
}

// Keep the configured REF as identity; labels are presentation metadata only.
func displayModelName(ref, id, name string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	if ref != "" {
		return ref
	}
	return id
}
func (a *application) modelInfo(ref string) ModelInfo {
	for _, entry := range a.entries {
		if entry.Ref == ref {
			m := entry.Model
			return ModelInfo{Ref: ref, DisplayName: displayModelName(ref, m.ID, m.DisplayName), Provider: m.Provider, ID: m.ID, API: m.API, ContextWindow: m.ContextWindow}
		}
	}
	return ModelInfo{Ref: ref, DisplayName: ref}
}
