# OpenAI Responses 独立适配器

当前包路径为 "CodeAgent/internal/provider/openai/responses"，Go 包名为 "responses"。下方验收记录里的 "internal/provider/openairesponses" 为迁移前历史路径，保留原始运行记录。

本包实现 `POST {BaseURL}/responses` 与 SSE 到 `llm.StreamReader` 的转换。仅依赖标准库、`internal/llm` 和原有 `internal/provider/sse`，依据官方协议独立实现。

没有注册 `internal/provider.Client` 的默认路由，没有修改全局 API 枚举、配置验证、main、Runtime、Agent 或 Session。现有 Chat Completions 的配置和行为保持原状。`openai-responses` 目前只在本包的显式装配中使用，不能直接填入当前 Runtime 配置后期待路由生效。

## 调用入口与装配示例

```go
const APIKind llm.APIKind = "openai-responses"
func NewClient(httpClient *http.Client) *Client
func (c *Client) Stream(ctx context.Context, request llm.Request) (llm.StreamReader, error)
func Stream(ctx context.Context, request llm.Request) (llm.StreamReader, error)
```

下例仅说明包内装配，不修改实际配置；`modelID`、`apiKey` 由可信调用方显式传入。

```go
request := llm.Request{
    Model: llm.Model{
        Provider: "openai",
        API: responses.APIKind,
        ID: modelID,
        BaseURL: "https://api.openai.com/v1",
        Reasoning: true,
    },
    Messages: []llm.Message{
        &llm.UserMessage{Content: []llm.ContentBlock{
            {Type: llm.TypeText, Text: "你好"},
        }},
    },
    Options: llm.RequestOptions{
        APIKey: apiKey,
        MaxTokens: 1024,
        OpenAI: &llm.OpenAIOptions{ReasoningEffort: "high"},
    },
}
message, err := llm.Complete(ctx, request, responses.Stream)
// 必须同时检查 err、message.State 和 message.StopReason。
```

模型能力和 effort 的合法取值由调用方选择、服务端校验；本包不按模型名称猜测能力。`Model.API` 为空时在请求副本中设置为本包协议，显式指定其他协议则拒绝。BaseURL 应包含 API 根路径（例如 `/v1`），不包含 `/responses`、用户信息、查询参数或片段。

请求固定 `stream:true`、`store:false`。SystemPrompt 映射到 `instructions`；MaxTokens 先使用请求值，再使用模型值，统一发送 `max_output_tokens`。`MaxTokensField` 是 Chat Completions 的选项，在此不生效。Temperature 按显式值发送。启用 Model.Reasoning 或提供 ReasoningEffort 时发送 `reasoning.summary:"auto"` 和兼容用的 `include:["reasoning.encrypted_content"]`。

认证仅从 RequestOptions 的 APIKey/Headers 获取，不读取环境、磁盘配置或用户密钥，不写日志。显式 APIKey 优先于 Headers.Authorization；Content-Type 和 Accept 固定为本协议所需值。HTTP 客户端采用传入 client 的副本并禁用自动重定向。输入校验错误同步返回；HTTP/SSE/协议错误进入 `Result.Message`，不冒充 InternalErr。

## 实际支持范围

- user：文本和 base64 PNG/JPEG/WebP/GIF 图片，转换为 `input_text` / `input_image`。
- assistant：文本、由本包记录且来源匹配的 reasoning item、普通同步 function call。保留已知的 `commentary` / `final_answer` phase；文本按原输出项分组回放。
- tool result：`function_call_output`，保留 ToolCallID 为 `call_id`，output 原生包含 `input_text` / `input_image` 数组；空结果为字符串 `""`。不注入合成 user 消息。ModelContent 的选择仍由上层历史投影负责，本包只读取 Content。
- tool 定义：`type:function`、name、description、parameters；显式 `strict:false`，不修改已有 schema 的可选字段。
- 输出：message 的 output_text/refusal、reasoning 的 summary/encrypted_content、function_call。工具参数使用 `json.Decoder.UseNumber`，返回和回放时保留大 JSON 数值精度。

处理生命周期事件、output_item added/done、content_part added/done、output_text delta/done、refusal delta/done、function_call_arguments delta/done、reasoning_summary_part added/done、reasoning_summary_text delta/done、completed/failed/incomplete 和顶层 error。

output_index、item_id、call_id 分别核验，支持并行交错调用。done 中的内容视为完整快照，只补尚未收到的后缀，重复 done 不重复文本或块结束事件，冲突快照明确失败。实时 BlockIndex 指向该事件 Snapshot 的 Content；块按首次出现分配索引，最终消息按服务端 output/content 索引排列。

未知通知先按 type 分流，不强套已知事件字段；未知工具调用事件和不支持的输出 item/content 明确失败。文本 annotations/logprobs 不投影。没有内置 web search、服务端工具、命名空间工具、异步/程序间接调用、音频、文件、WebSocket、OAuth、自动重试或服务端会话续接。

## Thinking 与来源

不需要扩展领域结构：`Thinking` 只含协议实际提供的摘要文本；`ThinkingSignature` 存储带 `openai-responses/reasoning-v1:` 前缀的本包回放封装，包含原生 item 的 id、summary 分段、可选 encrypted_content/status，以及 Provider/API/请求模型/BaseURL 来源。它是本地协议元数据，不宣称是 OpenAI 提供的签名，也不生成或解码隐藏推理。

只有 output_item.done 或确定终态的完成 reasoning item 可以写入回放封装。added 中可能不完整的密文被忽略；后续终态省略可选字段时保留此前 done 已确认的值。来源四项及消息来源必须同时匹配，摘要必须未被修改，消息必须成功且未截断。普通 thinking 文本、其他协议签名、来源不匹配、失败/取消/截断的 reasoning 均不回放；原历史保持不变。

`TextSignature` 使用独立的 `openai-responses/text-v1:` 封装保存输出 message ID、phase 与来源。以上字符串元数据可以随现有 ContentBlock 的复制/JSON 保存保留；不修改 Agent/Session 的存储结构。调用方遵守已有只读历史契约；来源标记不是防篡改认证。本包只保证其 Stream 返回及直接回放的大整数精度，不改变其他包自行 JSON 解码时的数值策略。

## 终态与用量

| 服务端结果 | 统一结果 |
| --- | --- |
| completed，纯文本/摘要 | success + stop |
| completed，合法 function calls | success + tool_use |
| incomplete / max_output_tokens | success + length；保留部分内容，不修补截断参数 |
| refusal 或 incomplete / content_filter | error + safety，拒绝文本保留在 Content |
| failed / error / 未知 incomplete 原因 / 协议错误 / 缺少终态的 EOF | error |
| 父 context 取消/超时，或 Reader.Close | aborted，保留最新已发布快照 |

RawStopReason 保存终态 status，或 incomplete_details.reason；不把 HTTP 成功、item.done 或 `[DONE]` 当作请求成功。Next 的等待 context 取消不取消整条流。所有终态经现有 Finish/Fail/Abort 收口，消费事件与获取 Result 相互独立。

Usage.Input 是 input_tokens 扣除 cached_tokens/cache_write_tokens 后的部分，CacheRead/CacheWrite 独立计入输入分类，Output 包含 reasoning_tokens，Reasoning 仅作子集展示。Total 校验为 input_tokens + output_tokens；不再次加缓存或 reasoning。成本调用现有 llm.CalculateCost，异常计数明确失败。

SSE 使用原有 1 MiB 单帧上限。错误响应最多读取 64 KiB，只提取结构化错误，不回显原始响应体；发送头中的凭据值在错误文案中脱敏。流快照复制 Content，已发布参数 map 保持只读。

## 验证与官方依据

本任务使用临时 httptest 假 SSE，无真实 API 请求或真实 key。验收包括文本/final-only、工具结果图片、并行调用与大整数、参数分片、重复/冲突 done、reasoning/phase 回放及来源过滤、refusal、usage、failed/error/incomplete、取消/Close/Next 等待取消、畸形/超长 SSE、未知事件/工具、提前 EOF 和输入不变。临时测试完成后按任务要求删除本任务测试文件。

最终验收记录：

- `go test ./internal/provider/openairesponses -run TestTemp -count=1 -v`：10 组全部通过，含 30 个协议表驱动分支；也验证了可省略 function item ID、缺失摘要 text 的拒绝处理。
- 删除本任务临时测试后，`go build ./internal/provider/openairesponses`、`go vet ./internal/provider/openairesponses`、`go test ./... -run '^$'` 均退出 0。最后一条只做全仓编译，不执行其他包测试。
- `responses_temp_test.go` 和本任务专用的 `openairesponses-build-temp` / `openairesponses-build-cache` 均已删除并确认不存在，没有清理其他任务文件。
- 实测环境为 Windows amd64、Go 1.26.3；CGO_ENABLED=0，未运行 race。中途执行环境断连已恢复，恢复后的最新源码已完成上述验收，无剩余环境阻塞。

- [创建 Responses 请求与参数](https://developers.openai.com/api/reference/python/resources/responses/methods/create)
- [SSE 事件 schema](https://developers.openai.com/api/reference/resources/responses/streaming-events)
- [原生 function output 文本/图片 schema](https://developers.openai.com/api/reference/python/__sdk_schema?declaration=%28resource%29+responses+%3E+%28model%29+response_function_tool_call_output_item+%3E+%28schema%29&selected=%28resource%29+responses)
- [Reasoning item schema](https://developers.openai.com/api/reference/python/__sdk_schema?declaration=%28resource%29+responses+%3E+%28model%29+response_reasoning_item+%3E+%28schema%29&selected=%28resource%29+responses)
- [Reasoning 使用说明](https://developers.openai.com/api/docs/guides/reasoning)

核对日期：2026-10-02。实现范围以上述子集为准；未宣称通过真实模型或兼容网关验收。
