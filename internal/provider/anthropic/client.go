package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"CodeAgent/internal/llm"
)

// Client 是 Anthropic Messages 适配器。
// 它负责参数校验、请求装配与启动后台流式处理 goroutine。
type Client struct {
	httpClient *http.Client
}

// NewClient 构造一个 Client，可注入自定义 http.Client（传入 nil 时使用默认客户端）。
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{httpClient: httpClient}
}

// Stream 发起一次流式请求，立即返回读端（StreamReader）。
// 建流前的配置校验与请求体编码失败直接返回 error。
func (c *Client) Stream(ctx context.Context, request llm.Request) (llm.StreamReader, error) {
	if strings.TrimSpace(request.Model.BaseURL) == "" {
		return nil, fmt.Errorf("anthropic 缺少 base_url")
	}
	if strings.TrimSpace(request.Model.ID) == "" {
		return nil, fmt.Errorf("anthropic 缺少 model id")
	}
	endpoint := strings.TrimRight(request.Model.BaseURL, "/") + "/messages"
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
