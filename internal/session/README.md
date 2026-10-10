# Session

独立保存会话事实、宿主上下文、压缩记录、子任务关系及长期记忆整理进度；不依赖 Agent、Hooks、Runtime 或 memory，不执行任何任务。

当前数据库版本为 **5**，恰好七张表：`sessions`、`runs`、`entries`、`tool_executions`、`compactions`、`child_tasks`、`memory_progress`。`Store.Init` 只初始化空库或验证本版本结构，旧版明确拒绝，不迁移、回填或删除数据。

## 入口

- `Service` 管理会话；`Session` 绑定单个会话，管理 Run 与记录。
- `StartRun` 原子保存 Run 与真实用户输入；`AppendMessage` 追加用户或模型原始消息。
- `RecordToolStart` 保存最终参数与启动记录；`AppendToolResult` 接受 `config.ToolResultInput`，显式记录工具状态和是否执行，原子保存原始结果及关联。它不再接受仅有消息、需要猜测执行状态的旧参数。
- `CompleteToolProcessing` 单次确认模型可见内容：nil 保留原文，非 nil 替换正文，空切片表示明确清空。原始 Message 不变。
- `AppendContext` 保存带来源的宿主反馈，`Kind=context` 明确区别于用户输入。
- `GetCurrentHistory` 返回原始记录与独立处理字段；`GetContext` 返回有稳定 EntryID 的模型可见记录。pending 项 Message 为 nil，绝不自动回退原文。
- `SaveCompaction` / `LatestCompaction` 保存、选择当前分支适用的摘要，不执行压缩算法。
- `CreateChildTask` / `GetChildTask` / `ListChildTasks` 保存父子归属；`DeliverChildResult` 原子追加带子 Run 来源的宿主上下文，同一父 Run 的同一子结果去重。
- `GetMemoryProgress` / `NextMemoryBatch` / `AdvanceMemoryProgress` 读进度、选固定来源批次、成功后条件推进。不建任务队列、不调用 longterm。

## 重要契约

输入及返回记录使用期间共享只读；调用方不得并发修改嵌套消息、切片、map。来源必须由可信宿主提供，不能直接信任模型填写父会话、来源 ID 或进度。

工具的 `StartedAt` 为可空指针；无启动记录的拒绝、跳过等结果不生成虚假时间。仅有启动记录时 `Executed=nil`，不能认定副作用发生。工具的成功、失败和取消状态不从 IsError 猜测，最终状态与 IsError 必须一致。`deferred` 只结束当前 Run，不自动重放。

压缩来源使用有序 Entry 引用，允许调用方提供处理后可见消息的子序列；Session 只校验引用、顺序、未重复覆盖前缀与分支，不替 Agent 做过滤或工具配对。摘要只在生成时 Head 仍为当前 Head 祖先时适用。

整理进度按 `(session_id, scope_key)` 保存成功覆盖边界。ScopeKey 由宿主稳定绑定一个记忆目标；不能把同一个 key 改指不同目录。正常完成指 `success + stop` 的主 Run；子 Run、失败、取消、截断和延期不独立进入自动整理。宿主上下文不冒充用户消息进入来源。来源固定到已结束 Run，消息记录不能再追加。

分支切换通过祖先交集判断覆盖；回到旧分支可能再次返回固定批次，由 longterm 回执去重。仅在 Consolidate 成功或 Duplicate 后推进；Session 无法证明外部记忆已提交。没有自动重试线程、定时器或持久化任务队列。

完整调用顺序、事务边界和验证限制见 [Session 与长期记忆接入契约](../../docs/Session与长期记忆接入契约.md)。这些能力尚未接入 Runtime。
