# 独立 MCP 客户端

依赖：`github.com/modelcontextprotocol/go-sdk v1.8.0`，Go 最低版本为 1.25。本包使用官方 SDK 处理协议，不依赖 Runtime 或内部工具适配。

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
c, err := mcp.ConnectStdio(ctx, mcp.StdioConfig{
    ID: "local-files", Command: "configured-mcp-server",
    Args: []string{"--stdio"},
})
if err != nil { return err }
defer c.Close()

// HTTP 连接改用 ConnectHTTP；认证可由调用方注入 HTTPClient 的 transport 提供。
// c, err := mcp.ConnectHTTP(ctx, mcp.HTTPConfig{
//     ID: "remote-files", Endpoint: "http://127.0.0.1:8080/mcp",
// })

requestCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
defer stop()
tools, err := c.ListTools(requestCtx) // 取得全部分页，原名和 schema 不变。
if err != nil { return err }
_ = tools
result, err := c.CallTool(requestCtx, "read_file", json.RawMessage(`{"path":"example.txt"}`))
if err != nil { return err }
// result.Content 包含 SDK 原生文本、图像、音频、资源等内容。
// result.StructuredContent 和 result.IsError 仍可独立处理。
_ = result
```

每个连接是独立实例，`ID()` 用于上层区分服务器；本包不改写或拼接工具名。构造函数完成 SDK 初始化/版本协商；新协议由 SDK 执行 discover，旧协议由 SDK 回退到 initialize/initialized。连接 context 只限制建立连接，每个操作另传 context 控制取消与超时。

`ListTools` 每次重新请求完整分页列表，本包通过 SDK 中间件将列表 TTL 设为零，避免缓存命中。SDK 仍保存分页条目；若服务器持续生成新游标且不发送列表变化通知，旧条目可能积累，这是当前 SDK 没有公开禁缓存/清空接口的限制。`ToolsChanged()` 是容量为一的合并通知频道，收到通知后由调用方再次调用 `ListTools`，不在 SDK 通知回调中发请求。通知不保证每次变化都独立交付，也不能构成跨分页原子快照；服务器不支持通知时仍可再次调用 `ListTools` 获取最新列表。`Done()` 仅表示本地关闭；服务器断连在后续操作的错误中体现。关闭后通知频道不关闭，消费者应同时监听 `Done()` 或自己的 context。

工具执行错误保留在成功接收的结果 `IsError` 内；Go `*mcp.Error` 的 Kind 分为 input、canceled、protocol 和 connection。JSON-RPC 错误保留为可 `errors.As` 的官方 `*jsonrpc.Error`，取消/超时可 `errors.Is`。SDK 的非结构化协商或解码错误没有公开分类，归 connection，原错误仍保留；Kind 无法判断副作用是否发生。

`tools/call` 只发送一次，不自动重放。显式禁用 SDK 默认的 MultiRoundTrip 自动重试，原样返回新版 `NeedsInput()`、`InputRequests` 和 `RequestState`，由上层决定是否继续；本包不处理输入请求。禁用 SDK SSE 自动重连和 HTTP 重定向；SSE 断流后会话可能失效，需要调用方显式关闭并重新连接，本包不会重放未决调用。不配置 OAuth 或 elicitation。自定义 HTTP transport 必须遵守不重试 POST、尊重请求 context 的契约。

取消只表示客户端放弃等待，SDK 取消通知为尽力发送，不代表服务器撤销操作。stdio 管道写入尚未结束时取消会关闭整条连接，避免复用可能已发送半条消息的流。Close 先取消本包在途操作并关闭底层连接以解除堵塞 I/O，再回收 SDK 会话；HTTP 会话终止 DELETE 保持独立的 SDK 超时（最长约 5 秒），不随在途请求一起取消。HTTP 包装关联连接生命周期，涵盖 SDK 初始化使用的独立监听 context；成功连接后原连接 context 的取消不影响会话。stdio 由 SDK 关闭 stdin、等待退出、必要时终止/杀死并回收自有直接子进程；不保证清理服务器自行创建的后代进程。命令直接执行，不经过 shell；不能用 shell 命令串充当 Command。阻塞的自定义 stderr writer 无法由客户端强制解除。

## 渐进式工具目录

`Catalog` 是独立的本地发现目录。调用方显式调用 `Client.ListTools`，再用 `Update(serverID, tools)` 替换该服务器的整份快照；`Remove(serverID)` 移除来源。目录不会联网、启动服务器、监听通知或自动更新，不提供调用与授权能力。

```go
var catalog mcp.Catalog // 零值可用；可并发调用，使用后不要复制。
if err := catalog.Update(c.ID(), tools); err != nil { return err }
candidates, err := catalog.Search("查找文件", 5)
if err != nil { return err }
if len(candidates) > 0 {
    definition, err := catalog.Get(candidates[0].Key)
    if err != nil { return err }
    _ = definition // 按需取得完整 SDK 定义，未来由 Runtime 决定如何装载。
}
// catalog.Remove(c.ID())
```

`ToolKey{ServerID, Name}` 保留来源 ID 与原工具名，不拼接字符串。不同来源允许同名；同来源重名、空工具或无法复制的定义会使整次 `Update` 失败，旧快照保持不变。快照更新只影响对应来源，空列表清空其工具。`Get` 查询不存在的键时可用 `errors.Is(err, mcp.ErrToolNotFound)` 判断。

`Search(query, limit)` 只索引名称、完整简介和来源，不索引 schema、标题或元数据。忽略大小写，按 Unicode 字母/数字分词，支持连续中文子串；多个关键词任一命中即可。评分只取最高命中等级：完整名称精确 600、完整名称前缀 500、名称分词 400、名称子串 300、简介子串 200、来源子串 100。同分按原始 `ServerID`、`Name` 升序排列。空白查询返回同样稳定排序的目录（评分为零）；`limit` 必须为 1–100，没有隐式默认值。候选只有结构化键、简介和评分，简介最多 200 个 Unicode 字符，完整 schema 只从 `Get` 返回。

目录在更新时序列化完整定义并保存独立 JSON 副本，`Get` 每次解码为独立的 SDK `Tool`。注解、图标、元数据、输入和输出 schema 均保留；schema 和元数据使用 JSON 原生 map/slice 等值，数字为 `json.Number`，不会在复制中转成 `float64`。`json.RawMessage` 或自定义 Go schema 类型会转换为 JSON 原生值，不保留其自定义 Go 类型；输入已经发生的精度损失无法恢复。调用方不得在 `Update` 进行期间并发修改输入，调用结束后修改输入或返回值不会改变目录。

这是渐进式工具目录机制。实际模型的按需装载需要未来 Runtime 配合，本轮未接入，因此不会减少当前模型请求的实际 token。这里借鉴 Amadeus `catalogs.go` / `lazy_tool.go` 的发现与执行边界、Pi `ai/deferred_tools.go` 的延迟定义装载边界，不引入它们的 Runtime、调用或审批框架。

范围外：Resources、Prompts、OAuth 登录 UI、服务器安装、未配置服务器启动、自动审批和 Runtime 接入。

参考：

- [官方稳定版本 v1.8.0](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0)
- [官方客户端与分页实现](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/mcp/client.go)
- [官方 stdio 子进程生命周期](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/mcp/cmd.go)
- [官方 Streamable HTTP](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/mcp/streamable.go)
- [官方默认多轮重试及禁用选项](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/mcp/mrtr.go)
- [OpenCode MCP 协议客户端与上层工具转换](https://github.com/anomalyco/opencode/blob/dev/packages/opencode/src/mcp/index.ts)

OpenCode 的 `McpTool` 直接提供原生定义与 SDK 客户端，交由消费者适配；其 `connectTransport` 在连接失败时关闭传输。本包采用同样的职责边界和失败清理原则。现有显式管理和适配能力见下文；审批、Runtime 注册和执行记录仍由上层负责。

## 显式多服务管理与工具适配

`config.Config.MCPServers()` 只校验并复制 TOML 配置，不发起连接。MCP 项目默认
`enabled=false`；只有明确启用、用 `mcp.NewManager(specs)` 构造管理器并调用
`manager.Start(ctx)` 后才连接。
Start 返回聚合错误时，成功服务仍可用，应同时检查 `Statuses()`，不要丢弃部分成功。

`Definitions()` 返回完整独立定义、来源、连接代次及定义版本。适配包
`tools/extend/mcp.New(manager, definitions, reservedNames)` 返回工具和明确拒绝列表，
必须检查拒绝列表。reservedNames 包含最终注册中的全部其他工具名；名称由结构化键
生成稳定摘要，执行按原始来源/名称路由，不拆分模型可见名称。当前尚未注册到 Runtime。

管理器是每条连接工具变化通知的唯一消费者。同服务发现串行执行，发布时检查代次
和关闭状态，防止旧刷新覆盖重连或关闭后复活。定义未变的刷新保持版本；定义变化和
重连拒绝旧绑定。无通知服务显式 `Refresh(ctx,id)`；刷新失败撤下该服务工具，成功刷新
后恢复。断连也撤下工具；`Reconnect(ctx,id)` 是显式操作，不重放任何未决调用。
`CloseServer(id)` 只关闭该服务，`Close()` 取消所有管理活动并等待 watcher 退出。

管理连接默认初始化 10 秒，发现/调用 30 秒，零值使用默认，负值或超过 24 小时拒绝。
完整发现预算为 256 页、10000 个 SDK 返回的工具，超限不返回部分快照。SDK 自身会
过滤非法工具，因此本 API 不提供未经处理的原始 wire 列表。stdio Env 继续遵守 nil
继承、非 nil 完整替换；command 是直接可执行文件名/路径，不执行 shell 字符串。

`Client.Wait()` 观察 SDK 连接终止，`Done()` 保留仅表示本地主动关闭的语义。状态及
ServerError 的可打印摘要不含 endpoint、headers、env 或原始 SDK 错误文本；Unwrap
保留原始错误用于机器判断，不应在没有凭据脱敏的情况下记录它。注入 transport 必须
响应 context、不自动重试 POST，stderr writer 必须不阻塞；本包不能强制中止任意自定义 I/O。

适配支持文本、图片（SDK 二进制数据只编码一次 base64）、StructuredContent 的显式
JSON 文本。仅投影模型可见 Content，保持顺序，结构化正文在末尾追加。
音频、资源链接、嵌入资源、sampling 内容及等待用户输入的结果明确报 unsupported，
不伪装成功、不静默跳过、不塞入大量二进制文本。此时远端调用已经返回，副作用可能
已发生，不应据此自动重试。工具结果 IsError=true 转为 Go 错误，仅保留最多 2048 个
Unicode 字符的文本诊断；当前 Agent Execute 无法同时传递失败标记、原始多模态内容
和 Details，不能声称错误转换无损。远端 hint 不授予授权或并行安全，初版工具默认串行。

含 x-mcp-header 的工具明确拒绝，但其他普通工具不受影响。SDK 会在 CallTool 中参考
过期分页缓存，因此同一连接代次中曾用该扩展的名称持续禁用，即使当前定义去掉注解；
显式重连建立新 SDK 缓存后解除历史标记。这是第一版限制，不是对该扩展的完整支持。

适配层默认不将结果或内容的 _meta、Annotations 原始对象写入模型正文。Audience 缺省、空或包含 assistant 时保留正文；仅 user 或其他非 assistant 受众先排除，错误诊断遵循相同规则。没有可见正文时说明服务已返回且无可展示内容，不重放调用。协议层原始结果仍保留；当前适配接口没有客户端元数据输出通道，不承诺完整保留到 Agent。
