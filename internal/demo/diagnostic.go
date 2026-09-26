package demo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type diagnosticTransport struct {
	base http.RoundTripper
}

// NewDiagnosticHTTPClient 构造带有出站参数与协议诊断的 http.Client。
// 它仅在内存中解析关键结构，用于核对出站字段与思考/工具回传，不输出凭据、鉴权头或完整请求体。
func NewDiagnosticHTTPClient() *http.Client {
	return &http.Client{
		Transport: &diagnosticTransport{
			base: http.DefaultTransport,
		},
	}
}

func (t *diagnosticTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	if len(bodyBytes) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(bodyBytes, &payload); err == nil {
			printOutboundDiagnostic(req, payload)
		}
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		fmt.Printf("<<< [HTTP 出站失败] 错误: %v\n", err)
		return nil, err
	}
	fmt.Printf("<<< [HTTP 响应状态] HTTP %d %s\n", resp.StatusCode, resp.Status)
	return resp, nil
}

func printOutboundDiagnostic(req *http.Request, p map[string]any) {
	fmt.Println("\n>>> [出站请求诊断 (脱敏)] >>>")
	fmt.Printf("  URL: %s %s\n", req.Method, req.URL.String())

	// 模型身份
	if m, ok := p["model"].(string); ok {
		fmt.Printf("  模型 ID: %s\n", m)
	}

	// 输出限制
	if maxT, ok := p["max_tokens"]; ok {
		fmt.Printf("  输出限制 (max_tokens): %v\n", maxT)
	}
	if maxCT, ok := p["max_completion_tokens"]; ok {
		fmt.Printf("  输出限制 (max_completion_tokens): %v\n", maxCT)
	}

	// 协议专属选项
	if effort, ok := p["reasoning_effort"]; ok {
		fmt.Printf("  OpenAI reasoning_effort: %v\n", effort)
	}
	if thinking, ok := p["thinking"]; ok {
		fmt.Printf("  Anthropic thinking: %v\n", thinking)
	}
	if outputCfg, ok := p["output_config"]; ok {
		fmt.Printf("  Anthropic output_config: %v\n", outputCfg)
	}

	// 工具定义
	if tools, ok := p["tools"].([]any); ok {
		var toolNames []string
		for _, t := range tools {
			if tm, ok := t.(map[string]any); ok {
				if f, ok := tm["function"].(map[string]any); ok {
					if n, ok := f["name"].(string); ok {
						toolNames = append(toolNames, n)
					}
				} else if n, ok := tm["name"].(string); ok {
					toolNames = append(toolNames, n)
				}
			}
		}
		fmt.Printf("  声明工具列表: %v\n", toolNames)
	}

	// 消息队列结构 (观察思考回传、签名、工具结果回传)
	if msgs, ok := p["messages"].([]any); ok {
		fmt.Printf("  消息数量: %d 条\n", len(msgs))
		for i, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)

			// 检查内容块
			switch c := mm["content"].(type) {
			case string:
				fmt.Printf("    [%d] role=%s (纯文本 content, 长度 %d 字节)\n", i, role, len(c))
			case []any:
				var blockTypes []string
				for _, b := range c {
					bm, ok := b.(map[string]any)
					if !ok {
						continue
					}
					bType, _ := bm["type"].(string)
					extra := ""
					if bType == "thinking" {
						sig, _ := bm["signature"].(string)
						if sig != "" {
							extra = "(带非空签名)"
						} else {
							extra = "(无签名)"
						}
					} else if bType == "tool_use" {
						id, _ := bm["id"].(string)
						name, _ := bm["name"].(string)
						extra = fmt.Sprintf("(id=%s, name=%s)", id, name)
					} else if bType == "tool_result" {
						tID, _ := bm["tool_use_id"].(string)
						isErr, _ := bm["is_error"].(bool)
						extra = fmt.Sprintf("(tool_use_id=%s, is_error=%v)", tID, isErr)
					}
					blockTypes = append(blockTypes, bType+extra)
				}
				fmt.Printf("    [%d] role=%s 内容块: %v\n", i, role, blockTypes)
			}

			// OpenAI 格式的工具调用
			if tc, ok := mm["tool_calls"].([]any); ok {
				var tcInfos []string
				for _, t := range tc {
					if tcm, ok := t.(map[string]any); ok {
						id, _ := tcm["id"].(string)
						tcInfos = append(tcInfos, id)
					}
				}
				fmt.Printf("    [%d] OpenAI tool_calls ID: %v\n", i, tcInfos)
			}
			if tID, ok := mm["tool_call_id"].(string); ok {
				fmt.Printf("    [%d] OpenAI tool_result for tool_call_id: %s\n", i, tID)
			}
		}
	}
	fmt.Println("<<< [出站请求诊断结束] <<<")
}
