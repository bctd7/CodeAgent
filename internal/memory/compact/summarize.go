package compact

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"CodeAgent/internal/llm"
)

// Summarize 显式执行计划，不判断自动触发阈值，不保存或应用摘要。
func Summarize(ctx context.Context, plan Plan, options Options) (Result, error) {
	var result Result
	if ctx == nil {
		return result, failure(InvalidInput, fmt.Errorf("context is required"))
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	sources := plan.Input.Sources
	if plan.HistoryEnd < 0 || plan.HistoryEnd > plan.FirstKeptIndex || plan.FirstKeptIndex < 0 ||
		plan.FirstKeptIndex > 0 && plan.FirstKeptIndex >= len(sources) {
		return result, failure(InvalidInput, fmt.Errorf("invalid plan boundaries"))
	}
	if plan.FirstKeptIndex == 0 {
		return result, nil
	}
	historySources := sources[:plan.HistoryEnd]
	turnPrefix := sources[plan.HistoryEnd:plan.FirstKeptIndex]
	if options.Stream == nil || strings.TrimSpace(options.Model.ID) == "" || strings.TrimSpace(options.Model.Provider) == "" || options.Model.API == "" {
		return result, failure(InvalidInput, fmt.Errorf("explicit model and stream are required"))
	}
	if options.Model.ContextWindow <= 0 || options.Model.MaxTokens < 0 || options.RequestOptions.MaxTokens < 0 {
		return result, failure(InvalidInput, fmt.Errorf("context window must be positive; output caps cannot be negative"))
	}

	// 先准备两个请求，避免第二段已知超窗口时仍调用第一段。
	var historyRequest, prefixRequest llm.Request
	var err error
	if len(historySources) != 0 {
		historyRequest, err = makeRequest(historySources, plan.Input.PreviousSummary, false, plan.Input.Settings, options)
		if err != nil {
			return result, err
		}
	}
	if len(turnPrefix) != 0 {
		prefixRequest, err = makeRequest(turnPrefix, "", true, plan.Input.Settings, options)
		if err != nil {
			return result, err
		}
	}
	history := plan.Input.PreviousSummary
	if len(historySources) != 0 {
		var usage llm.Usage
		history, usage, err = callSummary(ctx, historyRequest, options.Stream)
		result.Usage = usage
		if err != nil {
			return result, err
		}
	} else if history == "" {
		history = "No prior history."
	}
	summary := history
	if len(turnPrefix) != 0 {
		prefix, usage, err := callSummary(ctx, prefixRequest, options.Stream)
		result.Usage = sumUsage(result.Usage, usage)
		if err != nil {
			return result, err
		}
		summary += splitSeparator + prefix
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Compacted, result.Summary = true, summary
	result.FirstKeptIndex = plan.FirstKeptIndex
	return result, nil
}

func makeRequest(sources []Source, previous string, prefix bool, settings Settings, options Options) (llm.Request, error) {
	// 整数计算 floor(0.8*n)，避免乘法溢出或浮点舍入。
	cap := settings.ReserveTokens/5*4 + settings.ReserveTokens%5*4/5
	if prefix {
		cap = settings.ReserveTokens / 2
	}
	if options.Model.MaxTokens > 0 && cap > options.Model.MaxTokens {
		cap = options.Model.MaxTokens
	}
	if options.RequestOptions.MaxTokens > 0 && cap > options.RequestOptions.MaxTokens {
		cap = options.RequestOptions.MaxTokens
	}
	if cap <= 0 {
		return llm.Request{}, failure(InvalidInput, fmt.Errorf("derived summary output cap must be positive"))
	}
	prompt, err := summaryPrompt(sources, previous, prefix)
	if err != nil {
		return llm.Request{}, failure(InvalidInput, err)
	}
	// 仅启发式检查本摘要请求的文本。协议包装的精确预算由调用层检查。
	inputTokens, err := addTokens(charsToTokens(textChars(summarySystem)), charsToTokens(textChars(prompt)))
	if err != nil {
		return llm.Request{}, failure(InvalidInput, err)
	}
	if cap > options.Model.ContextWindow || inputTokens > options.Model.ContextWindow-cap {
		return llm.Request{}, failure(InputTooLarge, fmt.Errorf("estimated summary input %d plus output cap %d exceeds context window %d", inputTokens, cap, options.Model.ContextWindow))
	}
	requestOptions := options.RequestOptions // 只隔离需要修改的标量；嵌套配置共享只读。
	requestOptions.MaxTokens = cap
	return llm.Request{
		Model: options.Model, SystemPrompt: summarySystem, Options: requestOptions,
		Messages: []llm.Message{&llm.UserMessage{
			Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.TypeText, Text: prompt}},
		}},
	}, nil
}

func callSummary(ctx context.Context, request llm.Request, stream StreamFunc) (string, llm.Usage, error) {
	if err := ctx.Err(); err != nil {
		return "", llm.Usage{}, err
	}
	reader, err := stream(ctx, request)
	if reader == nil {
		if cause := ctx.Err(); cause != nil {
			return "", llm.Usage{}, cause
		}
		if err == nil {
			err = fmt.Errorf("stream returned a nil reader")
		}
		return "", llm.Usage{}, failure(ModelFailed, err)
	}
	var closeOnce sync.Once
	closeReader := func() { closeOnce.Do(reader.Close) }
	defer closeReader()
	if cause := ctx.Err(); cause != nil {
		return "", llm.Usage{}, cause
	}
	if err != nil {
		return "", llm.Usage{}, failure(ModelFailed, err)
	}
	// Result 不依赖事件消费；取消时主动 Close，解除其阻塞。
	stop := context.AfterFunc(ctx, closeReader)
	defer stop()
	response := reader.Result()
	usage := response.Message.Usage
	if err := ctx.Err(); err != nil {
		return "", usage, err
	}
	if response.InternalErr != nil {
		return "", usage, failure(ModelFailed, response.InternalErr)
	}
	m := response.Message
	if m.State != llm.StateSuccess {
		return "", usage, failure(ModelFailed, fmt.Errorf("summary state %q: %s", m.State, m.ErrorMessage))
	}
	if m.StopReason != llm.StopStop {
		return "", usage, failure(Rejected, fmt.Errorf("summary stopped with %q", m.StopReason))
	}
	var text strings.Builder
	for _, block := range m.Content {
		switch block.Type {
		case llm.TypeText:
			if !utf8.ValidString(block.Text) {
				return "", usage, failure(Rejected, fmt.Errorf("invalid summary UTF-8"))
			}
			text.WriteString(block.Text)
		case llm.TypeThinking:
			// 推理不是可安装的摘要，包括 redacted 内容。
		default:
			return "", usage, failure(Rejected, fmt.Errorf("unexpected summary content %q", block.Type))
		}
	}
	summary := strings.TrimSpace(text.String())
	if summary == "" {
		return "", usage, failure(Rejected, fmt.Errorf("empty summary"))
	}
	return summary, usage, nil
}

func sumUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		Input: a.Input + b.Input, Output: a.Output + b.Output,
		CacheRead: a.CacheRead + b.CacheRead, CacheWrite: a.CacheWrite + b.CacheWrite,
		Reasoning: a.Reasoning + b.Reasoning, Total: a.Total + b.Total,
		Cost: llm.Cost{
			Input: a.Cost.Input + b.Cost.Input, Output: a.Cost.Output + b.Cost.Output,
			CacheRead: a.Cost.CacheRead + b.Cost.CacheRead, CacheWrite: a.Cost.CacheWrite + b.Cost.CacheWrite,
			Total: a.Cost.Total + b.Cost.Total,
		},
	}
}
