package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	sessionconfig "CodeAgent/internal/session/config"
)

type CLI struct {
	backend Backend
	models  []ModelInfo
	tui     TUI
	version string
}

func New(cfg Config) (*CLI, error) {
	if cfg.Backend == nil {
		return nil, fmt.Errorf("cli: Backend 不能为空")
	}
	if cfg.Version == "" {
		cfg.Version = "development"
	}
	return &CLI{backend: cfg.Backend, models: append([]ModelInfo{}, cfg.Models...), tui: cfg.TUI, version: cfg.Version}, nil
}

// Models 为选择界面返回安全展示数据的副本。
func (c *CLI) Models() []ModelInfo { return append([]ModelInfo{}, c.models...) }

// Parse 接收 shell 已拆分的参数；位置参数以 -- 与选项分隔，未指定模型不填默认值。
func Parse(args []string) (Invocation, error) {
	pos, flags, err := options(args)
	if err != nil {
		return Invocation{}, err
	}
	i := Invocation{Action: "tui"}
	allowed := "model directory session"
	if _, ok := flags["help"]; ok {
		if len(flags) != 1 || len(pos) != 0 {
			return i, fmt.Errorf("--help 不能与其他参数组合")
		}
		i.Action = "help"
		return i, nil
	}
	if _, ok := flags["version"]; ok {
		if len(flags) != 1 || len(pos) != 0 {
			return i, fmt.Errorf("--version 不能与其他参数组合")
		}
		i.Action = "version"
		return i, nil
	}
	if len(pos) != 0 {
		switch pos[0] {
		case "run":
			if len(pos) != 2 || strings.TrimSpace(pos[1]) == "" {
				return i, fmt.Errorf("run 需要一个非空任务参数；包含空格时请加引号")
			}
			i.Action, i.Task, allowed = "run", pos[1], allowed+" json"
		case "models":
			if len(pos) != 1 {
				return i, fmt.Errorf("models 不接受位置参数")
			}
			i.Action, allowed = "models", "json"
		case "session":
			if len(pos) < 2 {
				return i, fmt.Errorf("session 需要 list/create/rename/archive/unarchive 子命令")
			}
			i.Action = "session:" + pos[1]
			switch pos[1] {
			case "list":
				if len(pos) != 2 {
					return i, fmt.Errorf("session list 不接受位置参数；搜索使用 --search")
				}
				allowed = "search limit offset archive-filter json"
			case "create":
				if len(pos) > 3 {
					return i, fmt.Errorf("session create 最多接受一个标题参数")
				}
				if len(pos) == 3 {
					i.Title = pos[2]
				}
				allowed = "model directory json"
			case "rename":
				if len(pos) != 4 || strings.TrimSpace(pos[2]) == "" || strings.TrimSpace(pos[3]) == "" {
					return i, fmt.Errorf("session rename 需要 ID 和非空标题")
				}
				i.SessionID, i.Title, allowed = pos[2], pos[3], "json"
			case "archive", "unarchive":
				if len(pos) != 3 || strings.TrimSpace(pos[2]) == "" {
					return i, fmt.Errorf("session %s 需要 ID", pos[1])
				}
				i.SessionID, allowed = pos[2], "json"
			default:
				return i, fmt.Errorf("未知 session 子命令")
			}
		default:
			return i, fmt.Errorf("未知终端命令；使用 --help 查看支持项")
		}
	}
	for name := range flags {
		if !strings.Contains(" "+allowed+" ", " "+name+" ") {
			return i, fmt.Errorf("当前命令不接受 --%s", name)
		}
	}
	i.ModelRef, i.WorkingDir = flags["model"], flags["directory"]
	if id, ok := flags["session"]; ok {
		i.SessionID = id
	}
	_, i.JSON = flags["json"]
	i.ListOptions, err = listOptions(flags)
	if err != nil {
		return i, err
	}
	if i.Action == "session:create" || i.Action == "run" && i.SessionID == "" || i.Action == "tui" && i.SessionID == "" && (i.ModelRef != "" || i.WorkingDir != "") {
		if i.ModelRef == "" {
			return i, fmt.Errorf("新建会话必须显式指定 --model")
		}
	}
	return i, nil
}

func options(args []string) ([]string, map[string]string, error) {
	pos, flags := []string{}, map[string]string{}
	values := " model directory session search limit offset archive-filter "
	booleans := " json help version "
	literal := false
	for n := 0; n < len(args); n++ {
		arg := args[n]
		if literal || !strings.HasPrefix(arg, "-") {
			pos = append(pos, arg)
			continue
		}
		if arg == "--" {
			literal = true
			continue
		}
		if arg == "-h" {
			arg = "--help"
		}
		if !strings.HasPrefix(arg, "--") {
			return nil, nil, fmt.Errorf("未知短选项")
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if _, duplicate := flags[name]; duplicate {
			return nil, nil, fmt.Errorf("重复选项 --%s", name)
		}
		switch {
		case strings.Contains(booleans, " "+name+" "):
			if hasValue {
				return nil, nil, fmt.Errorf("--%s 不接受值", name)
			}
		case strings.Contains(values, " "+name+" "):
			if !hasValue {
				n++
				if n >= len(args) || strings.HasPrefix(args[n], "--") {
					return nil, nil, fmt.Errorf("--%s 缺少值", name)
				}
				value = args[n]
			}
			if strings.TrimSpace(value) == "" {
				return nil, nil, fmt.Errorf("--%s 需要非空值", name)
			}
		default:
			return nil, nil, fmt.Errorf("未知选项 --%s", name)
		}
		flags[name] = value
	}
	return pos, flags, nil
}

func listOptions(flags map[string]string) (sessionconfig.SessionListOptions, error) {
	o := sessionconfig.SessionListOptions{TitleSearch: flags["search"], ArchiveFilter: sessionconfig.ArchiveFilter(flags["archive-filter"])}
	if o.ArchiveFilter != "" && o.ArchiveFilter != sessionconfig.ArchiveFilterActive && o.ArchiveFilter != sessionconfig.ArchiveFilterArchived && o.ArchiveFilter != sessionconfig.ArchiveFilterAll {
		return o, fmt.Errorf("--archive-filter 必须为 active/archived/all")
	}
	for _, field := range []struct {
		name   string
		target *int
	}{{"limit", &o.Limit}, {"offset", &o.Offset}} {
		if value, ok := flags[field.name]; ok {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return o, fmt.Errorf("--%s 必须为非负整数", field.name)
			}
			*field.target = n
		}
	}
	return o, nil
}

// Run 只返回退出码，不调用 os.Exit 或注册信号。普通管理命令不关闭调用方的共享 Backend。
func (c *CLI) Run(ctx context.Context, args []string, streams IO) int {
	if streams.Out == nil {
		streams.Out = io.Discard
	}
	if streams.Err == nil {
		streams.Err = io.Discard
	}
	i, err := Parse(args)
	if err != nil {
		fmt.Fprintln(streams.Err, err)
		return ExitUsage
	}
	if i.Action == "run" {
		return c.run(ctx, i, streams)
	}
	var output any
	switch i.Action {
	case "help":
		output = Help
	case "version":
		output = c.version
	case "models":
		output = c.models
	case "tui":
		if c.tui == nil {
			err = fmt.Errorf("cli: 尚未注入 TUI 启动接口")
			break
		}
		var state ChatState
		if i.SessionID != "" || i.ModelRef != "" || i.WorkingDir != "" {
			state.Runtime, _, err = c.open(ctx, i)
		}
		if err == nil {
			state, err = c.tui.Run(ctx, c, state, streams)
			if err == io.EOF {
				err = nil
			}
			if ctx.Err() != nil {
				err = errors.Join(err, ctx.Err())
			}
			if state.CurrentHandle != nil {
				_, finishErr := c.EndInput(ctx, state)
				err = errors.Join(err, finishErr)
			}
			err = errors.Join(err, c.backend.Close(context.WithoutCancel(ctx)))
		}
	default:
		output, err = c.manageSession(ctx, i)
	}
	if err != nil {
		fmt.Fprintln(streams.Err, err)
		return errorCode(err)
	}
	if output != nil {
		if err = c.write(streams.Out, output, i.JSON); err != nil {
			fmt.Fprintln(streams.Err, err)
			return ExitFailure
		}
	}
	return ExitOK
}

func errorCode(err error) int {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ExitCancelled
	}
	return ExitFailure
}

func (c *CLI) write(out io.Writer, value any, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(value)
	}
	switch value := value.(type) {
	case string:
		_, err := fmt.Fprintln(out, value)
		return err
	case []ModelInfo:
		for _, model := range value {
			if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", model.Ref, model.ID, model.DisplayName); err != nil {
				return err
			}
		}
	case []sessionconfig.SessionInfo:
		for _, session := range value {
			status := "active"
			if session.ArchivedAt != nil {
				status = "archived"
			}
			if _, err := fmt.Fprintf(out, "%s\t%s\t%s\n", session.ID, status, session.Title); err != nil {
				return err
			}
		}
	case sessionconfig.SessionInfo:
		_, err := fmt.Fprintf(out, "%s\t%s\n", value.ID, value.Title)
		return err
	default:
		return json.NewEncoder(out).Encode(value)
	}
	return nil
}

const Help = `用法：
  codeagent [--session ID] [--model REF] [--directory DIR]
  codeagent run "任务" [--session ID] [--model REF] [--directory DIR] [--json]
  codeagent session list [--search TEXT] [--limit N] [--offset N] [--archive-filter active|archived|all] [--json]
  codeagent session create ["标题"] --model REF [--directory DIR] [--json]
  codeagent session rename ID "标题" [--json]
  codeagent session archive ID [--json]
  codeagent session unarchive ID [--json]
  codeagent models [--json]
  codeagent --help | --version
会话内：/new /sessions /rename /archive /close /quit /model /directory /status /steer /cancel /history /runs /run ID /usage /help
新建必须明确模型；恢复沿用保存模型。目录只能首次绑定。
CLI 无审批通道；需要批准的操作必须由后端拒绝或在交互层完成审批。`
