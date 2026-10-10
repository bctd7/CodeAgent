package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"CodeAgent/internal/llm"
	mcpcore "CodeAgent/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxDiagnosticRunes = 2048

// ReturnedError 表示远程调用已返回，不能据此安全重试。
// 当前 Execute 接口无法同时携带 IsError 和原始多模态内容/Details；错误仅保留有界文本诊断。
type ReturnedError struct{ Kind, Diagnostic string }

func (e *ReturnedError) Error() string {
	message := "MCP remote call returned: " + e.Kind + "; side effects may have occurred; no automatic retry was performed"
	if e.Diagnostic != "" {
		message += "; " + e.Diagnostic
	}
	return message
}

func convertResult(result *mcpcore.CallToolResult) ([]llm.ContentBlock, error) {
	if result == nil {
		return nil, &ReturnedError{Kind: "empty result"}
	}
	if result.NeedsInput() {
		return nil, &ReturnedError{Kind: "input required is unsupported; caller must resolve input explicitly", Diagnostic: textDiagnostic(result)}
	}
	if result.IsError {
		return nil, &ReturnedError{Kind: "tool reported an execution error (non-text content and raw details are not preserved)", Diagnostic: textDiagnostic(result)}
	}
	blocks := make([]llm.ContentBlock, 0, len(result.Content)+2)
	for i, content := range result.Content {
		if !modelVisible(content) {
			continue
		}
		switch c := content.(type) {
		case *sdk.TextContent:
			if c == nil {
				return nil, unsupportedResult(result, i, "nil text")
			}
			blocks = append(blocks, llm.ContentBlock{Type: llm.TypeText, Text: c.Text})
		case *sdk.ImageContent:
			if c == nil || c.MIMEType == "" || !strings.HasPrefix(strings.ToLower(c.MIMEType), "image/") {
				return nil, unsupportedResult(result, i, "invalid image")
			}
			blocks = append(blocks, llm.ContentBlock{Type: llm.TypeImage, Data: base64.StdEncoding.EncodeToString(c.Data), MimeType: c.MIMEType})
		case *sdk.AudioContent:
			return nil, unsupportedResult(result, i, "audio")
		case *sdk.ResourceLink:
			return nil, unsupportedResult(result, i, "resource_link")
		case *sdk.EmbeddedResource:
			return nil, unsupportedResult(result, i, "embedded_resource")
		case *sdk.ToolUseContent, *sdk.ToolResultContent:
			return nil, unsupportedResult(result, i, "sampling-only content")
		default:
			return nil, unsupportedResult(result, i, "unknown or nil content")
		}
	}
	// 结构化正文追加在可见 Content 之后；客户端 _meta/Annotations 不投影为模型正文。
	if result.StructuredContent != nil {
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return nil, &ReturnedError{Kind: "structured content cannot be encoded"}
		}
		blocks = append(blocks, llm.ContentBlock{Type: llm.TypeText, Text: "MCP structuredContent: " + string(data)})
	}
	if len(result.InputRequests) != 0 || result.RequestState != "" {
		return nil, &ReturnedError{Kind: "unexpected input continuation state is unsupported"}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, llm.ContentBlock{Type: llm.TypeText, Text: "服务已返回，但没有可供模型展示的内容"})
	}
	return blocks, nil
}

func modelVisible(content sdk.Content) bool {
	// 受众提示用于模型投影；缺省受众可见，明确受众须包含 assistant。
	var annotations *sdk.Annotations
	switch c := content.(type) {
	case *sdk.TextContent:
		if c != nil {
			annotations = c.Annotations
		}
	case *sdk.ImageContent:
		if c != nil {
			annotations = c.Annotations
		}
	case *sdk.AudioContent:
		if c != nil {
			annotations = c.Annotations
		}
	case *sdk.ResourceLink:
		if c != nil {
			annotations = c.Annotations
		}
	case *sdk.EmbeddedResource:
		if c != nil {
			annotations = c.Annotations
		}
	}
	if annotations == nil || len(annotations.Audience) == 0 {
		return true
	}
	return slices.Contains(annotations.Audience, "assistant")
}

func unsupportedResult(result *mcpcore.CallToolResult, index int, kind string) error {
	return &ReturnedError{Kind: fmt.Sprintf("unsupported content %s at index %d; original content is not preserved", kind, index), Diagnostic: textDiagnostic(result)}
}

func textDiagnostic(result *mcpcore.CallToolResult) string {
	var b strings.Builder
	remaining := maxDiagnosticRunes
	for _, item := range result.Content {
		c, ok := item.(*sdk.TextContent)
		if !ok || c == nil || !modelVisible(item) {
			continue
		}
		if b.Len() != 0 && remaining > 0 {
			b.WriteByte('\n')
			remaining--
		}
		for _, r := range c.Text {
			if remaining == 0 {
				return b.String() + " [truncated]"
			}
			b.WriteRune(r)
			remaining--
		}
	}
	return b.String()
}
