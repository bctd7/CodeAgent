// Package responses 提供独立的 OpenAI Responses HTTP/SSE 适配器。
// 本包不注册默认 provider 路由，也不读取配置、环境变量或用户密钥。
package responses

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"CodeAgent/internal/llm"
)

// APIKind 只用于显式构造请求，不扩展全局配置验证或路由。
const APIKind llm.APIKind = "openai-responses"

// Client 复用调用方的 HTTP 传输；认证仅来自可信的 RequestOptions。
type Client struct{ httpClient *http.Client }

// NewClient 创建独立客户端。禁止自动重定向，避免把请求体或凭据转送到其他端点。
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	copy := *httpClient
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{httpClient: &copy}
}

// Stream 返回统一读端；输入校验同步执行，HTTP/SSE 失败由 Result.Message 表达。
func (c *Client) Stream(ctx context.Context, request llm.Request) (llm.StreamReader, error) {
	if request.Model.API != "" && request.Model.API != APIKind {
		return nil, fmt.Errorf("openairesponses: 请求协议必须为 %s", APIKind)
	}
	request.Model.API = APIKind
	endpoint, err := responseEndpoint(request.Model.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Model.ID) == "" {
		return nil, fmt.Errorf("openairesponses: model 不能为空")
	}
	body, err := buildRequestBody(request)
	if err != nil {
		return nil, err
	}
	// 在返回前复制 Headers；后台不再访问输入历史、工具定义或可变选项。
	headers := make(http.Header)
	for key, value := range request.Options.Headers {
		headers.Set(key, value)
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	if request.Options.APIKey != "" {
		headers.Set("Authorization", "Bearer "+request.Options.APIKey)
	}
	initial := llm.AssistantMessage{
		Role: llm.RoleAssistant, Provider: request.Model.Provider, API: APIKind,
		Model: request.Model.ID, State: llm.StatePending, Timestamp: time.Now(),
	}
	reader, writer := llm.NewEventStream(ctx, initial)
	go c.run(writer, request.Model, endpoint, headers, body, initial)
	return reader, nil
}

func responseEndpoint(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u == nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Opaque != "" {
		return "", fmt.Errorf("openairesponses: base_url 必须为完整 HTTP(S) 地址")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("openairesponses: base_url 不允许用户信息、查询参数或片段")
	}
	return strings.TrimRight(baseURL, "/") + "/responses", nil
}

var defaultClient = NewClient(nil)

// Stream 是可由未来路由显式调用的包级入口。
func Stream(ctx context.Context, request llm.Request) (llm.StreamReader, error) {
	return defaultClient.Stream(ctx, request)
}
