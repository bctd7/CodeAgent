package cli

import (
	"CodeAgent/internal/llm"
	sc "CodeAgent/internal/session/config"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Headless command values keep their schema. Only terminal panels use this format.
func formatCommandValue(value any) (string, error) {
	if task, ok := value.(map[string]any); ok {
		if detail, ok := task["latest_run"].(sc.RunDetail); ok {
			return formatTaskRun(task["task"], detail), nil
		}
	}
	if status, ok := value.(Status); ok {
		return formatStatusPanel(status)
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var normalized any
	if err = json.Unmarshal(encoded, &normalized); err != nil {
		return "", err
	}
	var out strings.Builder
	formatTerminalValue(&out, normalized, "")
	return strings.TrimSpace(out.String()), nil
}
func formatTerminalValue(out *strings.Builder, value any, indent string) {
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			out.WriteString("无\n")
		}
		for _, key := range keys {
			fmt.Fprintf(out, "%s%s: ", indent, strings.ReplaceAll(key, "_", " "))
			switch value[key].(type) {
			case map[string]any, []any:
				out.WriteString("\n")
				formatTerminalValue(out, value[key], indent+"  ")
			default:
				formatTerminalValue(out, value[key], indent+"  ")
			}
		}
	case []any:
		if len(value) == 0 {
			fmt.Fprintf(out, "%s无\n", indent)
		}
		for _, item := range value {
			fmt.Fprintf(out, "%s• ", indent)
			formatTerminalValue(out, item, indent+"  ")
		}
	case nil:
		out.WriteString("无\n")
	case bool:
		if value {
			out.WriteString("是\n")
		} else {
			out.WriteString("否\n")
		}
	case string:
		if value == "" {
			value = "未设置"
		}
		out.WriteString(strings.ReplaceAll(value, "\n", "\n"+indent) + "\n")
	default:
		fmt.Fprintln(out, value)
	}
}

// Put the current model and project before verbose capability metadata.
func formatStatusPanel(status Status) (string, error) {
	var out strings.Builder
	fmt.Fprintf(&out, "模型：%s\n实际 ID：%s\n项目目录：%s\n会话状态：%s\n模型 REF：%s\n会话 ID：%s\n", status.Model.DisplayName, status.Model.ID, status.Capabilities.Instructions.ProjectRoot, status.State, status.Model.Ref, status.SessionID)
	details, err := formatCommandValue(map[string]any{"run_id": status.RunID, "queued_run_ids": status.QueuedRunIDs, "closing": status.Closing, "error": status.Error, "approvals": status.Approvals, "servers": status.Servers})
	if err != nil {
		return "", err
	}
	out.WriteString("\n运行详情：\n" + details)
	capabilities, err := formatCommandValue(status.Capabilities)
	if err != nil {
		return "", err
	}
	out.WriteString("\n\n能力详情：\n" + capabilities)
	return out.String(), nil
}

func formatTaskRun(task any, detail sc.RunDetail) string {
	var out strings.Builder
	if task, ok := task.(sc.ChildTask); ok {
		fmt.Fprintf(&out, "子任务：%s · %s\n", task.ID, task.Role)
	}
	fmt.Fprintf(&out, "状态：%s\nRun：%s\n子会话：%s\n", detail.Record.State, detail.Record.ID, detail.Record.SessionID)
	if detail.Record.ErrorMessage != "" {
		fmt.Fprintf(&out, "错误：%s\n", detail.Record.ErrorMessage)
	}
	for i := len(detail.Entries) - 1; i >= 0; i-- {
		if message, ok := detail.Entries[i].Message.(*llm.AssistantMessage); ok {
			out.WriteString("\n" + textBlocks(message.Content))
			break
		}
	}
	return out.String()
}

// Called only for trusted child_run context metadata, never to classify user text.
func formatChildContext(text string) string {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return "子任务反馈\n" + text
	}
	var task struct {
		ID        string          `json:"id"`
		State     string          `json:"state"`
		Result    json.RawMessage `json:"result"`
		Error     string          `json:"error"`
		RunID     string          `json:"run_id"`
		SessionID string          `json:"session_id"`
	}
	if json.NewDecoder(strings.NewReader(text[start:])).Decode(&task) != nil {
		return "子任务反馈\n" + text
	}
	var result string
	if json.Unmarshal(task.Result, &result) != nil { // Readable projection of old saved deliveries, without rewriting them.
		var blocks []llm.ContentBlock
		if json.Unmarshal(task.Result, &blocks) == nil {
			result = textBlocks(blocks)
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "子任务结果 · %s\n任务：%s\nRun：%s\n子会话：%s", task.State, task.ID, task.RunID, task.SessionID)
	if task.Error != "" {
		out.WriteString("\n错误：" + task.Error)
	}
	if result != "" {
		out.WriteString("\n\n" + result)
	}
	return out.String()
}
