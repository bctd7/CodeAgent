package memory

import (
	"fmt"

	"CodeAgent/internal/llm"
)

// 工具调用 ID 仅在组内唯一，不要求全局唯一。每组范围包含 assistant 及其全部结果，
// 即使结果乱序到达或中间插入其他消息，也不拆组。未完成组的范围延伸至输入末端。
func compactSafeCuts(sources []Source) ([]bool, int, error) {
	safe := make([]bool, len(sources)+1)
	for i := range safe {
		safe[i] = true
	}
	type group struct {
		start     int
		remaining int
	}
	type call struct {
		group *group
		name  string
	}
	pending := map[string]call{}
	latestUser := -1 // 没有用户消息时不构造用户锚点；近期尾部仍至少保留最后一条消息。
	protect := func(start, end int) {
		for i := start + 1; i <= end; i++ {
			safe[i] = false
		}
	}
	for i, source := range sources {
		switch m := source.Message.(type) {
		case *llm.UserMessage:
			latestUser = i
		case *llm.AssistantMessage:
			if !m.State.IsTerminal() || m.State == llm.StateDeferred {
				protect(i, len(sources))
			}
			g := &group{start: i}
			for _, block := range m.Content {
				if block.Type != llm.TypeToolCall {
					continue
				}
				if block.ID == "" || block.Name == "" {
					return nil, 0, fmt.Errorf("missing tool ID/name at %s", source.ID)
				}
				if _, exists := pending[block.ID]; exists {
					return nil, 0, fmt.Errorf("ambiguous pending tool ID %q", block.ID)
				}
				pending[block.ID] = call{group: g, name: block.Name}
				g.remaining++
			}
		case *llm.ToolResultMessage:
			c, exists := pending[m.ToolCallID]
			if !exists || (m.ToolName != "" && m.ToolName != c.name) {
				return nil, 0, fmt.Errorf("unmatched tool result at %s", source.ID)
			}
			delete(pending, m.ToolCallID)
			c.group.remaining--
			if c.group.remaining == 0 {
				protect(c.group.start, i)
			}
		}
	}
	for _, c := range pending {
		protect(c.group.start, len(sources))
	}
	return safe, latestUser, nil
}

// 边界只向前回退，不为落入预算而额外丢弃近期消息。
// 最新用户消息可在尾部之前单独保留；若它插在工具组内部，则回退并原序保留整个组，
// 避免将该用户消息从组内抽出重放。未完成工具组始终由 safe 边界保护。
func compactCut(desired, latestUser int, safe []bool) int {
	cut := desired
	if latestUser >= 0 && latestUser < cut && !safe[latestUser] {
		cut = latestUser
	}
	for cut > 0 && !safe[cut] {
		cut--
	}
	return cut
}
