package responses

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"CodeAgent/internal/llm"
)

type partState struct {
	kind  string
	block int
	value string
	done  bool
}

type itemState struct {
	kind, id, callID, name, phase string
	block                         int
	parts                         map[int]*partState
	args                          string
	argsDone, done                bool
	argsErr                       error
	reasoning                     *outputItem
}

// output_index 与 item_id 双向核对；call_id 只用于工具配对，不拿 item_id 替代它。
type accumulator struct {
	writer   llm.StreamWriter
	model    llm.Model
	message  llm.AssistantMessage
	items    map[int]*itemState
	ids      map[string]int
	callIDs  map[string]int
	refusal  bool
	terminal bool
}

func newAccumulator(writer llm.StreamWriter, model llm.Model, message llm.AssistantMessage) *accumulator {
	return &accumulator{writer: writer, model: model, message: message,
		items: make(map[int]*itemState), ids: make(map[string]int), callIDs: make(map[string]int)}
}

func (a *accumulator) emit(kind llm.EventType, index int, delta string) error {
	snapshot := a.message
	snapshot.Content = append([]llm.ContentBlock(nil), a.message.Content...)
	// 参数 map 一经发布即不再修改；后续 done 只替换整张 map。
	return a.writer.Emit(llm.Event{Type: kind, BlockIndex: index, Delta: delta, Snapshot: &snapshot})
}

func indexValue(index *int) (int, error) {
	if index == nil || *index < 0 {
		return 0, fmt.Errorf("Responses 事件缺少非负索引")
	}
	return *index, nil
}

func (a *accumulator) eventItem(event streamEvent, kind string) (*itemState, error) {
	index, err := indexValue(event.OutputIndex)
	if err != nil {
		return nil, err
	}
	item := a.items[index]
	if item == nil || event.ItemID == "" || item.kind != kind {
		return nil, fmt.Errorf("Responses 事件的 output_index/item_id/类型不匹配")
	}
	if item.id == "" && item.kind == "function_call" {
		if previous, ok := a.ids[event.ItemID]; ok && previous != index {
			return nil, fmt.Errorf("Responses item_id 出现在不同 output_index")
		}
		item.id, a.ids[event.ItemID] = event.ItemID, index
	}
	if item.id != event.ItemID {
		return nil, fmt.Errorf("Responses 事件的 output_index/item_id 不匹配")
	}
	return item, nil
}

func (a *accumulator) apply(event streamEvent) (bool, error) {
	switch event.Type {
	case "response.created", "response.in_progress", "response.queued":
		if event.Response == nil {
			return false, fmt.Errorf("Responses 生命周期事件缺少 response")
		}
		return false, a.metadata(event.Response)
	case "response.output_item.added", "response.output_item.done":
		index, err := indexValue(event.OutputIndex)
		if err != nil {
			return false, err
		}
		if event.Item == nil {
			return false, fmt.Errorf("Responses 输出事件缺少 item")
		}
		return false, a.applyItem(index, *event.Item, event.Type == "response.output_item.done")
	case "response.content_part.added", "response.content_part.done":
		item, err := a.eventItem(event, "message")
		if err != nil {
			return false, err
		}
		index, err := indexValue(event.ContentIndex)
		if err != nil {
			return false, err
		}
		if event.Part == nil {
			return false, fmt.Errorf("Responses 内容事件缺少 part")
		}
		return false, a.fullPart(item, index, *event.Part, event.Type == "response.content_part.done")
	case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done":
		item, err := a.eventItem(event, "message")
		if err != nil {
			return false, err
		}
		index, err := indexValue(event.ContentIndex)
		if err != nil {
			return false, err
		}
		kind, value := "output_text", event.Text
		if strings.HasPrefix(event.Type, "response.refusal.") {
			kind, value = "refusal", event.Refusal
		}
		done := strings.HasSuffix(event.Type, ".done")
		if !done {
			value = event.Delta
		}
		if value == nil {
			return false, fmt.Errorf("Responses 文本事件缺少内容")
		}
		return false, a.textPart(item, index, kind, *value, done, done)
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		item, err := a.eventItem(event, "function_call")
		if err != nil {
			return false, err
		}
		done := event.Type == "response.function_call_arguments.done"
		value := event.Delta
		if done {
			value = event.Arguments
		}
		if value == nil {
			return false, fmt.Errorf("Responses 参数事件缺少内容")
		}
		return false, a.arguments(item, *value, done, done)
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		item, err := a.eventItem(event, "reasoning")
		if err != nil {
			return false, err
		}
		index, err := indexValue(event.SummaryIndex)
		if err != nil {
			return false, err
		}
		done := strings.HasSuffix(event.Type, ".done")
		full := done || event.Type == "response.reasoning_summary_part.added"
		value := event.Delta
		if strings.Contains(event.Type, "summary_part.") {
			if event.Part == nil || event.Part.Type != "summary_text" {
				return false, fmt.Errorf("不支持的 reasoning summary part")
			}
			value = event.Part.Text
		} else if done {
			value = event.Text
		}
		if value == nil {
			return false, fmt.Errorf("reasoning summary 事件缺少内容")
		}
		return false, a.summary(item, index, *value, full, done)
	case "response.completed", "response.failed", "response.incomplete":
		if event.Response == nil {
			return false, fmt.Errorf("Responses 终态事件缺少 response")
		}
		return true, a.finish(event.Type, event.Response)
	case "error":
		return false, fmt.Errorf("Responses error (%s): %s", event.Code, event.Message)
	default:
		// 未知通知可向前兼容；实际输出项仍经 applyItem 严格判别，不能伪装支持服务端工具。
		return false, nil
	}
}

func (a *accumulator) register(index int, output outputItem) (*itemState, error) {
	if output.ID == "" && output.Type != "function_call" {
		return nil, fmt.Errorf("Responses 输出项缺少 id")
	}
	if previous, ok := a.ids[output.ID]; output.ID != "" && ok && previous != index {
		return nil, fmt.Errorf("Responses item_id 出现在不同 output_index")
	}
	if item := a.items[index]; item != nil {
		if (item.id != "" && output.ID != "" && item.id != output.ID) || item.kind != output.Type {
			return nil, fmt.Errorf("Responses output_index 被不同输出项复用")
		}
		if item.kind == "function_call" && (item.callID != output.CallID || item.name != output.Name) {
			return nil, fmt.Errorf("Responses 工具身份发生变化")
		}
		if item.id == "" && output.ID != "" {
			item.id, a.ids[output.ID] = output.ID, index
		}
		return item, nil
	}
	item := &itemState{kind: output.Type, id: output.ID, callID: output.CallID, name: output.Name,
		phase: output.Phase, block: -1, parts: make(map[int]*partState)}
	switch output.Type {
	case "message":
	case "reasoning":
		item.block = len(a.message.Content)
		a.message.Content = append(a.message.Content, llm.ContentBlock{Type: llm.TypeThinking})
	case "function_call":
		if output.CallID == "" || output.Name == "" {
			return nil, fmt.Errorf("Responses 工具缺少 call_id/name")
		}
		if _, ok := a.callIDs[output.CallID]; ok {
			return nil, fmt.Errorf("Responses 重复工具 call_id")
		}
		a.callIDs[output.CallID] = index
		item.block = len(a.message.Content)
		a.message.Content = append(a.message.Content, llm.ContentBlock{Type: llm.TypeToolCall, ID: output.CallID, Name: output.Name})
	default:
		return nil, fmt.Errorf("openairesponses: 不支持的输出项类型 %q（含服务端工具）", output.Type)
	}
	a.items[index] = item
	if output.ID != "" {
		a.ids[output.ID] = index
	}
	if item.kind == "reasoning" {
		if err := a.emit(llm.EventThinkingBlockStart, item.block, ""); err != nil {
			return nil, err
		}
	} else if item.kind == "function_call" {
		if err := a.emit(llm.EventToolCallBlockStart, item.block, ""); err != nil {
			return nil, err
		}
	}
	return item, nil
}

func (a *accumulator) applyItem(index int, output outputItem, done bool) error {
	if output.Status != "" && output.Status != "in_progress" && output.Status != "completed" && output.Status != "incomplete" {
		return fmt.Errorf("不支持的 Responses item status %q", output.Status)
	}
	if output.Type == "message" && (output.Role != "assistant" || (output.Phase != "" && output.Phase != "commentary" && output.Phase != "final_answer")) {
		return fmt.Errorf("Responses 输出 message 不是支持的 assistant/phase")
	}
	if output.Type == "function_call" && (output.Namespace != "" || output.Async || (output.Caller != nil && output.Caller.Type != "direct")) {
		return fmt.Errorf("尚不支持命名空间、异步或程序间接工具调用")
	}
	if output.Type == "reasoning" && len(output.Content) > 0 {
		return fmt.Errorf("仅支持 reasoning summary/encrypted_content，不支持 reasoning content")
	}
	item, err := a.register(index, output)
	if err != nil {
		return err
	}
	if item.done && !done {
		return fmt.Errorf("已完成的 Responses 输出项再次 added")
	}
	switch item.kind {
	case "message":
		if output.Phase == "" {
			output.Phase = item.phase
		}
		if item.phase != "" && item.phase != output.Phase {
			return fmt.Errorf("Responses assistant phase 发生变化")
		}
		item.phase = output.Phase
		if done && output.Content == nil {
			return fmt.Errorf("Responses 完成的 message 缺少 content")
		}
		if done && len(item.parts) > len(output.Content) {
			return fmt.Errorf("Responses 最终 message 丢失内容分段")
		}
		for partIndex, part := range output.Content {
			if err := a.fullPart(item, partIndex, part, done); err != nil {
				return err
			}
		}
		signature := saveTextMetadata(output, a.model)
		for _, part := range item.parts {
			a.message.Content[part.block].TextSignature = signature
		}
	case "function_call":
		if output.Arguments == nil {
			if done {
				return fmt.Errorf("Responses 完成的工具项缺少 arguments")
			}
		} else if err := a.arguments(item, *output.Arguments, true, done); err != nil {
			return err
		}
	case "reasoning":
		if done && output.Summary == nil {
			return fmt.Errorf("Responses reasoning 缺少 summary")
		}
		if done && len(item.parts) > len(output.Summary) {
			return fmt.Errorf("Responses 最终 reasoning 丢失摘要分段")
		}
		for partIndex, part := range output.Summary {
			if part.Type != "summary_text" || part.Text == nil {
				return fmt.Errorf("不支持的 reasoning 摘要类型 %q", part.Type)
			}
			if err := a.summary(item, partIndex, *part.Text, true, done); err != nil {
				return err
			}
		}
		// added 中的密文可能是不完整前缀；只保存 done 或确定终态中的完成项。
		if done && (output.Status == "" || output.Status == "completed") {
			// 终态可省略可选字段；保留此前 done 已确认的密文，不使用 added 的前缀。
			if item.reasoning != nil {
				if output.EncryptedContent == nil {
					output.EncryptedContent = item.reasoning.EncryptedContent
				}
				if item.reasoning.EncryptedContent != nil && output.EncryptedContent != nil && *item.reasoning.EncryptedContent != *output.EncryptedContent {
					return fmt.Errorf("Responses reasoning done 密文冲突")
				}
				if output.Status == "" {
					output.Status = item.reasoning.Status
				}
			}
			signature, err := saveReasoning(output, a.model)
			if err != nil {
				return err
			}
			block := &a.message.Content[item.block]
			block.ThinkingSignature = signature
			copy := output
			item.reasoning = &copy
		}
		if done && !item.done {
			if err := a.emit(llm.EventThinkingBlockEnd, item.block, ""); err != nil {
				return err
			}
		}
	}
	item.done = item.done || done
	return nil
}

// done 是快照而非增量；仅补充尚未收到的后缀，重复 done 幂等，冲突快照报错。
func reconcile(previous, value string, full, closed bool) (string, error) {
	if full {
		if !strings.HasPrefix(value, previous) || (closed && value != previous) {
			return "", fmt.Errorf("Responses 最终内容与已接收内容冲突")
		}
		return strings.TrimPrefix(value, previous), nil
	}
	if closed {
		return "", fmt.Errorf("Responses 已完成内容收到后续 delta")
	}
	return value, nil
}

func (a *accumulator) fullPart(item *itemState, index int, part outputPart, done bool) error {
	value := part.Text
	if part.Type == "refusal" {
		value = part.Refusal
	}
	if value == nil {
		return fmt.Errorf("Responses content part 缺少内容")
	}
	return a.textPart(item, index, part.Type, *value, true, done)
}

func (a *accumulator) textPart(item *itemState, index int, kind, value string, full, done bool) error {
	if kind != "output_text" && kind != "refusal" {
		return fmt.Errorf("不支持的 Responses content 类型 %q", kind)
	}
	part := item.parts[index]
	if part == nil {
		if item.done {
			return fmt.Errorf("已完成的 message 出现新 content part")
		}
		part = &partState{kind: kind, block: len(a.message.Content)}
		item.parts[index] = part
		a.message.Content = append(a.message.Content, llm.ContentBlock{Type: llm.TypeText})
		if kind == "refusal" {
			a.refusal = true
		}
		if err := a.emit(llm.EventTextBlockStart, part.block, ""); err != nil {
			return err
		}
	}
	if part.kind != kind {
		return fmt.Errorf("Responses content_index 类型冲突")
	}
	delta, err := reconcile(part.value, value, full, part.done)
	if err != nil {
		return err
	}
	part.value += delta
	a.message.Content[part.block].Text = part.value
	if delta != "" {
		if err := a.emit(llm.EventTextDelta, part.block, delta); err != nil {
			return err
		}
	}
	if done && !part.done {
		part.done = true
		return a.emit(llm.EventTextBlockEnd, part.block, "")
	}
	return nil
}

func (a *accumulator) summary(item *itemState, index int, value string, full, done bool) error {
	part := item.parts[index]
	if part == nil {
		if item.done {
			return fmt.Errorf("已完成的 reasoning 出现新摘要分段")
		}
		part = &partState{}
		item.parts[index] = part
	}
	delta, err := reconcile(part.value, value, full, part.done)
	if err != nil {
		return err
	}
	part.value += delta
	part.done = part.done || done
	if delta == "" {
		return nil
	}
	var text strings.Builder
	for _, i := range sortedParts(item.parts) {
		text.WriteString(item.parts[i].value)
	}
	a.message.Content[item.block].Thinking = text.String()
	return a.emit(llm.EventThinkingDelta, item.block, delta)
}

func (a *accumulator) arguments(item *itemState, value string, full, done bool) error {
	delta, err := reconcile(item.args, value, full, item.argsDone)
	if err != nil {
		return err
	}
	item.args += delta
	if delta != "" {
		if err := a.emit(llm.EventToolCallDelta, item.block, delta); err != nil {
			return err
		}
	}
	if done && !item.argsDone {
		item.argsDone = true
		args, err := parseArguments(item.args)
		item.argsErr = err
		if err == nil {
			a.message.Content[item.block].Arguments = args
		}
		// 截断参数保持 nil，不修补为 {}；等 response 终态决定 length 或 error。
		return a.emit(llm.EventToolCallBlockEnd, item.block, "")
	}
	return nil
}

func parseArguments(raw string) (map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var args map[string]any
	if err := decoder.Decode(&args); err != nil || args == nil {
		return nil, fmt.Errorf("Responses 工具参数必须是完整 JSON 对象")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("Responses 工具参数包含多余 JSON 内容")
	}
	return args, nil
}

func sortedParts(parts map[int]*partState) []int {
	indices := make([]int, 0, len(parts))
	for index := range parts {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	return indices
}
