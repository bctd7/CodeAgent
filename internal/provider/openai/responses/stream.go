package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"CodeAgent/internal/llm"
	"CodeAgent/internal/provider/sse"
)

func (c *Client) run(writer llm.StreamWriter, model llm.Model, endpoint string, headers http.Header, body []byte, initial llm.AssistantMessage) {
	a := newAccumulator(writer, model, initial)
	fail := func(err error) {
		if cause := writer.Context().Err(); cause != nil {
			writer.Abort(cause)
			return
		}
		a.message.ErrorMessage = redactError(err.Error(), headers)
		writer.Fail(a.message, nil)
	}
	req, err := http.NewRequestWithContext(writer.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		fail(err)
		return
	}
	req.Header = headers
	resp, err := c.httpClient.Do(req)
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fail(httpError(resp))
		return
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		fail(fmt.Errorf("Responses 返回的 Content-Type 不是 text/event-stream"))
		return
	}
	if err := a.emit(llm.EventMessageStart, 0, ""); err != nil {
		return
	}
	err = sse.Read(resp.Body, sse.DefaultMaxFrameSize, func(frame sse.Frame) (bool, error) {
		if len(bytes.TrimSpace(frame.Data)) == 0 && !strings.HasPrefix(frame.Event, "response.") && frame.Event != "error" {
			return false, nil
		}
		if bytes.Equal(bytes.TrimSpace(frame.Data), []byte("[DONE]")) {
			return false, fmt.Errorf("Responses 缺少确定终态，不能以 [DONE] 完成")
		}
		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(frame.Data, &header) != nil || header.Type == "" {
			return false, fmt.Errorf("Responses SSE JSON 无效或缺少 type")
		}
		if frame.Event != "" && frame.Event != header.Type {
			return false, fmt.Errorf("Responses SSE event 与 JSON type 不一致")
		}
		if !supportedEvent(header.Type) {
			// 新通知的字段类型可能与旧事件不同，不能用已知事件结构解码后再忽略。
			name := strings.TrimPrefix(header.Type, "response.")
			if strings.HasPrefix(header.Type, "response.") && (strings.Contains(name, "_call") || strings.HasPrefix(name, "mcp_")) {
				return false, fmt.Errorf("openairesponses: 不支持的工具事件 %q", header.Type)
			}
			return false, nil
		}
		var event streamEvent
		if json.Unmarshal(frame.Data, &event) != nil {
			return false, fmt.Errorf("Responses SSE 事件字段无效")
		}
		done, err := a.apply(event)
		if err == nil && done {
			a.terminal = true
		}
		return done, err
	})
	if err != nil {
		fail(err)
		return
	}
	if !a.terminal {
		fail(fmt.Errorf("Responses SSE 提前 EOF：未收到 completed/failed/incomplete 终态"))
		return
	}
	if cause := writer.Context().Err(); cause != nil {
		writer.Abort(cause)
		return
	}
	if a.message.State == llm.StateSuccess {
		writer.Finish(a.message)
		return
	}
	a.orderContent()
	a.message.ErrorMessage = redactError(a.message.ErrorMessage, headers)
	writer.Fail(a.message, nil)
}

func supportedEvent(kind string) bool {
	switch kind {
	case "response.created", "response.in_progress", "response.queued",
		"response.output_item.added", "response.output_item.done",
		"response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.completed", "response.failed", "response.incomplete", "error":
		return true
	default:
		return false
	}
}

// 限制错误响应读取量，不回显原始 HTTP body、请求体或认证头。
func httpError(response *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("Responses HTTP %d：读取错误响应失败", response.StatusCode)
	}
	var envelope struct {
		Error *responseError `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error != nil {
		return fmt.Errorf("Responses HTTP %d (%s): %s", response.StatusCode, envelope.Error.Code, envelope.Error.Message)
	}
	return fmt.Errorf("Responses HTTP %d", response.StatusCode)
}

func redactError(message string, headers http.Header) string {
	for name, values := range headers {
		if name == "Accept" || name == "Content-Type" {
			continue
		}
		for _, value := range values {
			if value == "" {
				continue
			}
			message = strings.ReplaceAll(message, value, "[redacted]")
			if strings.EqualFold(name, "Authorization") {
				_, token, ok := strings.Cut(value, " ")
				if ok && token != "" {
					message = strings.ReplaceAll(message, token, "[redacted]")
				}
			}
		}
	}
	return message
}
