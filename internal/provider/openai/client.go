package openai

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"CodeAgent/internal/llm"
)

// Client 是 OpenAI-compatible Chat Completions 适配器。
// 它不直接干活（HTTP/SSE/解析），只负责"装配 + 起后台 goroutine"。
// 真正的活在 stream.go 的 run 里。
type Client struct {
	httpClient *http.Client
	envKey     string // 读哪个环境变量拿 API key（来自 config 的 provider.env_key）
}

// NewClient 构造一个 Client。envKey 是读 API key 的环境变量名（由 config 层传入）。
func NewClient(envKey string) *Client {
	return &Client{httpClient: &http.Client{}, envKey: envKey}
}

// Stream 发起一次流式请求，立即返回读端（StreamReader）。
// 真正的 HTTP/SSE 工作在后台的 run goroutine 里异步进行。
// 建流前的配置校验与请求体编码失败直接返回 error。
func (c *Client) Stream(ctx context.Context, request llm.Request) (llm.StreamReader, error) {
	if strings.TrimSpace(request.Model.BaseURL) == "" {
		return nil, fmt.Errorf("base_url 不能为空")
	}
	endpoint := strings.TrimRight(request.Model.BaseURL, "/") + "/chat/completions"
	body, err := buildRequestBody(request)
	if err != nil {
		return nil, err
	}
	initial := llm.AssistantMessage{
		Role:      llm.RoleAssistant,
		Provider:  request.Model.Provider,
		API:       request.Model.API,
		Model:     request.Model.ID,
		State:     llm.StatePending,
		Timestamp: time.Now(),
	}
	reader, writer := llm.NewEventStream(ctx, initial)
	go c.run(writer, request, endpoint, body, initial)
	return reader, nil
}
