// 本文件按输入验证与编码、输出解释与解析的顺序组织脚本协议。
package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// preparedEvent 将匹配元数据与已编码载荷绑定，避免两者不一致。
type preparedEvent struct {
	mcp    bool
	name   Event
	common CommonInput
	match  string
	raw    []byte
}

type wireHeader struct {
	HookEventName Event `json:"hook_event_name"`
}

// 事件元数据和传输载荷来自同一份类型化输入。
// 编码时直接嵌入头部，避免序列化、映射转换、反序列化的往返。
func prepareEvent(input any, inputLimit int) (preparedEvent, error) {
	var event preparedEvent
	var payload any
	var err error
	switch in := input.(type) {
	case SessionStartInput:
		event = preparedEvent{name: SessionStart, common: in.CommonInput, match: in.Source}
		if !oneOf(in.Source, "startup", "resume", "clear", "compact", "fork") {
			err = inputError("session source")
		}
		payload = struct {
			wireHeader
			SessionStartInput
		}{wireHeader{SessionStart}, in}
	case UserPromptSubmitInput:
		event = preparedEvent{name: UserPromptSubmit, common: in.CommonInput}
		payload = struct {
			wireHeader
			UserPromptSubmitInput
		}{wireHeader{UserPromptSubmit}, in}
	case PreToolUseInput:
		event = preparedEvent{name: PreToolUse, common: in.CommonInput, match: in.ToolName}
		err = validateTool(in.ToolInput)
		payload = struct {
			wireHeader
			PreToolUseInput
		}{wireHeader{PreToolUse}, in}
	case PostToolUseInput:
		event = preparedEvent{name: PostToolUse, common: in.CommonInput, match: in.ToolName, mcp: in.IsMCP}
		err = validateToolResult(in.ToolInput, in.DurationMS)
		if err == nil && !json.Valid(in.ToolResponse) {
			err = inputError("tool_response JSON required")
		}
		payload = struct {
			wireHeader
			PostToolUseInput
		}{wireHeader{PostToolUse}, in}
	case PostToolUseFailureInput:
		event = preparedEvent{name: PostToolUseFailure, common: in.CommonInput, match: in.ToolName}
		err = validateToolResult(in.ToolInput, in.DurationMS)
		if err == nil && in.Error == "" {
			err = inputError("failure error required")
		}
		payload = struct {
			wireHeader
			PostToolUseFailureInput
		}{wireHeader{PostToolUseFailure}, in}
	case PostToolBatchInput:
		event = preparedEvent{name: PostToolBatch, common: in.CommonInput}
		err = validateBatch(in.ToolCalls)
		payload = struct {
			wireHeader
			PostToolBatchInput
		}{wireHeader{PostToolBatch}, in}
	case StopInput:
		event = preparedEvent{name: Stop, common: in.CommonInput}
		for _, raw := range []json.RawMessage{in.BackgroundTasks, in.SessionCrons} {
			if raw != nil && !isJSONArray(raw) {
				err = inputError("stop task/cron lists must be JSON arrays")
				break
			}
		}
		payload = struct {
			wireHeader
			StopInput
		}{wireHeader{Stop}, in}
	case SessionEndInput:
		event = preparedEvent{name: SessionEnd, common: in.CommonInput, match: in.Reason}
		if !oneOf(in.Reason, "clear", "resume", "logout", "prompt_input_exit", "other") {
			err = inputError("session end reason")
		}
		payload = struct {
			wireHeader
			SessionEndInput
		}{wireHeader{SessionEnd}, in}
	default:
		panic("hooks: unhandled internal event input")
	}
	if err != nil {
		return event, err
	}
	if err := validateCommon(event.common); err != nil {
		return event, err
	}
	event.raw, err = json.Marshal(payload)
	if err != nil {
		return event, inputError(err.Error())
	}
	if len(event.raw) > inputLimit {
		return event, inputError("input too large")
	}
	// RawMessage 字段由调用方提供；在事件入口统一检查
	// 重复键、编码和嵌套深度，后续处理复用验证结果。
	if err := validateJSON(event.raw); err != nil {
		return event, inputError(err.Error())
	}
	return event, nil
}

// validateCommon 检查会话标识和工作目录等公共输入约束。
func validateCommon(in CommonInput) error {
	if in.SessionID == "" || !filepath.IsAbs(in.CWD) {
		return inputError("session_id and absolute cwd required")
	}
	for _, path := range []string{in.TranscriptPath, in.ScratchpadDir, in.ProjectDir} {
		if path != "" && !filepath.IsAbs(path) {
			return inputError("optional paths must be absolute")
		}
	}
	return nil
}

// validateTool 检查工具标识与输入 JSON 的形状。
func validateTool(in ToolInput) error {
	if in.ToolName == "" || in.ToolUseID == "" || !isJSONObject(in.ToolInput) {
		return inputError("tool name, call ID and argument object required")
	}
	if in.MCPServer != nil && !isJSONObject(in.MCPServer) {
		return inputError("mcp_server object required")
	}
	return nil
}

func validateToolResult(in ToolInput, duration *int64) error {
	if err := validateTool(in); err != nil {
		return err
	}
	if duration != nil && *duration < 0 {
		return inputError("duration_ms must be nonnegative")
	}
	return nil
}

// validateBatch 检查批量工具条目的标识、状态及响应内容。
func validateBatch(calls []BatchTool) error {
	if len(calls) == 0 {
		return inputError("nonempty complete batch required")
	}
	seen := map[string]bool{}
	for _, call := range calls {
		if err := validateTool(call.ToolInput); err != nil {
			return err
		}
		if seen[call.ToolUseID] {
			return inputError("duplicate batch call ID")
		}
		seen[call.ToolUseID] = true
		if !json.Valid(call.ToolResponse) {
			return inputError("model-facing batch response required")
		}
		if !oneOf(string(call.Status), string(ToolSucceeded), string(ToolFailed), string(ToolDenied), string(ToolCanceled), string(ToolUnavailable)) {
			return inputError("explicit batch status required")
		}
	}
	return nil
}

func inputError(message string) error { return fmt.Errorf("%w: %s", ErrInput, message) }

type preToolEffect struct {
	permission Permission
	reason     string
	input      *json.RawMessage
}

type postToolEffect struct {
	feedback          string
	output, mcpOutput *json.RawMessage
}

type decisionEffect struct {
	block  bool
	reason string
}

// effect 保存协议解释后的公共控制及各事件专属动作。
type effect struct {
	control         Control
	system, context string
	pre             preToolEffect
	post            postToolEffect
	decision        decisionEffect
}

// wireOutput 对应各事件共享的输出字段。
type wireOutput struct {
	Continue       *bool           `json:"continue"`
	StopReason     string          `json:"stopReason"`
	SuppressOutput bool            `json:"suppressOutput"`
	SystemMessage  string          `json:"systemMessage"`
	Decision       string          `json:"decision"`
	Reason         string          `json:"reason"`
	Specific       json.RawMessage `json:"hookSpecificOutput"`
}

type specificOutput struct {
	Event            Event           `json:"hookEventName"`
	Permission       Permission      `json:"permissionDecision"`
	PermissionReason string          `json:"permissionDecisionReason"`
	Input            json.RawMessage `json:"updatedInput"`
	Context          string          `json:"additionalContext"`
	Output           json.RawMessage `json:"updatedToolOutput"`
	MCPOutput        json.RawMessage `json:"updatedMCPToolOutput"`
}

// 标准输出承载控制决策。标准错误的解码问题只产生诊断，
// 不撤销有效的标准输出决策；进程执行失败不会凭空产生决策。
func interpret(event Event, toolName string, execution Execution, trustedMCP ...bool) (effect, error) {
	var out effect
	if execution.Status != ProcessExited {
		return out, execution.Error
	}
	stderr := strings.TrimSpace(strings.ToValidUTF8(string(execution.Stderr), "\uFFFD"))
	var stderrErr error
	if !utf8.Valid(execution.Stderr) {
		stderrErr = fmt.Errorf("%w: stderr is not UTF-8; display uses replacement characters", ErrProtocol)
	}
	if event == SessionEnd {
		if execution.ExitCode != 0 {
			return out, errors.Join(stderrErr, fmt.Errorf("SessionEnd hook exited %d: %s", execution.ExitCode, stderr))
		}
		return out, stderrErr
	}
	data := bytes.TrimSpace(execution.Stdout)
	var parseErr error
	parsed := false
	if !utf8.Valid(data) {
		parseErr = fmt.Errorf("%w: stdout must be UTF-8", ErrProtocol)
	} else if len(data) > 0 && data[0] == '{' && data[len(data)-1] == '}' {
		parsed = true
		out, parseErr = parseOutput(event, toolName, data, trustedMCP...)
		if parseErr != nil {
			out = effect{}
		}
	} else if execution.ExitCode == 0 && (event == SessionStart || event == UserPromptSubmit) {
		out.context = string(data)
	}
	if execution.ExitCode == 2 {
		parseErr = errors.Join(parseErr, applyBlockingExit(event, &out, stderr))
	} else if execution.ExitCode != 0 && (!parsed || parseErr != nil) {
		parseErr = errors.Join(parseErr, fmt.Errorf("hook exited %d: %s", execution.ExitCode, stderr))
	}
	return out, errors.Join(parseErr, stderrErr)
}

// applyBlockingExit 按事件解释退出码 2，并使用标准错误提供原因。
func applyBlockingExit(event Event, out *effect, stderr string) error {
	reason := stderr
	if out.pre.permission == Deny && out.pre.reason != "" {
		reason = out.pre.reason
	}
	if out.decision.block && out.decision.reason != "" {
		reason = out.decision.reason
	}
	if out.post.feedback != "" {
		reason = out.post.feedback
	}
	if reason == "" {
		reason = "Hook exited with blocking status 2"
	}
	switch event {
	case PreToolUse:
		out.pre.permission = Deny
		out.pre.reason = reason
	case UserPromptSubmit, PostToolBatch, Stop:
		out.decision = decisionEffect{block: true, reason: reason}
	case PostToolUse, PostToolUseFailure:
		out.post.feedback = reason
	case SessionStart:
		return fmt.Errorf("SessionStart hook exited 2: %s", reason)
	}
	return nil
}

// parseOutput 验证公共字段和事件专属字段，再转换为内部效果。
func parseOutput(event Event, toolName string, data []byte, trustedMCP ...bool) (effect, error) {
	if err := validateJSON(data); err != nil {
		return effect{}, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	common, err := parseCommonOutput(event, data)
	if err != nil {
		return effect{}, err
	}
	specific, err := parseSpecificOutput(event, common.Specific)
	if err != nil {
		return effect{}, err
	}
	out := effect{system: common.SystemMessage, context: specific.Context}
	if common.Continue != nil && !*common.Continue {
		out.control.StopProcessing = true
		out.control.StopReasons = appendNonempty(nil, common.StopReason)
		// 公共停止控制生效后，不再应用事件专属动作。
		// 未应用的改写无需检查调用方能力。
		return out, nil
	}
	switch event {
	case PreToolUse:
		out.pre = parsePreToolOutput(common, specific)
		if out.pre.permission == Defer {
			out.context = ""
		}
	case PostToolUse:
		out.post, err = parsePostToolOutput(toolName, common, specific, trustedMCP...)
	case PostToolUseFailure:
		if common.Decision == "block" {
			out.post.feedback = common.Reason
		}
	case UserPromptSubmit, PostToolBatch, Stop:
		out.decision = decisionEffect{block: common.Decision == "block", reason: common.Reason}
		if event == Stop && out.decision.block && strings.TrimSpace(common.Reason) == "" {
			err = fmt.Errorf("%w: Stop block requires reason", ErrProtocol)
		}
	case SessionStart:
		if common.Decision != "" {
			err = fmt.Errorf("%w: SessionStart decision", ErrUnsupported)
		}
	}
	return out, err
}

func parseCommonOutput(event Event, data []byte) (wireOutput, error) {
	var out wireOutput
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return out, fmt.Errorf("%w: output must be an object", ErrProtocol)
	}
	if err := checkFields(fields, []string{"continue", "stopReason", "suppressOutput", "systemMessage", "decision", "reason", "hookSpecificOutput"}, "hookSpecificOutput"); err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	if out.Decision != "" && out.Decision != "block" && !(event == PreToolUse && out.Decision == "approve") {
		return out, fmt.Errorf("%w: decision %q", ErrProtocol, out.Decision)
	}
	return out, nil
}

func parseSpecificOutput(event Event, data []byte) (specificOutput, error) {
	var out specificOutput
	if data == nil {
		return out, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return out, fmt.Errorf("%w: hookSpecificOutput must be an object", ErrProtocol)
	}
	allowed := []string{"hookEventName", "additionalContext"}
	switch event {
	case PreToolUse:
		allowed = append(allowed, "permissionDecision", "permissionDecisionReason", "updatedInput")
	case PostToolUse:
		allowed = append(allowed, "updatedToolOutput", "updatedMCPToolOutput")
	}
	if err := checkFields(fields, allowed, "updatedToolOutput", "updatedMCPToolOutput"); err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	if out.Event != event {
		return out, fmt.Errorf("%w: hookEventName does not match %s", ErrProtocol, event)
	}
	if event == PreToolUse {
		if !oneOf(string(out.Permission), "", "allow", "deny", "ask", "defer") {
			return out, fmt.Errorf("%w: permissionDecision", ErrProtocol)
		}
		if out.Input != nil && out.Permission != Defer && !isJSONObject(out.Input) {
			return out, fmt.Errorf("%w: updatedInput must be an object", ErrProtocol)
		}
	}
	return out, nil
}

func parsePreToolOutput(common wireOutput, specific specificOutput) preToolEffect {
	out := preToolEffect{permission: specific.Permission, reason: specific.PermissionReason}
	if out.permission == "" && common.Decision != "" {
		out.permission = Allow
		if common.Decision == "block" {
			out.permission = Deny
		}
		out.reason = common.Reason
	}
	if specific.Input != nil && out.permission != Defer {
		out.input = &specific.Input
	}
	return out
}

// parsePostToolOutput 解释反馈及输出改写，并校验工具适用范围。
func parsePostToolOutput(toolName string, common wireOutput, specific specificOutput, trustedMCP ...bool) (postToolEffect, error) {
	var out postToolEffect
	if common.Decision == "block" {
		out.feedback = common.Reason
	}
	if specific.Output != nil {
		out.output = &specific.Output
	}
	if specific.MCPOutput != nil {
		isMCP := strings.HasPrefix(toolName, "mcp__")
		if len(trustedMCP) > 0 {
			isMCP = trustedMCP[0]
		}
		if !isMCP {
			return out, fmt.Errorf("%w: updatedMCPToolOutput requires an MCP tool", ErrProtocol)
		}
		out.mcpOutput = &specific.MCPOutput
	}
	return out, nil
}

// checkFields 拒绝未声明字段，并检查允许为空的字段范围。
func checkFields(fields map[string]json.RawMessage, allowed []string, nullable ...string) error {
	for key, value := range fields {
		if !oneOf(key, allowed...) {
			return fmt.Errorf("%w: field %s", ErrUnsupported, key)
		}
		if isJSONNull(value) && !oneOf(key, nullable...) {
			return fmt.Errorf("%w: null %s", ErrProtocol, key)
		}
	}
	return nil
}
