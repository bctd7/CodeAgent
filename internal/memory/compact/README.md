# 上下文压缩

`Prepare` 校验已投影的有序消息，计算连续保留尾部，以及待总结的历史和当前轮前缀。`Summarize` 使用调用方明确指定的模型和流回调生成摘要。没有待总结前缀时返回 `Compacted=false`，不调用模型；存在前缀时，手动调用不受自动触发阈值限制。

触发、读库、持久化、安装摘要和模型选择属于后续 Runtime/Session 接入。本包不依赖父包 memory，也没有接入主执行链。输入、计划、嵌套参数及回调参数共享只读；Go 不冻结这些对象，也不保证并发修改安全。修改请求输出上限只做 RequestOptions 值复制。

```go
plan, err := compact.Prepare(compact.Input{
    Sources: sources, // 上次保留的原文尾部 + 新消息，首次为当前有效路径。
    PreviousSummary: previousSummary,
    Settings: compact.DefaultSettings(),
})
if err != nil { return err }
result, err := compact.Summarize(ctx, plan, compact.Options{
    Model: summaryModel,
    RequestOptions: requestOptions,
    Stream: stream,
})
if err == nil && result.Compacted {
    // 后续调用方保存 result.Summary，保留 plan.Retained。
    // 持久化及历史路径复核成功后，再安装上下文。
}
```

后续压缩不得再次传入旧摘要已覆盖的原文。失败时 Summary 与成功边界为空，但 Usage 可以包含已经发生的消耗。Stream 回调须响应 context，reader.Close 须解除 Result 阻塞。

核心切点和摘要组织参考 [Pi 固定提交 1cedd327](https://github.com/earendil-works/pi/blob/1cedd32724abfcb0915f76cc61b6827e2c16dbad/packages/coding-agent/src/core/compaction/compaction.ts)：倒序累计消息估算量，在累计位置处或之后选最近合法切点，没有后续切点时用最后合法切点；普通摘要一次，split turn 最多两次，直接拼接。默认 ReserveTokens=16384、KeepRecentTokens=20000，由 DefaultSettings 显式提供。KeepRecentTokens=0 保留参考算法的零值行为；ReserveTokens 太小导致摘要上限为零时，在调用模型前报错。

适配差异：

- 只接受本项目 User、Assistant、ToolResult；检查来源 ID 和工具配对。未完成工具组及未终止 assistant 后的范围不可切开；未完成工具组中插入其他消息、孤立结果、名称冲突及活动调用 ID 冲突明确报错。已经完成的调用 ID 可复用。
- 使用 UTF-16 单位模拟 JavaScript length，消息字符数/4 向上取整；图片块按 4800 字符估算。它不是 tokenizer，中文估算也不保证保守。EstimatedTokensBefore 只含旧摘要与原消息，不含主对话系统提示和工具定义。
- 提示词按本项目重写，工具参数使用 Go 的 JSON 序列化。工具结果文本截断到最多 2000 UTF-16 单位，非 BMP 字符不拆开；图片仅标记存在且未经视觉总结，不传 base64、Details、签名或不透明 redacted 数据。
- 历史输出上限为 floor(0.8 × ReserveTokens)，轮次前缀为 floor(0.5 × ReserveTokens)，分别取 Model.MaxTokens（正值）及 RequestOptions.MaxTokens（正值）的更小值。0 表示未另设上限，负值拒绝，不修改原请求选项。
- 本摘要请求先按系统提示及转录文本近似检查模型窗口；不自动分块或重试。实际协议包装以及应用后的完整主请求预算仍由调用层检查。
- 不移植 Pi 的插件、context_edit、分支导航摘要、文件操作追踪、模型选择或预算恢复机制。
