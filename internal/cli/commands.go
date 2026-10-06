package cli

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"CodeAgent/internal/llm"
	sessionconfig "CodeAgent/internal/session/config"
)

// Split 使用单/双引号分组，反斜杠始终保留，不实现 shell 转义、变量或命令展开。
func Split(input string) ([]string, error) {
	args := []string{}
	var word strings.Builder
	var quote rune
	started := false
	for _, r := range input {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote = r
			started = true
		case unicode.IsSpace(r):
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("引号未闭合")
	}
	if started {
		args = append(args, word.String())
	}
	return args, nil
}

// Command 不打印聊天界面，返回结果及交互意图；ChatState 的更新由 TUI 负责。
func (c *CLI) Command(ctx context.Context, state ChatState, input string) (CommandResult, error) {
	result := CommandResult{Intent: ShowResult}
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return result, fmt.Errorf("输入不能为空")
	}
	if !strings.HasPrefix(trimmed, "/") {
		if state.Runtime == nil {
			return result, fmt.Errorf("请先选择或新建会话")
		}
		handle, err := state.Runtime.Submit(ctx, &llm.UserMessage{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: input}}, Timestamp: time.Now()})
		result.Intent, result.Handle = TaskSubmitted, handle
		return result, err
	}
	args, err := Split(trimmed)
	if err != nil {
		return result, err
	}
	name, args := args[0], args[1:]
	switch name {
	case "/help":
		if len(args) != 0 {
			return result, fmt.Errorf("/help 不接受参数")
		}
		result.Help = Help + "\n" + CommandHelp
		return result, nil
	case "/new":
		i, err := Parse(append([]string{"session", "create"}, args...))
		if err != nil {
			return result, err
		}
		if i.JSON {
			return result, fmt.Errorf("会话内命令返回结构化结果，不接受 --json")
		}
		result.Runtime, result.Session, err = c.open(ctx, i)
		result.Intent = EnterSession
		return result, err
	case "/sessions":
		if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
			i, err := Parse(append([]string{"--session", args[0]}, args[1:]...))
			if err != nil {
				return result, err
			}
			result.Runtime, result.Session, err = c.open(ctx, i)
			result.Intent = EnterSession
			return result, err
		}
		i, err := Parse(append([]string{"session", "list"}, args...))
		if err != nil {
			return result, err
		}
		if i.JSON {
			return result, fmt.Errorf("会话内命令返回结构化结果，不接受 --json")
		}
		result.Sessions, err = c.backend.ListSessions(ctx, i.ListOptions)
		return result, err
	case "/quit":
		if len(args) != 0 {
			return result, fmt.Errorf("/quit 不接受参数")
		}
		result.Intent = Quit
		return result, c.backend.Close(context.WithoutCancel(ctx))
	}
	if state.Runtime == nil {
		return result, fmt.Errorf("当前没有选择会话")
	}
	r := state.Runtime
	switch name {
	case "/rename":
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return result, fmt.Errorf("/rename 需要一个标题参数，空格标题请加引号")
		}
		err = c.backend.RenameSession(ctx, r.SessionID(), args[0])
	case "/archive":
		if len(args) != 0 {
			return result, fmt.Errorf("/archive 不接受参数")
		}
		err = c.backend.ArchiveSession(ctx, r.SessionID())
		if err == nil {
			result.Intent = SelectSession
		}
	case "/close":
		if len(args) != 0 {
			return result, fmt.Errorf("/close 不接受参数")
		}
		err = c.backend.CloseSession(ctx, r.SessionID())
		if err == nil {
			result.Intent = SelectSession
		}
	case "/model":
		if len(args) > 1 {
			return result, fmt.Errorf("/model 最多接受一个模型引用")
		}
		if len(args) == 1 {
			if strings.TrimSpace(args[0]) == "" {
				return result, fmt.Errorf("模型引用不能为空")
			}
			err = r.SetModel(ctx, sessionconfig.ModelSelection{Ref: args[0]})
		}
		if err == nil {
			snapshot := projectStatus(r.Snapshot())
			result.Snapshot = &snapshot
			result.Models = append([]ModelInfo{}, c.models...)
		}
	case "/directory":
		if len(args) > 1 {
			return result, fmt.Errorf("/directory 最多接受一个目录")
		}
		if len(args) == 1 {
			if strings.TrimSpace(args[0]) == "" {
				return result, fmt.Errorf("目录不能为空")
			}
			err = r.BindWorkingDir(ctx, args[0])
		}
		if err == nil {
			snapshot := projectStatus(r.Snapshot())
			result.Snapshot = &snapshot
		}
	case "/status":
		if len(args) != 0 {
			return result, fmt.Errorf("/status 不接受参数")
		}
		snapshot := projectStatus(r.Snapshot())
		result.Snapshot = &snapshot
	case "/steer":
		if len(args) == 0 || strings.TrimSpace(strings.Join(args, " ")) == "" {
			return result, fmt.Errorf("/steer 需要补充指令")
		}
		err = r.Steer(strings.Join(args, " "))
	case "/cancel":
		if len(args) != 0 {
			return result, fmt.Errorf("/cancel 不接受 Run ID")
		}
		current := r.Snapshot().Agent.CurrentRunID
		if state.CurrentHandle == nil || current == "" || state.CurrentHandle.ID() != current {
			return result, fmt.Errorf("没有持有当前任务的句柄")
		}
		state.CurrentHandle.Cancel()
	case "/history":
		if len(args) != 0 {
			return result, fmt.Errorf("/history 不接受参数")
		}
		result.History, err = r.GetCurrentHistory(ctx)
	case "/runs":
		var pos []string
		var flags map[string]string
		pos, flags, err = options(args)
		if err != nil {
			return result, err
		}
		if len(pos) != 0 {
			return result, fmt.Errorf("/runs 只接受 --limit 和 --offset")
		}
		for flag := range flags {
			if flag != "limit" && flag != "offset" {
				return result, fmt.Errorf("/runs 不接受 --%s", flag)
			}
		}
		var opts sessionconfig.SessionListOptions
		opts, err = listOptions(flags)
		if err == nil {
			result.Runs, err = r.ListRuns(ctx, opts.Limit, opts.Offset)
		}
	case "/run":
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return result, fmt.Errorf("/run 需要 Run ID")
		}
		detail, readErr := r.GetRun(ctx, args[0])
		err = readErr
		if err == nil {
			result.Run = &detail
		}
	case "/usage":
		if len(args) > 1 {
			return result, fmt.Errorf("/usage 最多接受一个 Run ID")
		}
		id := r.Snapshot().Agent.CurrentRunID
		if len(args) == 1 {
			id = args[0]
		}
		if strings.TrimSpace(id) == "" {
			return result, fmt.Errorf("当前没有活动 Run，请显式指定 Run ID；此入口不统计会话累计用量")
		}
		detail, readErr := r.GetRun(ctx, id)
		err = readErr
		if err == nil {
			result.Usage = &RunUsage{RunID: id, UsageByOrigin: detail.UsageByOrigin, TotalUsage: detail.TotalUsage}
		}
	default:
		return result, fmt.Errorf("未知会话内命令；使用 /help 查看支持项")
	}
	return result, err
}

const CommandHelp = `会话内参数：
  /new ["标题"] --model REF [--directory DIR]
  /sessions [--search TEXT] [--limit N] [--offset N] [--archive-filter active|archived|all]
  /sessions ID [--model REF] [--directory DIR]
  /rename "标题" | /archive | /close | /quit
  /model [REF] | /directory [DIR] | /status
  /steer "补充指令" | /cancel | /history
  /runs [--limit N] [--offset N] | /run ID | /usage [ID] | /help
普通文本提交任务。/cancel 只取消已持有句柄对应的当前任务。/usage 不带 ID 时只查询活动 Run。
单/双引号用于分组；反斜杠保持原样，不进行 shell 展开。`
