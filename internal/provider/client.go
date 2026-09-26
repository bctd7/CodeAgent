package provider

import (
	"context"
	"fmt"
	"net/http"

	"CodeAgent/internal/llm"
	"CodeAgent/internal/provider/anthropic"
	"CodeAgent/internal/provider/openai"
)

// Client 统一调度各协议的适配器。
type Client struct {
	openaiClient    *openai.Client
	anthropicClient *anthropic.Client
}

// NewClient 构造统一调度客户端，允许注入自定义 *http.Client。
func NewClient(httpClient *http.Client) *Client {
	return &Client{
		openaiClient:    openai.NewClient(httpClient),
		anthropicClient: anthropic.NewClient(httpClient),
	}
}

// Stream 依据 Model.API 选择对应的适配器，使用直接分支调度。
func (c *Client) Stream(ctx context.Context, request llm.Request) (llm.StreamReader, error) {
	switch request.Model.API {
	case llm.APIKindOpenAI:
		return c.openaiClient.Stream(ctx, request)
	case llm.APIKindAnthropic:
		return c.anthropicClient.Stream(ctx, request)
	default:
		return nil, fmt.Errorf("不支持的 API 协议: %q", request.Model.API)
	}
}

var defaultClient = NewClient(nil)

// Stream 是默认的统一流式调度入口。
func Stream(ctx context.Context, request llm.Request) (llm.StreamReader, error) {
	return defaultClient.Stream(ctx, request)
}
