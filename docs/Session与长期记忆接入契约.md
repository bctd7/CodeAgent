# Session 与长期记忆接入契约

本文描述已实现的 Session、longterm 接口及未来 Runtime 必须遵循的交接，不代表 Runtime 已接入。Session／longterm 建设阶段未修改 Agent；随后单独授权补充了 Agent.ProjectHistory，接法见第 5.1 节。Hooks、LLM、Provider、基础工具集合不在这些修改范围内。

## 1. 职责与边界

| 组件 | 负责 | 不负责 |
|---|---|---|
| longterm.Service | 已提交读取、只读工具协议、整理业务 | AgentLoop、会话调度、来源进度 |
| longterm/storage | 文件校验、暂存、提交、恢复、同目录协调 | 模型执行、Session、审批 |
| Session | 会话事实、上下文记录、摘要引用、父子关系、进度 | 模型循环、Hook 执行、长期记忆文件 |
| 未来 Runtime | 装配、Driver 交接、调用已有接口、资源生命周期 | SQL、记忆文件操作、第二套执行循环 |

不新增 memory_jobs、通用事件表、审批日志表或模型请求快照表。不增加旧接口转发、旧数据库迁移及双模式整理开关。

## 2. longterm 公开接口

```go
List(ctx context.Context) ([]string, error)
Read(ctx context.Context, name string) (Document, error)
Search(ctx context.Context, query string, limit int) ([]Match, error)
ReadTools() []llm.Tool
CallReadTool(ctx context.Context, call llm.ContentBlock) (*llm.ToolResultMessage, error)
Consolidate(ctx context.Context, batch Batch) (ConsolidationResult, error)
Close() error
```

### 2.1 前台读取

宿主创建绑定到明确记忆目录的 Service，把 ReadTools 的定义与 CallReadTool 回调适配为 Agent 工具。longterm 不导入 Agent，基础四工具构造器不受影响。

模型先接收用户消息，再判断是否检索。可依次调用 memory_list/search/read，也可以不检索直接处理任务。工具结果进入正常消息上下文；不自动把全部记忆塞进 system prompt，不启动额外检索模型。

只读目录与后台对应三工具的名称、描述和 schema 相同。调用入口校验 Type、调用 ID、工具名、必需参数、未知参数和类型。前台拒绝 write/edit/delete；单次失败不污染其他读取，也不修改后台事务的失败标记。

List 返回排序主题清单并验证主题格式。Read 支持 MEMORY.md 和主题文件；前者是链接与短摘要，不是全部正文。索引缺失返回空内容，主题缺失保留 os.ErrNotExist。Search 沿用字面匹配、行号、最多 1000 字符片段及结果数量规则；不承诺语义召回。

Runtime 本轮绑定项目记忆，同一项目的不同会话共享，由模型自主检索。不增加跨项目功能或额外工具组，不能由模型输入任意目录。一个工具适配器的 Service 绑定由宿主保持固定。

### 2.2 后台整理

Consolidate 继续构建 Task，并交给 Options.Execute。宿主将来使用独立 Agent 执行 Task，工具仅为本批六工具；逐轮输入、实时输出、回合数和工具批次预算由执行适配器落实。

六工具继续操作 Transaction：读取可见本批暂存结果，修改旧主题须先读，来源 ID 只能来自可信输入或实际读取的旧主题；任一工具领域错误导致整批失败。前台不能取得这个 Transaction 或长期保留 Task.CallTool。

Execute 必须在返回前完成资源收尾。成功执行不等于记忆已提交；宿主须检查 Consolidate 返回值及 error。发布 pending 后的错误仍按 ErrCommitPending 表达，不能报告已回滚。

### 2.3 锁和恢复

同目录实例共享两类锁，固定顺序为整理锁→数据访问锁：

1. UpdateBatch 获取整理锁，串行保护整个整理任务。
2. 在数据访问锁内恢复已发布提交、读取成功回执，随后释放数据锁。
3. Execute 操作私有暂存；期间前台可读取旧的完整已提交内容。
4. 提交前再次获取数据锁，完成发布和文件写入后释放。
5. 前台 List/Read/Search 只获取数据锁；如有已发布提交，先恢复再读。

每个读取调用提供完整已提交视图；不同调用之间允许新提交。锁等待支持 context 取消；已发布提交的恢复不被普通取消拆断。损坏日志明确失败并保留，不返回部分结果。

Close 不与操作并发。跨进程和外部直接编辑仍由宿主协调；本轮没有新增跨进程锁、缓存或版本库。

## 3. Session 七张表

数据库版本为 5。Init 初始化空库或严格验证当前版本的七表及索引；旧结构明确报错，不自动迁移、清空或补写。

| 表 | 关键数据与约束 |
|---|---|
| sessions | 元数据、工作目录、模型引用、Head |
| runs | 所属会话、起始 Head、输入与末记录、终态与次数 |
| entries | ParentID、Sequence、Run、原始消息、Kind/来源、处理状态及独立可见正文 |
| tool_executions | assistant Entry + call ID 主键、最终参数、状态、Executed、可空启动时间、结果引用 |
| compactions | 来源 Head、前一摘要、有序来源 ID、FirstKeptIndex、摘要、模型来源和用量 |
| child_tasks | 任务 ID、父会话与发起 Run、唯一子会话、角色、文件范围 |
| memory_progress | 会话 + 范围主键、成功覆盖 Entry、更新时间 |

Sequence 只表示写入顺序。所有历史适用性检查必须基于 ParentID 路径，不能把其他分支按 Sequence 拼接进来。

## 4. 消息、工具和处理结果

### 4.1 原始事实

StartRun 原子写 Run 和真实用户输入。AppendMessage 只接收用户或 Assistant 原始消息；Assistant 必须附带实际模型来源。RecordStep 记录实际模型调用尝试。

RecordToolStart 接收最终参数，保存启动事实，不证明工具已经产生副作用。只有启动记录时 Executed 为 nil、结果引用为空，不可自动重试。

AppendToolResult 改为接收 config.ToolResultInput，显式提供 Result、Arguments、Status、Executed、ErrorMessage。验证所属调用、工具名、参数一致性及 Run 未结束；结果 Entry、执行终态、结果引用、Run 末位置和会话 Head 在同一事务提交。

无启动记录的拒绝、不可用、跳过、延期或执行前取消也可以记录结果，StartedAt 保持 nil。宣称已执行必须已有启动记录。Succeeded/Failed 必须为 Executed=true；Denied/Unavailable/Skipped/Deferred 必须为 false；Canceled 可发生在执行前或执行中。非成功工具结果须 IsError=true。

FinishRun 接受 deferred，StopReason 必须为空；不创建恢复或重放任务。其他终态沿用现有约束。

### 4.2 Hook 处理交接

1. AppendToolResult 保存原始结果，Processing=pending。
2. 宿主执行成功或失败工具 Hook。
3. CompleteToolProcessing 确认一次：nil 表示 original，非 nil 表示 replaced；空切片表示明确移除正文。
4. 原始 Message、ToolCallID、工具名、IsError 不被覆盖；仅模型可见 Content 可以替换。

重复确认返回冲突。处理结果在 Run 活跃期间确认，不能恢复时自动补跑 Hook。

GetCurrentHistory 返回原始记录及处理字段。GetContext 返回稳定 EntryID、Kind、来源、处理状态、模型可见 Message 与 Origin。pending 项的 Message 为 nil，调用方必须显式处理，禁止静默回退敏感原文。此查询不做模型特定过滤、工具配对或压缩。

### 4.3 宿主上下文

AppendContext 接收 SourceKind、SourceID 和 user 形状的消息。模型协议使用 user 形状，持久记录以 Kind=context 明确它不是实际用户输入。来源事件在同一 Run 内唯一，重复普通反馈返回冲突。

child_run 是保留来源类别，只允许 DeliverChildResult 写入。Session 不接受通过普通上下文入口伪造子任务交付。

## 5. 压缩记录

SaveCompaction 接收 HeadID、PreviousID、有序 SourceIDs、FirstKeptIndex、Summary、Origin、Usage。SourceIDs 是本次使用的真实消息来源 Entry 引用，按 Session 祖先路径排列；生成说明不占一个 Entry。FirstKeptIndex 相对于这份真实来源列表，不一定等于 compact 投影列表中的切点。来源引用不是内容哈希，不要求 Session 重新生成处理后的正文。

校验：来源存在于该 Head 的祖先路径，唯一且顺序递增；FirstKeptIndex 必须覆盖至少一个来源且不超过输入长度；来源处理不能 pending；PreviousID 必须属于本会话且其 Head 在该路径上；本次输入不重复上次已覆盖前缀。

Session 不决定摘要触发、不切分工具批次、不调用 compact。调用方负责提供 compact 实际使用的有序来源。摘要不复制完整模型请求。

LatestCompaction 只选择其生成 Head 仍是当前 Head 祖先的摘要，优先覆盖更远边界；没有适用摘要返回 sql.ErrNoRows。对分支采用保守适用策略，不因为覆盖的某个较早片段相同就复用另一分支生成的摘要。

### 5.1 Agent 历史整理接线

Agent.ProjectHistory(history, targetModel, originMap) 返回 ProjectedMessage 列表，每项含 Message、SourceIndex、Generated。SourceIndex 指向本次输入 history；Generated=true 表示补出的缺失工具结果说明，此时来源指向所属 Assistant，不表示数据库存在这条工具结果。

宿主从 GetContext 构造同序记录／消息数组，pending 正文不加入输入；originMap 用这些实际 Assistant 对象建立。真实投影项通过 SourceIndex 直接查对应 EntryID。生成项只有本次 compact Sources 内的临时唯一 ID，不保存成 Entry，也不把所属 Assistant ID重复当作另一条真实来源。

compact 的切点按投影列表计算。保存时先划分覆盖／保留部分，再去除生成项，将真实来源按祖先路径排序；Session.FirstKeptIndex 换算为覆盖的真实来源数。工具组不能被 compact 切点拆开，因此组内结果重排不改变真实覆盖边界。禁止把两个列表的下标直接当成同一个。

应用摘要时，从真实覆盖末 Entry 之后选保留历史，再调用 ProjectHistory；通过已有 Driver 的 Decision.RequestContext 替换本次请求。后续请求重复应用已保存摘要与当前尾部，不要求修改 Agent 内部历史。详细步骤与示例见待做计划第 19 节。

该接口已经局部验证，Runtime 的实际持久化、取消、预算等接线仍未实现。

## 6. 子任务关系与交付

CreateChildTask 绑定活跃父 Run 与未运行、未归档的空子会话。角色仅 explore/worker；explore 不接受修改文件范围。文件范围保存宿主提供的值，实际路径规范化与占用协调不在 Session。

子会话不能继续创建孙任务。实际 Agent、取消、等待和权限继承均由后续 Runtime 负责。子会话继续使用普通 runs/entries，child_tasks 不复制运行状态。

GetChildTask/ListChildTasks 只返回绑定父会话拥有的关系。运行结果通过子会话的 GetRun 获取。

DeliverChildResult 要求具体子 Run 已结束且属于该任务子会话；宿主提供用于父上下文的摘要。新增交付时父 Run 必须活跃。

交付 Entry 同时保存来源关联与正文，唯一键为父 Run + child_run + 子 Run。同内容重试返回原 Entry，不追加消息；不同内容返回冲突。不同父 Run 可以显式交付同一已结束子结果，不复制子会话全部历史。

## 7. 长期记忆进度

ScopeKey 是宿主提供的稳定逻辑目标，绑定一个明确记忆目录，不能复用同一 key 改指另一个目录。

NextMemoryBatch 从当前历史路径选择最早一个未覆盖、正常完成的主 Run。正常完成限定为 success + stop；子会话、失败、取消、截断、延期不独立进入自动整理。返回 nil,nil 表示没有待处理批次。

批次包含 SessionID、ScopeKey、RunID、查询时 HeadID、Run 末 Entry、旧进度及该 Run 的原始消息记录。宿主上下文不伪装成用户来源。Run 已结束后不能再追加消息，因此同一批次来源固定。

Batch.ID 从版本标识、会话 ID 和 Run ID 确定性生成，内容不参与哈希匹配。对同一固定批次重试，ID 与来源保持不变。

处理步骤：

1. 读取 NextMemoryBatch，将 Entries 的 ID 与原始 Message 转成 longterm.Source。
2. 调用 Consolidate；本轮不提供自动拆分，超限或错误直接报告，进度不变。
3. error 为 nil 且成功或 Duplicate 时，调用 AdvanceMemoryProgress。
4. Advance 比较旧边界并重新验证固定路径上的下一批次，只允许顺序推进；过期或越批提交返回冲突。

Session 无法验证外部文件提交，调用方必须遵守成功后推进。若记忆已提交而进度未更新，下次重复固定批次会命中 longterm 回执，不再次执行成功批次。

旧进度位于另一分支时，仅旧覆盖路径与新路径共同祖先算已覆盖。回到整理过的旧分支可能再次返回旧批次，由 longterm 去重。这避免新增分支进度日志、任务队列或回执副本。

进度不意味着后台线程已存在。本轮没有定时器、自动重试线程或跨重启任务队列，触发由后续 Runtime 负责。

## 8. 验证与实施边界

备份及 SHA-256 基线在编辑前完成，按用户要求不反复执行哈希校验。测试放仓库外临时副本，使用假 Execute、临时数据库和临时记忆目录，不调用真实模型、外网、shell 业务或子代理任务。

验证包括目标包测试、只读/六工具契约、暂存隔离、后台执行期间读取、恢复、取消、同目录锁、原始与处理结果恢复、工具各终态、分支摘要、父子交付、进度与回执交接，以及 Windows/Linux 编译。

Linux 交叉编译不等于 Linux 上执行测试。Runtime 尚未接入；流式执行预算、真实模型语义、真实子任务执行及整项目健康不在本轮验证结论内。遇到范围外编译错误只报告，不扩大修改。

