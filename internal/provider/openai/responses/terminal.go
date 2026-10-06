package responses

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"CodeAgent/internal/llm"
)

func (a *accumulator) metadata(response *responseObject) error {
	if response.ID != "" {
		if a.message.ResponseID != "" && a.message.ResponseID != response.ID {
			return fmt.Errorf("Responses response id 发生变化")
		}
		a.message.ResponseID = response.ID
	}
	if response.Model != "" {
		if a.message.ResponseModel != "" && a.message.ResponseModel != response.Model {
			return fmt.Errorf("Responses response model 发生变化")
		}
		a.message.ResponseModel = response.Model
	}
	return nil
}

func mapUsage(value responseUsage, pricing llm.Pricing) (llm.Usage, error) {
	cached, written, reasoning := value.InputDetails.CachedTokens, value.InputDetails.CacheWriteTokens, value.OutputDetails.ReasoningTokens
	if value.InputTokens < 0 || value.OutputTokens < 0 || cached < 0 || written < 0 || reasoning < 0 ||
		cached > value.InputTokens || written > value.InputTokens-cached || reasoning > value.OutputTokens ||
		value.InputTokens > math.MaxInt64-value.OutputTokens || value.TotalTokens != value.InputTokens+value.OutputTokens {
		return llm.Usage{}, fmt.Errorf("Responses usage 计数不一致")
	}
	// cached/cache_write 是输入分类，reasoning 是输出子集；任何分类均不重复加入 Total。
	usage := llm.Usage{Input: value.InputTokens - cached - written, CacheRead: cached, CacheWrite: written,
		Output: value.OutputTokens, Reasoning: reasoning, Total: value.TotalTokens}
	return llm.CalculateCost(usage, pricing), nil
}

func (a *accumulator) finish(eventType string, response *responseObject) error {
	expected := strings.TrimPrefix(eventType, "response.")
	if response.Status != expected || response.ID == "" {
		return fmt.Errorf("Responses 终态的 status/id 无效")
	}
	if err := a.metadata(response); err != nil {
		return err
	}
	a.message.RawStopReason = response.Status
	if response.Incomplete != nil && response.Status == "incomplete" {
		a.message.RawStopReason = response.Incomplete.Reason
	}
	if response.Usage != nil {
		usage, err := mapUsage(*response.Usage, a.model.Pricing)
		if err != nil {
			return err
		}
		a.message.Usage = usage
	}
	if response.Status == "completed" && response.Output == nil {
		return fmt.Errorf("Responses completed 缺少 output")
	}
	// 最终 output 为权威顺序，并核对与已收到的索引、ID 和分段是否一致。
	for index, item := range response.Output {
		if response.Status == "completed" && item.Status != "" && item.Status != "completed" {
			return fmt.Errorf("Responses completed 包含未完成的输出项")
		}
		if err := a.applyItem(index, item, true); err != nil {
			return err
		}
	}
	if response.Status == "completed" && len(a.items) != len(response.Output) {
		return fmt.Errorf("Responses completed 丢失已接收的输出项")
	}
	if response.Status == "failed" {
		a.message.ErrorMessage = "Responses 生成失败"
		if response.Error != nil {
			a.message.ErrorMessage = fmt.Sprintf("Responses failed (%s): %s", response.Error.Code, response.Error.Message)
		}
		return nil // run 统一脱敏后 Fail。
	}
	if response.Error != nil {
		return fmt.Errorf("Responses 非 failed 终态包含 error")
	}
	if a.refusal || (response.Status == "incomplete" && response.Incomplete != nil && response.Incomplete.Reason == "content_filter") {
		a.message.StopReason = llm.StopSafety
		a.message.ErrorMessage = "模型拒绝响应或内容被安全过滤器拦截"
		return nil
	}
	stop := llm.StopStop
	if response.Status == "incomplete" {
		if response.Incomplete == nil || response.Incomplete.Reason != "max_output_tokens" {
			return fmt.Errorf("Responses incomplete: 不支持的结束原因 %q", a.message.RawStopReason)
		}
		stop = llm.StopLength
	} else {
		for _, item := range a.items {
			if item.kind == "function_call" {
				if !item.argsDone || item.argsErr != nil {
					return fmt.Errorf("Responses completed 包含不完整或非法工具参数")
				}
				stop = llm.StopToolUse
			}
		}
	}
	if err := a.closeBlocks(); err != nil {
		return err
	}
	a.orderContent()
	a.message.State, a.message.StopReason = llm.StateSuccess, stop
	return nil
}

func (a *accumulator) closeBlocks() error {
	for _, index := range a.itemIndices() {
		item := a.items[index]
		switch item.kind {
		case "message":
			for _, partIndex := range sortedParts(item.parts) {
				part := item.parts[partIndex]
				if !part.done {
					part.done = true
					if err := a.emit(llm.EventTextBlockEnd, part.block, ""); err != nil {
						return err
					}
				}
			}
		case "function_call":
			if !item.argsDone {
				if err := a.arguments(item, item.args, true, true); err != nil {
					return err
				}
			}
		case "reasoning":
			if !item.done {
				if err := a.emit(llm.EventThinkingBlockEnd, item.block, ""); err != nil {
					return err
				}
			}
		}
		item.done = true
	}
	return nil
}

func (a *accumulator) itemIndices() []int {
	indices := make([]int, 0, len(a.items))
	for index := range a.items {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	return indices
}

func (a *accumulator) orderContent() {
	ordered := make([]llm.ContentBlock, 0, len(a.message.Content))
	for _, index := range a.itemIndices() {
		item := a.items[index]
		if item.kind == "message" {
			for _, partIndex := range sortedParts(item.parts) {
				ordered = append(ordered, a.message.Content[item.parts[partIndex].block])
			}
		} else {
			ordered = append(ordered, a.message.Content[item.block])
		}
	}
	a.message.Content = ordered
}
