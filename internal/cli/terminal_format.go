package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Headless command values keep their schema. Only terminal panels use this format.
func formatCommandValue(value any) (string, error) {
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
