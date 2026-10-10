package runtime

import (
	"CodeAgent/internal/hooks"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/permission"
	sc "CodeAgent/internal/session/config"
	"CodeAgent/internal/session/store"
	"CodeAgent/internal/tools/extend/subagent"
)

func productHost(t *testing.T, subagents bool) *Host {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	db, e := store.Open(ctx, filepath.Join(root, "test.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	if e = db.Init(ctx); e != nil {
		t.Fatal(e)
	}
	h, e := NewHost(ctx, Options{Store: db, CloseStore: true, OwnershipDir: filepath.Join(root, "owners"), ProjectDir: root, Models: map[string]ModelConfig{"fake/main": {Model: llm.Model{ID: "fake", Provider: "fake", API: llm.APIKindOpenAI, ContextWindow: 8192, MaxTokens: 1024}}}, ModelRef: "fake/main", MemoryDirectory: filepath.Join(root, "memory"), MemoryScope: "project:test", Subagents: subagents, Stream: func(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if e := h.Close(ctx); e != nil {
			t.Error(e)
		}
	})
	return h
}
func TestProductMemoryPerSessionAndRestore(t *testing.T) {
	h := productHost(t, false)
	ctx := context.Background()
	on, e := h.Create(ctx, "on", true)
	if e != nil {
		t.Fatal(e)
	}
	off, e := h.Create(ctx, "off", false)
	if e != nil {
		t.Fatal(e)
	}
	a, e := h.Open(ctx, on.ID)
	if e != nil {
		t.Fatal(e)
	}
	b, e := h.Open(ctx, off.ID)
	if e != nil {
		t.Fatal(e)
	}
	if a.memory == nil || b.memory != nil || !a.memoryEnabled || b.memoryEnabled {
		t.Fatal("per-session gate failed")
	}
	catalog, e := b.ToolCatalog()
	if e != nil {
		t.Fatal(e)
	}
	for _, tool := range catalog {
		if tool.Identity.SourceID == "project-memory" {
			t.Fatal("disabled memory tool exposed")
		}
	}
	if e = b.Close(ctx); e != nil {
		t.Fatal(e)
	}
	b, e = h.Open(ctx, off.ID)
	if e != nil {
		t.Fatal(e)
	}
	if b.memory != nil {
		t.Fatal("restore enabled memory")
	}
	if h.options.MemoryDirectory == "" || a.memory == nil {
		t.Fatal("shared Host configuration cleared")
	}
}
func TestProductProjectPolicyAndCatalogCopy(t *testing.T) {
	h := productHost(t, false)
	ctx := context.Background()
	x, _ := h.Create(ctx, "a", true)
	a, e := h.Open(ctx, x.ID)
	if e != nil {
		t.Fatal(e)
	}
	y, _ := h.Create(ctx, "explore", true)
	b, e := h.open(ctx, y.ID, "explore", nil)
	if e != nil {
		t.Fatal(e)
	}
	p := permission.Policy{WorkingDir: h.options.ProjectDir, WorkMode: permission.Build, ApprovalMode: permission.AutoEdit, DeniedTools: []permission.Tool{{Source: permission.Builtin, SourceID: "builtin", ToolID: "edit"}}}
	if e = a.SetPolicy(ctx, p); e != nil {
		t.Fatal(e)
	}
	p.DeniedTools[0].ToolID = "changed"
	if a.policy.ApprovalMode != permission.AutoEdit || b.policy.WorkMode != permission.Plan || b.policy.DeniedTools[0].ToolID != "edit" {
		t.Fatal("project policy or role restriction lost")
	}
	z, _ := h.Create(ctx, "future", true)
	c, e := h.Open(ctx, z.ID)
	if e != nil {
		t.Fatal(e)
	}
	if c.policy.ApprovalMode != permission.AutoEdit {
		t.Fatal("future Runtime inherited stale policy")
	}
	first, e := a.ToolCatalog()
	if e != nil || len(first) == 0 {
		t.Fatal(e)
	}
	name := first[0].Name
	first[0].Name = "mutated"
	if len(first[0].Parameters) > 0 {
		first[0].Parameters[0] = '!'
	}
	again, e := a.ToolCatalog()
	if e != nil || again[0].Name != name || again[0].Parameters[0] == '!' {
		t.Fatal("catalog shares mutable data")
	}
}
func TestProductDisabledMemoryChildAndBusyPolicy(t *testing.T) {
	h := productHost(t, true)
	ctx := context.Background()
	info, _ := h.Create(ctx, "off", false)
	r, e := h.Open(ctx, info.ID)
	if e != nil {
		t.Fatal(e)
	}
	run, e := r.Submit(ctx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "fake task"}}, Timestamp: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs, e := r.session.ListUnfinishedRuns(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if len(runs) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	p := r.policy
	p.ApprovalMode = permission.AutoApprove
	if e = r.SetPolicy(ctx, p); e == nil {
		t.Fatalf("busy policy: %v", e)
	}
	task, e := r.Spawn(ctx, subagent.SpawnRequest{Role: subagent.Explore, Task: "fake child"})
	if e != nil {
		t.Fatal(e)
	}
	child, e := h.service.GetSession(ctx, task.SessionID)
	if e != nil || child.MemoryEnabled {
		t.Fatal("disabled parent did not propagate memory gate", e)
	}
	if _, e = r.Cancel(ctx, subagent.CancelRequest{ID: task.ID}); e != nil {
		t.Fatal(e)
	}
	r.CancelRun(run)
}

func TestProductPolicyConcurrentOpenCreateSubmitAndFailureAtomicity(t *testing.T) {
	h := productHost(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	x, _ := h.Create(ctx, "a", false)
	a, e := h.Open(ctx, x.ID)
	if e != nil {
		t.Fatal(e)
	}
	y, _ := h.Create(ctx, "b", false)
	b, e := h.Open(ctx, y.ID)
	if e != nil {
		t.Fatal(e)
	}
	old := a.policy
	p := old
	p.ApprovalMode = permission.AutoEdit
	a.operation.Lock()
	policyDone := make(chan error, 1)
	go func() { policyDone <- a.SetPolicy(ctx, p) }()
	for {
		h.mu.Lock()
		changing := h.catalogChanging
		h.mu.Unlock()
		if changing {
			break
		}
		if ctx.Err() != nil {
			a.operation.Unlock()
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	z, e := h.Create(ctx, "created during policy boundary", false)
	if e != nil {
		a.operation.Unlock()
		t.Fatal(e)
	}
	if _, e = h.Open(ctx, z.ID); e != ErrBusy {
		a.operation.Unlock()
		t.Fatalf("Open admitted during policy boundary: %v", e)
	}
	submitted := make(chan *agent.RunHandle, 1)
	submitError := make(chan error, 1)
	go func() {
		handle, e := a.Submit(ctx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "fake"}}, Timestamp: time.Now()})
		submitted <- handle
		submitError <- e
	}()
	a.operation.Unlock()
	policyError := <-policyDone
	handle := <-submitted
	if e := <-submitError; e != nil {
		t.Fatal(e)
	}
	expected := p.ApprovalMode
	if policyError != nil {
		expected = old.ApprovalMode
	}
	if a.policy.ApprovalMode != expected || b.policy.ApprovalMode != expected || h.options.Policy.ApprovalMode != expected {
		t.Fatal("partial project policy application")
	}
	a.CancelRun(handle)
	handle.Wait(ctx)
	c, e := h.Open(ctx, z.ID)
	if e != nil {
		t.Fatal(e)
	}
	if c.policy.ApprovalMode != expected {
		t.Fatal("future Open observed stale policy")
	}
	// A different busy Runtime must reject the entire update before any assignment.
	busy, e := b.Submit(ctx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "busy sibling"}}, Timestamp: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	next := a.policy
	next.ApprovalMode = permission.AutoApprove
	if e = a.SetPolicy(ctx, next); e == nil {
		t.Fatal("busy sibling accepted policy")
	}
	if a.policy.ApprovalMode != expected || h.options.Policy.ApprovalMode != expected || c.policy.ApprovalMode != expected {
		t.Fatal("failure partly applied policy")
	}
	b.CancelRun(busy)
}

func TestProductToolCatalogNestedSchemaDetached(t *testing.T) {
	h := productHost(t, false)
	ctx := context.Background()
	info, _ := h.Create(ctx, "schema", false)
	r, e := h.Open(ctx, info.ID)
	if e != nil {
		t.Fatal(e)
	}
	first, e := r.ToolCatalog()
	if e != nil || len(first) == 0 {
		t.Fatal(e)
	}
	var schema map[string]any
	if e = json.Unmarshal(first[0].Parameters, &schema); e != nil {
		t.Fatal(e)
	}
	schema["properties"].(map[string]any)["detached_marker"] = map[string]any{"nested": []any{"mutated"}}
	data, e := json.Marshal(schema)
	if e != nil {
		t.Fatal(e)
	}
	first[0].Parameters = data
	again, e := r.ToolCatalog()
	if e != nil || bytes.Contains(again[0].Parameters, []byte("detached_marker")) {
		t.Fatal("nested schema mutation reached Runtime", e)
	}
}

func TestProductCancelledPolicyDoesNotApply(t *testing.T) {
	h := productHost(t, false)
	ctx := context.Background()
	info, _ := h.Create(ctx, "cancel", false)
	r, e := h.Open(ctx, info.ID)
	if e != nil {
		t.Fatal(e)
	}
	old := r.policy
	next := old
	next.ApprovalMode = permission.AutoApprove
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	if e = r.SetPolicy(stopped, next); e == nil {
		t.Fatal("cancelled policy accepted")
	}
	if r.policy.ApprovalMode != old.ApprovalMode || h.options.Policy.ApprovalMode != old.ApprovalMode {
		t.Fatal("cancelled policy partly applied")
	}
}

// Exercise the actual registered controls through the unchanged approval boundary.
func TestProductOrchestrationPermissionBoundaries(t *testing.T) {
	h := productHost(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := h.Create(ctx, "controls", false)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Open(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"spawn_agent", "send_input", "wait", "cancel_agent"} {
		binding := r.bindings[name]
		if binding == nil || binding.Capability != permission.Orchestration {
			t.Fatalf("%s not classified as orchestration", name)
		}
		for _, mode := range []permission.ApprovalMode{permission.Default, permission.AutoEdit, permission.AutoApprove} {
			r.policy.ApprovalMode = mode
			allowed, reason, err := r.awaitPermission(agent.Checkpoint{Context: ctx}, binding, map[string]any{}, nil, false)
			if err != nil || !allowed {
				t.Fatalf("%s/%s: allowed=%v reason=%s err=%v", name, mode, allowed, reason, err)
			}
		}
		request := permission.Request{RequestID: name, SessionID: r.id, Tool: binding.Identity, Capability: binding.Capability}
		p := r.policy
		p.WorkMode = permission.Plan
		if got := permission.Evaluate(p, request); got.Decision != permission.Deny || got.Reason != permission.PlanRestriction {
			t.Fatalf("Plan %s: %+v", name, got)
		}
		p.WorkMode = permission.Build
		p.DeniedTools = []permission.Tool{binding.Identity}
		if got := permission.Evaluate(p, request); got.Decision != permission.Deny || got.Reason != permission.ExplicitDeny {
			t.Fatalf("deny %s: %+v", name, got)
		}
		p.DeniedTools = nil
		request.Tool.Source = permission.External
		if got := permission.Evaluate(p, request); got.Decision != permission.Ask || got.Reason != permission.ExternalConfirmation {
			t.Fatalf("external %s: %+v", name, got)
		}
	}
	// Permission knows the category, not any subagent source or tool spelling.
	arbitrary := permission.Request{RequestID: "arbitrary", SessionID: r.id, Tool: permission.Tool{Source: permission.Builtin, SourceID: "other-trusted-host", ToolID: "other-control"}, Capability: permission.Orchestration}
	if got := permission.Evaluate(r.policy, arbitrary); got.Decision != permission.Allow || got.Reason != permission.BuiltinOrchestration {
		t.Fatalf("category rule: %+v", got)
	}
	for _, name := range []string{"read", "find", "grep", "ls"} {
		if r.bindings[name].Capability != permission.ReadOnly {
			t.Fatalf("changed %s classification", name)
		}
	}
	if r.bindings["write"].Capability != permission.FileWrite || r.bindings["edit"].Capability != permission.FileWrite {
		t.Fatal("file capability changed")
	}
	for _, binding := range r.bindings {
		if binding.Identity.SourceID == "builtin" && binding.Capability == permission.Orchestration {
			t.Fatal("base tool reclassified")
		}
	}
}

func productPendingApproval(t *testing.T, r *Runtime, ctx context.Context) Approval {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		r.mu.Lock()
		for _, p := range r.approvals {
			view := p.view()
			r.mu.Unlock()
			return view
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			t.Fatal("approval not produced", ctx.Err())
		case <-ticker.C:
		}
	}
}

// The test binary is a tiny deterministic command Hook; no shell or model API.
func TestProductOrchestrationHookProcess(t *testing.T) {
	if len(os.Args) < 2 {
		return
	}
	choice := os.Args[len(os.Args)-1]
	if choice != "orchestration-ask" && choice != "orchestration-deny" {
		return
	}
	decision := "ask"
	if choice == "orchestration-deny" {
		decision = "deny"
	}
	fmt.Printf(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":%q,"permissionDecisionReason":"test control"}}`, decision)
	os.Exit(0)
}

func TestProductOrchestrationHookStillControls(t *testing.T) {
	h := productHost(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	info, err := h.Create(ctx, "hooks", false)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Open(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	call := agent.Checkpoint{Context: ctx, RunID: "hook-test", ToolCall: &llm.ContentBlock{Type: llm.TypeToolCall, ID: "wait-test", Name: "wait", Arguments: map[string]any{"ids": []any{"owned-placeholder"}, "mode": "all", "timeout_ms": 1}}}
	for _, choice := range []string{"orchestration-deny", "orchestration-ask"} {
		document, _ := json.Marshal(map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{"matcher": "wait", "hooks": []any{map[string]any{"type": "command", "command": executable, "args": []string{"-test.run=^TestProductOrchestrationHookProcess$", "--", choice}, "timeout": 3}}}}}})
		r.hooks, err = hooks.New([]hooks.Source{{ID: "test", JSON: document}}, hooks.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if choice == "orchestration-deny" {
			got := r.prepareTool(call, "unused")
			if got.Kind != agent.Skip || got.Disposition != agent.ToolDenied {
				t.Fatalf("Hook deny: %+v", got)
			}
			continue
		}
		done := make(chan agent.Decision, 1)
		go func() { done <- r.prepareTool(call, "unused") }()
		approval := productPendingApproval(t, r, ctx)
		if !approval.HookAsk || approval.Reason != permission.BuiltinOrchestration {
			t.Fatalf("Hook Ask missing: %+v", approval)
		}
		if err := r.Approve(approval.ID, permission.Reject); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-done:
			if got.Kind != agent.Skip || got.Disposition != agent.ToolDenied {
				t.Fatalf("Hook rejected: %+v", got)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	r.hooks = nil
}

func TestProductOrchestrationChildWriteRemainsIndependent(t *testing.T) {
	h := productHost(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	info, err := h.Create(ctx, "parent", false)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Open(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	identity := r.bindings["write"].Identity
	request := permission.Request{RequestID: "parent-write", SessionID: r.id, Tool: identity, Capability: permission.FileWrite, Arguments: `{"path":"allowed.txt","content":"test"}`, FileTargets: []string{filepath.Join(h.options.ProjectDir, "allowed.txt")}}
	result := r.permissions.Evaluate(r.policy, request)
	if result.Confirmation == nil {
		t.Fatal("parent write not asking")
	}
	if err := r.permissions.Grant(*result.Confirmation, permission.SessionTool); err != nil {
		t.Fatal(err)
	}
	if got := r.permissions.Evaluate(r.policy, request); got.Decision != permission.Allow {
		t.Fatal("parent grant failed")
	}
	run, err := r.Submit(ctx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "fake parent"}}, Timestamp: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.CancelRun(run)
	// Wait for the fake model so Spawn sees an active persisted parent run.
	for {
		runs, err := r.session.ListUnfinishedRuns(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) > 0 && r.agent.Snapshot().Status == agent.AgentStatusRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	task, err := r.Spawn(ctx, subagent.SpawnRequest{Role: subagent.Worker, Task: "fake worker", FileScope: []string{"allowed.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Cancel(context.Background(), subagent.CancelRequest{ID: task.ID})
	child, err := r.child(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker := child.runtime
	if worker.permissions == r.permissions {
		t.Fatal("permission container shared")
	}
	binding := worker.bindings["write"]
	paths, err := worker.targets(binding, map[string]any{"path": "allowed.txt"})
	if err != nil || len(paths) != 1 {
		t.Fatal("allowed worker scope", err)
	}
	if _, err := worker.targets(binding, map[string]any{"path": "outside.txt"}); err == nil {
		t.Fatal("worker scope bypassed")
	}
	done := make(chan bool, 1)
	go func() {
		allowed, _, err := worker.awaitPermission(agent.Checkpoint{Context: ctx}, binding, map[string]any{"path": "allowed.txt", "content": "test"}, paths, false)
		done <- allowed && err == nil
	}()
	approval := productPendingApproval(t, worker, ctx)
	if approval.Reason != permission.WriteConfirmation || approval.HookAsk {
		t.Fatalf("child inherited parent grant or skipped file rule: %+v", approval)
	}
	if err := worker.Approve(approval.ID, permission.Reject); err != nil {
		t.Fatal(err)
	}
	select {
	case allowed := <-done:
		if allowed {
			t.Fatal("rejected worker write allowed")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, b := range worker.bindings {
		if b.Capability == permission.Orchestration || b.Capability == permission.Shell || b.Identity.Source == permission.External {
			t.Fatal("child received unsupported control/shell/MCP")
		}
	}
}

func TestProductChildDeliveryModelFlow(t *testing.T) {
	for _, tc := range []struct {
		name            string
		calls, contexts int
	}{{"wait", 3, 0}, {"automatic", 3, 1}, {"timeout", 4, 1}, {"restart", 5, 0}, {"any", 4, 1}, {"failed", 3, 0}, {"cancelled", 3, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			h := productHost(t, true)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var mu sync.Mutex
			parentCalls, childCalls := 0, 0
			leaked := false
			var taskID string
			seenRuns := map[string]bool{}
			var taskIDs []any
			release := make(chan struct{})
			released := false
			h.options.Stream = func(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
				mu.Lock()
				defer mu.Unlock()
				isChild, slowChild := false, false
				for _, m := range req.Messages {
					if u, ok := m.(*llm.UserMessage); ok {
						for _, b := range u.Content {
							if strings.HasPrefix(b.Text, "child:test") {
								isChild = true
								slowChild = strings.Contains(b.Text, "slow")
							}
						}
					}
				}
				message := llm.AssistantMessage{Role: llm.RoleAssistant, Provider: "fake", API: llm.APIKindOpenAI, Model: "fake", State: llm.StateSuccess, StopReason: llm.StopStop, Timestamp: time.Now()}
				if isChild {
					childCalls++
					message.Content = []llm.ContentBlock{{Type: llm.TypeThinking, Thinking: "private-child-thinking", ThinkingSignature: "private-thinking-signature"}, {Type: llm.TypeText, Text: fmt.Sprintf("public-result-%d", childCalls), TextSignature: "private-text-signature"}}
					reader, writer := llm.NewEventStream(ctx, message)
					if tc.name == "timeout" || tc.name == "any" && slowChild {
						go func() {
							select {
							case <-release:
								writer.Finish(message)
							case <-ctx.Done():
							}
						}()
					} else if tc.name == "failed" {
						message.State = llm.StateError
						message.StopReason = ""
						message.ErrorMessage = "fake child failed"
						writer.Fail(message, nil)
					} else if tc.name == "cancelled" {
						writer.Abort(context.Canceled)
					} else {
						writer.Finish(message)
					}
					return reader, nil
				}
				parentCalls++
				taskIDs = nil
				for _, m := range req.Messages {
					var content []llm.ContentBlock
					switch v := m.(type) {
					case *llm.UserMessage:
						content = v.Content
					case *llm.ToolResultMessage:
						content = v.Content
						if v.ToolName == "wait" {
							for _, b := range v.Content {
								var value struct {
									Tasks []struct {
										RunID string `json:"run_id"`
										State string `json:"state"`
									} `json:"tasks"`
								}
								if json.Unmarshal([]byte(b.Text), &value) == nil {
									for _, task := range value.Tasks {
										if task.State != "running" && task.RunID != "" {
											seenRuns[task.RunID] = true
										}
									}
								}
							}
						}
						if v.ToolName == "spawn_agent" {
							for _, b := range v.Content {
								var value struct {
									ID string `json:"id"`
								}
								if json.Unmarshal([]byte(b.Text), &value) == nil && value.ID != "" {
									taskID = value.ID
									taskIDs = append(taskIDs, value.ID)
								}
							}
						}
					}
					for _, b := range content {
						if strings.Contains(b.Text, "private-child-thinking") || strings.Contains(b.Text, "private-text-signature") || strings.Contains(b.Text, `"Thinking"`) || strings.Contains(b.Text, `"TextSignature"`) || strings.Contains(b.Text, `"Type"`) {
							leaked = true
						}
					}
				}
				call := func(name string, args map[string]any) {
					message.StopReason = llm.StopToolUse
					message.Content = []llm.ContentBlock{{Type: llm.TypeToolCall, ID: fmt.Sprintf("parent-call-%d", parentCalls), Name: name, Arguments: args}}
				}
				switch {
				case parentCalls == 1:
					call("spawn_agent", map[string]any{"role": "explore", "task": "child:test"})
					if tc.name == "any" {
						message.Content = append(message.Content, llm.ContentBlock{Type: llm.TypeToolCall, ID: "slow-spawn", Name: "spawn_agent", Arguments: map[string]any{"role": "explore", "task": "child:test-slow"}})
					}
				case parentCalls == 2 && tc.name != "automatic":
					timeout := 3000
					if tc.name == "timeout" {
						timeout = 1
					}
					mode := "all"
					ids := []any{taskID}
					if tc.name == "any" {
						mode = "any"
						ids = taskIDs
					}
					call("wait", map[string]any{"ids": ids, "mode": mode, "timeout_ms": timeout})
				case parentCalls == 3 && tc.name == "restart":
					call("send_input", map[string]any{"id": taskID, "input": "child:test second run"})
				case parentCalls == 4 && tc.name == "restart":
					call("wait", map[string]any{"ids": []any{taskID}, "mode": "all", "timeout_ms": 3000})
				default:
					if (tc.name == "timeout" || tc.name == "any") && !released {
						close(release)
						released = true
					}
					message.Content = []llm.ContentBlock{{Type: llm.TypeText, Text: "parent final"}}
				}
				reader, writer := llm.NewEventStream(ctx, message)
				writer.Finish(message)
				return reader, nil
			}
			info, err := h.Create(ctx, "flow", false)
			if err != nil {
				t.Fatal(err)
			}
			r, err := h.Open(ctx, info.ID)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := r.Submit(ctx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "parent:test"}}, Timestamp: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			result, err := handle.Wait(ctx)
			if err != nil || result.State != agent.RunStateSuccess {
				t.Fatal("flow failed", result.State, err, result.ErrorMessage)
			}
			mu.Lock()
			calls, secret, children, received := parentCalls, leaked, childCalls, len(seenRuns)
			mu.Unlock()
			if tc.name == "restart" && (children != 2 || received != 2) {
				t.Fatal("new child Run not independently received", children, received)
			}
			if tc.name == "any" && children != 2 {
				t.Fatal("any did not run both children", children)
			}
			if calls != tc.calls {
				t.Errorf("parent model calls=%d want=%d: duplicate or missing child delivery", calls, tc.calls)
			}
			if secret {
				t.Error("internal child thinking/signatures reached parent")
			}
			detail, err := r.session.GetRun(ctx, handle.ID())
			if err != nil {
				t.Fatal(err)
			}
			contexts := 0
			for _, e := range detail.Entries {
				if e.Kind == "context" && e.SourceKind == "child_run" {
					contexts++
				}
			}
			if contexts != tc.contexts {
				t.Errorf("automatic child contexts=%d want=%d", contexts, tc.contexts)
			}
			child, err := r.child(taskID)
			if err != nil && tc.name == "automatic" {
				for _, ch := range r.runChildren(handle.ID()) {
					child = ch
					break
				}
			} else if err != nil {
				t.Fatal(err)
			}
			childHistory, err := child.runtime.session.GetCurrentHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			originalThinking := false
			for _, e := range childHistory {
				if m, ok := e.Message.(*llm.AssistantMessage); ok {
					for _, b := range m.Content {
						if b.Type == llm.TypeThinking && b.Thinking == "private-child-thinking" {
							originalThinking = true
						}
					}
				}
			}
			if !originalThinking {
				t.Fatal("child original history lost")
			}
		})
	}
}

func TestProductChildReceiptRequiresPersistedFinalProjection(t *testing.T) {
	for _, mode := range []string{"pending", "original", "hidden", "replacement", "cancelled-processing"} {
		t.Run(mode, func(t *testing.T) {
			h := productHost(t, true)
			ctx := context.Background()
			info, err := h.Create(ctx, "receipt", false)
			if err != nil {
				t.Fatal(err)
			}
			r, err := h.Open(ctx, info.ID)
			if err != nil {
				t.Fatal(err)
			}
			task := subagent.Task{ID: "task", SessionID: "child-session", RunID: "child-run", ResultRef: "child-session/child-run", State: subagent.Completed, Result: []llm.ContentBlock{{Type: llm.TypeText, Text: "public"}}}
			raw, _ := json.Marshal(struct {
				Tasks    []taskOutput `json:"tasks"`
				TimedOut bool         `json:"timed_out"`
			}{[]taskOutput{taskView(task)}, false})
			user := &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: "fake"}}, Timestamp: time.Now()}
			if _, err = r.session.StartRun(ctx, "parent", user); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"ids": []any{"task"}, "mode": "all", "timeout_ms": 1}
			assistant, err := r.session.AppendMessage(ctx, "parent", &llm.AssistantMessage{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.TypeToolCall, ID: "call", Name: "wait", Arguments: args}}, State: llm.StateSuccess, StopReason: llm.StopToolUse, Timestamp: time.Now()}, &sc.MessageModelOrigin{Provider: "fake", API: llm.APIKindOpenAI, ModelID: "fake"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.session.RecordToolStart(ctx, assistant.ID, "call", args); err != nil {
				t.Fatal(err)
			}
			entry, err := r.session.AppendToolResult(ctx, assistant.ID, sc.ToolResultInput{Result: &llm.ToolResultMessage{Role: llm.RoleTool, ToolCallID: "call", ToolName: "wait", Content: []llm.ContentBlock{{Type: llm.TypeText, Text: string(raw)}}}, Arguments: args, Status: sc.ToolSucceeded, Executed: true})
			if err != nil {
				t.Fatal(err)
			}
			detail, err := r.session.GetRun(ctx, "parent")
			if err != nil {
				t.Fatal(err)
			}
			if r.childResultVisible(detail, task) {
				t.Fatal("pending/raw tool output prematurely consumed delivery")
			}
			var replacement *[]llm.ContentBlock
			if mode == "hidden" {
				empty := []llm.ContentBlock{}
				replacement = &empty
			}
			if mode == "replacement" {
				content := []llm.ContentBlock{{Type: llm.TypeText, Text: string(raw)}}
				replacement = &content
			}
			if mode == "cancelled-processing" {
				stopped, cancel := context.WithCancel(ctx)
				cancel()
				if err = r.session.CompleteToolProcessing(stopped, entry.ID, nil); err == nil {
					t.Fatal("cancelled processing succeeded")
				}
			} else if mode != "pending" {
				if err = r.session.CompleteToolProcessing(ctx, entry.ID, replacement); err != nil {
					t.Fatal(err)
				}
			}
			detail, err = r.session.GetRun(ctx, "parent")
			if err != nil {
				t.Fatal(err)
			}
			expected := mode == "original" || mode == "replacement"
			if got := r.childResultVisible(detail, task); got != expected {
				t.Fatalf("receipt=%v want=%v", got, expected)
			}
			second := task
			second.RunID = "second-run"
			second.ResultRef = "child-session/second-run"
			if r.childResultVisible(detail, second) {
				t.Fatal("receipt keyed only by child ID")
			}
			if _, err = r.session.FinishRun(ctx, "parent", sc.RunCompletion{State: sc.RunStateSuccess, StopReason: llm.StopStop}); err != nil {
				t.Fatal(err)
			}
			id := r.id
			if err = r.Close(ctx); err != nil {
				t.Fatal(err)
			}
			restored, err := h.Open(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			detail, err = restored.session.GetRun(ctx, "parent")
			if err != nil {
				t.Fatal(err)
			}
			if got := restored.childResultVisible(detail, task); got != expected {
				t.Fatal("receipt not durable across restore", got, expected)
			}
		})
	}
}
