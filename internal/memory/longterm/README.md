# 长期记忆

本包提供 Markdown 主题存储、索引、字面检索和有界模型整理。根包依赖标准库、`internal/llm` 与自己的 `storage` 子包；存储子包只依赖标准库。没有接入 Runtime、Session 或 Agent；主对话只读工具协议已提供，尚待宿主装配。

调用方提供目录、消息批次和 Execute 回调，负责何时调用、选择当前历史路径、分配用户/项目目录、主对话注入。不同会话打开同一目录即可共享主题；本包不认识 SessionID，不调度任务，不创建默认 provider，也不读取 API Key 环境变量。业务包保持平级独立，未来由 Runtime 统一装配和调度。

## 代码组织

```text
longterm/
  service.go                 已提交读取、三工具公开入口与整理流程
  types.go                   执行契约、配置、存储类型别名及预算默认值
  prepare.go                 来源文本、提示词及初始任务组装
  tools.go                   共用工具定义/参数解析，后台六工具与读后修改规则
  storage/
    store.go                 存储对象、双锁、已提交读取与底层文件访问
    document.go              Markdown 格式、校验、来源解析及索引
    transaction.go           批次入口、暂存读写检索、回执、提交日志与恢复
```

通过开发约定保持根包到 storage 的依赖方向；Runtime 只使用根包 Service。Service 提供 List、Read、Search、ReadTools、CallReadTool、Consolidate 和 Close。前三项访问已提交内容；ReadTools/CallReadTool 是主对话的三工具协议。Consolidate 通过 Store.UpdateBatch 串行整理；六工具只操作本批 Transaction，成功后由存储层登记回执与提交，失败丢弃未发布暂存。整理回调不能重入同目录 Consolidate。

六工具仍是批次整理协议：参数、可信来源、读取证据和首个领域失败由根包负责。工具经 Transaction.Read/Files/Search/Put/Forget 使用存储能力，不直接访问目录句柄、暂存映射或磁盘状态。Transaction.ReadCommittedIndex 用于初始任务的已提交索引；Read("MEMORY.md") 返回包含暂存修改的索引。

Document、Match、Entry、StorageLimits 使用存储类型别名；预算与待恢复错误复用同一错误值，保持 errors.Is 判断。主题 Markdown 与 MEMORY.md 保存记忆内容，状态和提交日志 JSON 保存成功批次 ID 和恢复信息；本包自行持久化，不写 Session。

## 存储格式

类型只允许 user、feedback、project、reference。主题采用 description、type、sources frontmatter；摘要单行、最多 200 个 Unicode 字符。MEMORY.md 由代码按文件名排序生成，Transaction.Put 和模型均不能直接写索引。Search 是支持中文的大小写归一字面匹配，返回路径、行号和片段，不是语义检索。

路径限于目录内普通 Markdown 文件，拒绝穿越、绝对路径、斜杠、ADS、设备名、内部文件和索引保留名。使用 os.Root、普通文件与 UTF-8 检查；不沿符号链接访问目录外内容。主题格式统一由 storage.ParseTopic 解析；读取、搜索、索引和提交均要求完整元数据。

## 外部执行整理

Execute 接收 Task 中的提示词、初始消息、六工具、ExecutionLimits 与 CallTool，负责整个模型/工具任务及结果配对，返回 ExecutionResult 和 error。本包不接触模型流、不创建 Agent，也不自行执行模型循环。后续 Runtime 使用独立 Agent 实现 Execute，当前尚未接线。

```go
// 离线验收用假 Execute：直接执行固定脚本，Turns 是模拟统计，不调用模型。
// 正式 Execute 将由 Runtime 提供，不能将这个示例作为生产执行器。
executeMemory := func(ctx context.Context, task longterm.Task) (longterm.ExecutionResult, error) {
    _, err := task.CallTool(ctx, llm.ContentBlock{
        Type: llm.TypeToolCall, ID: "write-orm", Name: "memory_write",
        Arguments: map[string]any{
            "path": "orm.md", "summary": "核心链路的 SQL 控制约束",
            "type": "project", "content": "项目不用 ORM，因为需要直接控制 SQL。",
            "source_ids": []string{"message-1"},
        },
    })
    return longterm.ExecutionResult{StopReason: llm.StopStop, Turns: 1}, err
}
s, err := longterm.NewService(directory, longterm.Options{
    Execute: executeMemory,
})
if err != nil { return err }
defer s.Close()
result, err := s.Consolidate(ctx, longterm.Batch{
    ID: "batch-1",
    Sources: []longterm.Source{{ID: "message-1", Message: &llm.UserMessage{
        Role: llm.RoleUser,
        Content: []llm.ContentBlock{{Type: llm.TypeText,
            Text: "项目不用 ORM，因为我要直接控制 SQL。"}},
    }}},
})
if err != nil { return err } // result.Execution.Usage 不代表已经保存。
fmt.Println(result.Changed, result.Execution, result.ToolCalls)
```

Execute 的约定：

- Task 包含系统提示词、初始消息和六工具目录；不修改共享输入或装配其他工具。模型与传输配置由执行方持有。CallTool 按模型原始顺序串行调用。
- Execute 同步响应 context，返回前结束所有模型、工具活动和自身资源收尾，不保留 CallTool 给后台使用，不重入同目录 Consolidate/UpdateBatch；整理中的工具读取仍使用 Task.CallTool，以保留暂存可见性和读取证据。longterm 不监督 reader、每轮调用顺序或 Agent/Driver 生命周期。
- 任一中间轮模型、工具、预算或收尾失败都返回 error，取消与预算保留 errors.Is 语义。ExecutionResult.StopReason 表达整个任务的结束原因；error 为 nil、StopStop、正轮数且不超限、非负统计才允许提交。零值、length、tool_use、未知终态均失败。
- ExecutionResult.Turns 是实际尝试发起的模型调用数，Usage 是全任务已报告用量之和；失败时也保留返回的已知统计。ConsolidationResult.Execution 保存完整执行结果，顶层不再重复 Turns/Usage。ConsolidationResult.ToolCalls 由实际回调入口计数，包括失败尝试，不从执行器重复收集。
- 任一记忆工具失败，本批禁止提交。回调保存首个领域错误，后续调用不再修改暂存；即使 Execute 忽略工具错误并返回正常结束，也不能提交。本阶段不允许工具失败后修复再提交。
- 未配置 Execute 时，新批次返回 ErrNoExecutor，没有隐式 fallback。已成功批次去重不需要执行器或模型。本包没有 Model、RequestOptions 或 Stream 配置。

例外更新流程：第一批保存“不用 ORM，以直接控制 SQL”；第二批输入“后台管理模块允许 ORM，核心链路还是不用”，模型先 memory_read 旧主题，再 memory_write 更新为“核心链路不用 ORM，以直接控制 SQL；后台管理模块允许 ORM”。最终只有一份包含范围、例外和原因的主题，索引同步更新。更新主题时，代码合并本轮实际读取主题的旧来源与本批依据，支持合并主题后保留出处；新主题仍只声明本批来源。模型不能虚构旧来源。来源集合保守保留已读主题的引用，不证明逐句真实性；假模型验证机制也不证明真实模型的语义准确率。

工具限定为 memory_list/read/search/write/edit/delete；当前模型的调用与结果配对由可信 Execute 负责，本包检查白名单、参数、调用 ID 和业务权限。历史工具调用只是证据，不会执行。现有主题必须读后修改；新建后再次 edit/delete 也须先读当前暂存版本。list/search 不替代 read。edit 只在正文作一次精确替换，未知工具、参数、来源、重复调用 ID 或执行错误会终止整批，丢弃尚未提交的暂存结果。

长期记忆通过修改、合并与删除持续维护；没有按年龄自动过期或超限静默淘汰。整理任务通过 memory_delete 删除已读主题，由 Transaction.Forget 暂存删除。删除主题不删除原始会话，也不阻止调用方以后再次提供同样来源。

## 批次、预算与取消

- Batch.ID 在目录内唯一，同一 ID 始终代表同一批来源；调用方不能将它复用于新输入。只按成功 ID 返回 Duplicate，不计算哈希，不判断同 ID 内容冲突。无内容改动也登记成功 ID，去重无需执行器。
- longterm 不保存或提供 Cursor；来源选择和处理进度由未来 Runtime 管理。
- 状态只使用版本 3，保存 `receipts: {"batch-id": true}`；提交日志只使用版本 1，内含当前状态。主题必须具有完整的 description、type、sources 和非空正文，元数据块使用本包生成的 LF 格式。无旧格式转换、缺失元数据兜底或自动迁移；不符合当前格式时返回错误。
- 默认 Turns=5、ToolCalls=24、InputBytes=256000、OutputBytes=256000、FileBytes=64000、Files=256、IndexLines=200、IndexRunes=25000；零值补默认，负值拒绝。它们是资源预算，不是 token 估算。模型的 MaxTokens 及传输选项由未来 Runtime 设置和校验。
- 预算使用具名分组：例如 options.Limits.Execution.Turns 设置模型轮数上限，options.Limits.Storage.FileBytes 设置主题文件大小上限；Task.ExecutionLimits 只携带执行预算。
- 本包限制完整初始请求的 JSON 字节数，保留单次工具入口上限、返回轮数、文件、索引、状态和日志检查。Limits.Execution 是 ExecutionLimits，Limits.Storage 是 StorageLimits；两组预算统一补默认值后，只将执行预算交给 Task.ExecutionLimits；不自动分块、重试或截掉来源。
- 执行方须在每次模型调用前检查 Turns 和本轮输入（系统提示、实际消息、工具目录的 JSON 字节数，不含鉴权），在每轮工具批次开始前检查剩余工具额度。OutputBytes 累计各轮最终 Content JSON 和工具业务结果 JSON 字节数；工具结果只计 CallTool 返回的 JSON 文本，不重复计消息外壳。生成期间实时检查 delta 增长及快照，最终累计不重复计两者。
- 当前没有生产 Execute/Runtime 实现，因此逐轮输入、模型轮数、工具批次和实时输出预算尚未接线。假 Execute 测试只验证预算传递及预算错误传播，不能证明实时执行限额已经落实。longterm 不为此恢复流包装或新增执行循环。
- messageClean 只返回历史记录文本与来源 ID 集合；requestPrepare 将索引、文件清单和这段文本组装为一条 UserMessage，正文按需读取。不传来源的 Details、Thinking、签名、redacted 数据或图片 base64；图片只标记未经视觉提炼。可见文本和工具参数仍可能包含秘密，本包不能保证识别任意凭据。模型与传输配置不进入 Task、记忆文件或本包日志。
- 输入、嵌套参数和选项共享只读；Go 不冻结对象，也不保证并发修改安全。
- 本包检查锁等待、来源处理、工具入口和提交前取消。Execute 负责执行中的取消、流关闭、在途工具等待及 Driver 收尾；本包不能强制终止不合作的 Go 回调。执行阶段失败或取消不会发布提交，失败 ConsolidationResult 的 Changed 不表示成功提交，但 Execution.Usage 可记录消耗。正常 StopStop 不要求模型输出非空正文。

## 提交与恢复

Consolidate 通过 Store.UpdateBatch 统一提交 Transaction 中的全部修改，使用有界提交日志 `.memory-pending.json`。锁内先校验全部目标主题、最终索引、批次状态和实际序列化字节数；状态固定上限 8 MiB，日志固定上限 32 MiB，自定义文件预算不会绕过它们。

1. 临时文件写入、Sync、Rename 发布 pending；发布前失败或取消不改变主题、索引、成功状态。
2. 发布日志是不可撤销的恢复点。从此不按普通取消打断提交，按排序写主题、索引、删除旧主题、写状态，最后移除日志。
3. 发布后 I/O 失败返回可判断的 ErrCommitPending，并保留日志。此时磁盘可能部分改变，不能声称已经回滚。日志移除失败也属于待恢复。
4. NewService 只创建服务，不获取目录锁或重放 pending。所有公开已提交读取及 Store.UpdateBatch 都在数据访问锁内先重放 pending；恢复成功前不读部分结果、不启动新模型或覆盖日志。重放不调用模型，删除不存在文件可幂等完成，恢复后的同批次命中去重。
5. 恢复检查版本、路径、大小、目标内容、索引一致性与状态结构。损坏日志保留并明确失败，不静默忽略。重新创建服务时配置的限制须能容纳已发布目标，否则首次实际数据操作明确失败，须调整限制后恢复。

同目录实例共享整理锁和数据访问锁。整理锁覆盖 UpdateBatch 全程，数据访问锁仅覆盖准备时的恢复/状态读取、公开只读操作以及提交。固定获取顺序为整理锁→数据锁；只读操作只取数据锁。因此模型 Execute 运行期间可以读取旧的完整已提交内容，实际提交/恢复期间读取等待，锁等待响应 context。每次 List/Read/Search 具有完整已提交视图，多次独立调用之间可以发生新提交。Close 不与操作并发。跨进程写入、外部编辑和直接读取仍由调用方协调；不提供跨进程锁或版本快照。这里只承诺已发布日志的 I/O 失败/进程中断重放，不保证突然断电或磁盘损坏后的事务安全。

本次接口调整的离线验收使用仓库外确定性假 Execute 测试、临时目录和 Go overlay。故障注入仅在 overlay 中插入本地 I/O 边界，覆盖发布前失败及主题、索引、删除、状态、清日志阶段的恢复；生产包没有文件系统替身或故障接口。未调用真实模型、未读取用户记忆、未实现 Agent/Driver 适配器。

未来 Runtime 还须装配独立 Agent 和仅本批六工具，实现实时执行预算，正确映射整个任务结果与错误、累计 Usage 并等待所有执行收尾结束，之后再做真实集成测试和主对话注入。

## 主对话只读协议与接入顺序

1. 宿主创建并绑定项目记忆 Service；跨项目记忆只在显式启用后装配，模型不指定磁盘目录。
2. 将 ReadTools() 的三个 llm.Tool 定义与 CallReadTool 回调适配到 Agent。longterm 不依赖 Agent；基础四工具集合不受影响。
3. 模型收到用户消息后，自主调用 memory_list、memory_search、memory_read；无需自动预载主题或额外检索模型。
4. 正常主任务结束且会话事实已提交后，宿主获取固定来源批次，后台调用 Consolidate；成功或 Duplicate 后再推进 Session 进度。

Read(ctx, "MEMORY.md") 返回主题链接及简短摘要，不是全部正文；主题不存在保留 os.ErrNotExist，缺少索引返回空索引。List 返回排序的主题名并验证主题格式；Search 保留原有字面匹配、行号及片段规则。ReadTools 每次返回独立目录，名称/schema 与后台相应三个工具一致。主对话入口拒绝写入类工具、未知参数和无效调用形状；单次读取失败不会污染后台批次或下一次读取。

前台三个工具读取已提交内容，后台六工具读取本批 Transaction。两者共享定义与参数解析，但不共享暂存、读取证据、调用计数或失败状态。读取不登记批次回执；它可能完成已有 pending 提交的恢复，恢复失败时不返回部分内容。

本轮补充使用仓库外临时测试验证读写隔离、Execute 期间读取、同目录锁、取消、恢复及工具权限。尚未执行真实模型整理，也没有接入 Runtime。
