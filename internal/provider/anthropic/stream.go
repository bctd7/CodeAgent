package anthropic

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

// defaultMaxSSEFrameSize 是单个 SSE 帧的大小上限（1MB）。
const defaultMaxSSEFrameSize = 1 << 20

// abortOrFail 统一处理“取消就 Abort，否则 Fail”的收尾。
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
		Type  string `json:"type"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
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

// run 在后台 goroutine 里执行整个 Anthropic HTTP 请求-响应流程。
func (c *Client) run(writer llm.StreamWriter, request llm.Request, endpoint string, body []byte, initial llm.AssistantMessage) {
	// 1. 构造并发送 HTTP 请求
	req, err := http.NewRequestWithContext(writer.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		abortOrFail(writer, initial, err)
		return
	}

	// 先设置协议默认请求头，再应用配置中的 headers，允许网关显式覆盖
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if request.Options.APIKey != "" {
		req.Header.Set("x-api-key", request.Options.APIKey)
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

	// 2. 初始化累积器
	accumulator := newResponseAccumulator(writer, request.Model.Pricing, initial)

	// 3. 读取 SSE 帧并累积消息
	err = sse.Read(resp.Body, defaultMaxSSEFrameSize, func(frame sse.Frame) (bool, error) {
		if len(bytes.TrimSpace(frame.Data)) == 0 {
			return false, nil
		}
		var event responseEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			return false, fmt.Errorf("解析 Anthropic SSE 帧失败: %w", err)
		}
		if err := accumulator.applyEvent(event); err != nil {
			return false, err
		}
		if accumulator.receivedMessageStop {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		abortOrFail(writer, accumulator.message, err)
		return
	}

	// 4. message_stop 才表示完整结束；此前断流报告失败
	if !accumulator.receivedMessageStop {
		abortOrFail(writer, accumulator.message, fmt.Errorf("流在收到 message_stop 之前中断"))
		return
	}

	// 5. 检查并映射结束原因
	stopReason, err := mapStopReason(accumulator.message.RawStopReason)
	accumulator.message.StopReason = stopReason
	if err != nil {
		abortOrFail(writer, accumulator.message, err)
		return
	}

	// 6. 成功结束
	accumulator.message.State = llm.StateSuccess
	writer.Finish(accumulator.message)
}
