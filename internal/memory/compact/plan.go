package compact

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"CodeAgent/internal/llm"
)

// Pi 算法参考固定版本 1cedd32724abfcb0915f76cc61b6827e2c16dbad：
// packages/coding-agent/src/core/compaction/compaction.ts。
// 本实现只处理已投影的三类 LLM 消息，并额外保护跨消息的未完成工具组。

// Prepare 只计算连续保留范围，不判断触发时机、不调用模型、不修改输入。
func Prepare(input Input) (Plan, error) {
	if input.Settings.ReserveTokens < 0 || input.Settings.KeepRecentTokens < 0 || !utf8.ValidString(input.PreviousSummary) {
		return Plan{}, failure(InvalidInput, fmt.Errorf("negative settings or invalid previous summary"))
	}
	tokens, safe, err := inspectSources(input.Sources)
	if err != nil {
		return Plan{}, failure(InvalidInput, err)
	}
	plan := Plan{prepared: true, PreviousSummary: input.PreviousSummary, Settings: input.Settings}
	plan.EstimatedTokensBefore = charsToTokens(textChars(input.PreviousSummary))
	var cuts []int
	for i, source := range input.Sources {
		plan.EstimatedTokensBefore, err = addTokens(plan.EstimatedTokensBefore, tokens[i])
		if err != nil {
			return Plan{}, failure(InvalidInput, err)
		}
		if _, toolResult := source.Message.(*llm.ToolResultMessage); !toolResult && safe[i] {
			cuts = append(cuts, i)
		}
	}
	if len(cuts) == 0 {
		plan.Retained = input.Sources
		return plan, nil
	}
	cut, accumulated := cuts[0], 0
	for i := len(tokens) - 1; i >= 0; i-- {
		if tokens[i] == 0 {
			continue
		}
		accumulated, err = addTokens(accumulated, tokens[i])
		if err != nil {
			return Plan{}, failure(InvalidInput, err)
		}
		if accumulated >= input.Settings.KeepRecentTokens {
			candidate := sort.SearchInts(cuts, i)
			if candidate == len(cuts) {
				candidate--
			}
			cut = cuts[candidate]
			break
		}
	}
	plan.FirstKeptIndex, plan.FirstKeptEntryID = cut, input.Sources[cut].ID
	plan.Retained = input.Sources[cut:]
	plan.Ready = cut > 0
	historyEnd := cut
	if _, assistant := input.Sources[cut].Message.(*llm.AssistantMessage); assistant {
		for i := cut - 1; i >= 0; i-- {
			if _, user := input.Sources[i].Message.(*llm.UserMessage); user {
				plan.IsSplitTurn = true
				historyEnd = i
				plan.TurnPrefix = input.Sources[i:cut]
				break
			}
		}
	}
	plan.History = input.Sources[:historyEnd]
	return plan, nil
}

// inspectSources 校验来源、估算消息并标记不能切开的工具组。
func inspectSources(sources []Source) ([]int, []bool, error) {
	tokens, safe := make([]int, len(sources)), make([]bool, len(sources)+1)
	for i := range safe {
		safe[i] = true
	}
	type group struct {
		start, remaining int
	}
	type call struct {
		group *group
		name  string
	}
	ids, pending := make(map[string]bool, len(sources)), make(map[string]call)
	protect := func(start, end int) {
		for i := start + 1; i <= end; i++ {
			safe[i] = false
		}
	}
	for i, source := range sources {
		if strings.TrimSpace(source.ID) == "" || !utf8.ValidString(source.ID) || ids[source.ID] {
			return nil, nil, fmt.Errorf("empty, invalid or duplicate source ID at %d", i)
		}
		ids[source.ID] = true
		if len(pending) != 0 {
			if _, result := source.Message.(*llm.ToolResultMessage); !result {
				return nil, nil, fmt.Errorf("message %q interrupts an unfinished tool group", source.ID)
			}
		}
		var err error
		tokens[i], err = EstimateTokens(source.Message)
		if err != nil {
			return nil, nil, fmt.Errorf("source %q: %w", source.ID, err)
		}
		switch m := source.Message.(type) {
		case *llm.AssistantMessage:
			if !m.State.IsTerminal() || m.State == llm.StateDeferred {
				protect(i, len(sources))
			}
			g := &group{start: i}
			seen := make(map[string]bool)
			for _, block := range m.Content {
				if block.Type != llm.TypeToolCall {
					continue
				}
				if _, exists := pending[block.ID]; exists || seen[block.ID] {
					return nil, nil, fmt.Errorf("ambiguous tool call ID %q", block.ID)
				}
				seen[block.ID] = true
				pending[block.ID] = call{group: g, name: block.Name}
				g.remaining++
			}
		case *llm.ToolResultMessage:
			c, exists := pending[m.ToolCallID]
			if !exists || m.ToolName != "" && m.ToolName != c.name {
				return nil, nil, fmt.Errorf("unmatched tool result at %q", source.ID)
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
		break // 未完成组期间拒绝插入其他调用，因此只会剩下一个组。
	}
	return tokens, safe, nil
}
