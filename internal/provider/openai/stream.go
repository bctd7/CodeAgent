package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"CodeAgent/internal/llm"
	"CodeAgent/internal/provider/sse"
)

// defaultMaxSSEFrameSize 是单个 SSE 帧的大小上限。
const defaultMaxSSEFrameSize = 1 << 20 // 1MB

// abortOrFail 统一处理"取消就 Abort，否则 Fail"的收尾。
// 出错时使用当前累积消息 msg 收尾，保留已生成的内容。
func abortOrFail(writer llm.StreamWriter, msg llm.AssistantMessage, err error) {
	if cause := writer.Context().Err(); cause != nil {
		writer.Abort(cause)
		return
	}
	msg.ErrorMessage = err.Error()
	writer.Fail(msg, nil)
}

// parseHTTPError 读取最多 64 KiB 的 HTTP 错误响应体，优先提取服务端错误消息。
// 不输出请求头、凭据或完整请求体。
func parseHTTPError(resp *http.Response) error {
	const maxErrBodySize = 64 << 10 // 64 KiB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
	if err != nil {
		return fmt.Errorf("HTTP %d (读取响应体失败: %w)", resp.StatusCode, err)
	}

	errMsg := extractErrorMessage(body)
	if errMsg != "" {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, errMsg)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

func extractErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var errWire struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &errWire); err == nil {
		if errWire.Error != nil && strings.TrimSpace(errWire.Error.Message) != "" {
			return strings.TrimSpace(errWire.Error.Message)
		}
		if strings.TrimSpace(errWire.Message) != "" {
			return strings.TrimSpace(errWire.Message)
		}
	}

	if len(body) <= 512 {
		trimmed := strings.TrimSpace(string(body))
		if trimmed != "" && !strings.Contains(trimmed, "<html") && !strings.Contains(trimmed, "<HTML") {
			return trimmed
		}
	}
	return ""
}

// run 在后台 goroutine 里执行整个请求-响应流程。
func (c *Client) run(writer llm.StreamWriter, request llm.Request, endpoint string, body []byte, initial llm.AssistantMessage) {
	// 1. 构造并发送 HTTP 请求，检查响应。
	req, err := http.NewRequestWithContext(writer.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		abortOrFail(writer, initial, err)
		return
	}
	// 先设置协议默认请求头，再应用配置中的 headers，允许网关显式覆盖。
	req.Header.Set("Content-Type", "application/json")
	if request.Options.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+request.Options.APIKey)
	}
	for k, v := range request.Options.Headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		abortOrFail(writer, initial, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		abortOrFail(writer, initial, parseHTTPError(resp))
		return
	}

	// 2. 初始化完整的累积消息，发布 EventMessageStart。
	accumulator := newResponseAccumulator(writer, request.Model.Pricing, initial)
	if err := accumulator.emitEvent(llm.EventMessageStart, 0, ""); err != nil {
		return
	}

	// 3. 读取 SSE 帧，由适配器判断 [DONE] 结束条件，将每帧的 JSON 数据解码为 Chunk 并累积消息。
	err = sse.Read(resp.Body, defaultMaxSSEFrameSize, func(frame sse.Frame) (bool, error) {
		if bytes.Equal(bytes.TrimSpace(frame.Data), []byte("[DONE]")) {
			return true, nil
		}
		var chunk chatCompletionChunk
		if err := json.Unmarshal(frame.Data, &chunk); err != nil {
			return false, err
		}
		return false, accumulator.applyChunk(chunk)
	})
	if err != nil {
		// Emit 失败时流内核已经终止，重复收尾不会覆盖其结果。
		abortOrFail(writer, accumulator.message, err)
		return
	}

	// 4. sse.Read 正常返回后，检查结束原因并完成内容块。
	stopReason, err := accumulator.completeBlocks()
	accumulator.message.StopReason = stopReason
	if err != nil {
		abortOrFail(writer, accumulator.message, err)
		return
	}

	// 5. 调用 writer.Finish(message)。
	accumulator.message.State = llm.StateSuccess
	writer.Finish(accumulator.message)
}
