# Runtime 实施与验证记录

2026-10-02 命名更新：多会话管理入口改为 Host / NewHost，文件改为 host.go；单会话 Runtime 与持久化 Session 名称不变。下文验收记录仍属于原实施日期，不代表本次重新验证。

日期：2026-10-01。依据当前 `待做计划.md` 的 Runtime 任务实施，保留现有 Agent 循环和 Session 目录边界。未修改 demo、工具算法、LLM、Provider 或实际配置；未提交或推送 Git。

## 实现范围和文件职责

| 文件 | 职责 |
| --- | --- |
| `internal/runtime/host.go` | 共享数据库初始化、多会话登记、并发打开复用、列表与管理入口、整体关闭 |
| `internal/runtime/runtime.go` | 单会话操作、准入与故障状态、读取、绑定目录、模型选择和关闭等待 |
| `internal/runtime/persistence.go` | 同步实现 Agent Recorder；调用 Session 保存执行事实，转换模型来源及终态 |
| `internal/runtime/ownership.go` | 数据库路径规范化、进程内独占登记、会话锁文件生命周期 |
| `internal/runtime/ownership_windows.go` | Windows `LockFileEx` / `UnlockFileEx` 独占锁 |
| `internal/runtime/ownership_unix.go` | Linux/macOS `flock` 独占锁 |
| `internal/runtime/tools.go` | 复用七个基础工具定义，执行时取得首次绑定的工作目录 |
| `internal/runtime/config/types.go` | 装配参数、创建/打开参数、实例状态、快照及少量可判断错误 |

Runtime 不维护第二套 Run 队列或模型循环，不直接执行 Session SQL。Store 仍为五个正式文件，数据库仍保存多个会话。新增锁使用现有 `golang.org/x/sys`，没有新增锁库或存储后端。

## 应用层调用接口

Host 提供 `NewHost`、`CreateSession`、`ListSessions`、`OpenSession`、`RenameSession`、`ArchiveSession`、`UnarchiveSession`、`CloseSession`、`Close`。

Runtime 提供 `SessionID`、`Submit`、`Steer`、`SetModel`、`BindWorkingDir`、`GetCurrentHistory`、`GetRun`、`ListRuns`、`Snapshot`、`Close`。`Submit` 返回原有 `*agent.RunHandle`，继续使用其 `ID`、`Cancel`、`Wait`。

装配示例（模型引用需替换为 TOML 中真实存在的 `provider/alias`）：

```go
models, err := modelconfig.Load("config.toml")
if err != nil { return err }
host, err := runtime.NewHost(ctx, runtimeconfig.Config{
    DatabasePath: "sessions.db",
    Models: models,
    OnEvent: func(sessionID string, event agent.Event) {
        // 快速交给展示层，不在这里等待 Run 或 Close。
    },
})
if err != nil { return err }

info, err := host.CreateSession(ctx, runtimeconfig.CreateSessionOptions{
    Model: sessionconfig.ModelSelection{Ref: "provider/alias"},
    // WorkingDir 可省略；未绑定时可以聊天，但文件及命令工具明确返回业务错误。
})
if err != nil { return err }
conversation, err := host.OpenSession(ctx, info.ID, runtimeconfig.OpenSessionOptions{})
if err != nil { return err }
handle, err := conversation.Submit(taskContext, userMessage)
if err != nil { return err }
result, waitErr := handle.Wait(waitContext)
// waitErr 表示等待失败；任务结果另看 result.State / ErrorMessage。
// 程序退出时 host.Close(shutdownContext)；超时不代表后台收尾已经完成。
```

没有默认模型。模型选择仅保存 `ModelSelection.Ref`；本轮没有增加请求参数覆盖接口，参数、凭据和协议选项沿用当前 TOML 装配结果。API Key 和 Headers 不进入模型选择、历史来源或 Runtime 快照。来源地址拒绝用户信息、查询参数和片段，避免将地址中的凭据持久化。

## 必要的 Agent、Session 接入

Agent 仍只依赖 LLM 和基础类型，不依赖 Runtime、Session 或 SQL。

- 新增可选 `Recorder`，包含 StartRun、RecordStep、AppendMessage、RecordToolStart、AppendToolResult、FinishRun。未注入时仍可纯内存运行。
- Config 可注入 UUID 生成函数、恢复历史的 `InitialOrigins`、保存超时。`ModelOrigin` 导出，恢复索引指向实际 assistant 对象。
- 出队、请求准备、首次绑定及模型更新使用小型执行边界协调；数据库操作不持有 Agent 状态锁。
- `SetModelWithPersistence` 在请求准备边界内保存配置并更新 Agent，边界释放后才分发事件；旧请求使用已固定的 Model/Options。
- Run 收尾预约终态提交，拒绝这期间的 Steer，保留 currentRun 直到保存完成。`finishing` 仅表示这个提交边界，不参与模型切换。
- 记录失败保留首次原因，取消执行、停止新调用和新工具、完成被丢弃的排队句柄。取消用于收尾，不能把存储错误覆盖成普通 aborted。
- 工具启动前保存启动记录；每个结果完成时单独保存。内存历史仍按模型调用顺序组织，数据库按实际提交顺序记录。
- 非等待的 `RequestClose` 用于 Runtime 先停止准入；原有 `Close()` 继续等待同一个工作循环退出。

Session 仅扩展模型选择：SessionInfo 增加可选 ModelSelection，Service 增加 `CreateSessionWithModel`、`SetModelSelection`；原有 `CreateSession` 保留。Store/SQL 增加对应列及读写，未重新组织包，也未搬入 Agent 编排。

工具包没有业务改动。Runtime 初始化时注册完整工具集合，执行适配器在未绑定时拒绝执行，绑定后复用既有构造函数和 Execute。没有 `os.Chdir`、运行时逐次目录存在性检查或新 sandbox。

这些记录点必须接在执行主体中；观察事件不能阻挡工具启动，也不能保证结果先落盘，因此不能只靠 Runtime 的 OnEvent 回调实现持久化。

## 执行、保存和关闭顺序

```text
OpenSession：取得会话所有权 → 检查会话/目录 → 中断本会话遗留 Run
             → 读取当前分支和来源 → 解析显式模型 → 装配工具/Agent → 返回可用实例

Submit：Agent 入队 → 返回 Handle（此时不创建数据库 Run）
真正出队：StartRun 提交用户输入 → 消费并保存纠正 → 准备请求
          → RecordStep → 调用模型/转发 LLM 事件
          → 保存最终 assistant 和实际 Origin
          → 工具启动记录 → Execute → 每个工具结果及时保存
          → 确定终态 → FinishRun 提交 → Handle 就绪
          → StepEnd → RunEnd → 外层循环处理下一任务

Close：停止准入 → 取消当前及排队任务 → 等待 Agent/记录收尾
       → 等待仍在进行的会话管理/读取操作 → 释放所有权 → 实例 Closed
Host.Close：停止新增操作 → 请求所有实例关闭并等待 → 关闭共享数据库
```

模型消息的 LLM 完成事件可实时展示，它不表示整个 Run 已经持久化完成；最终完成以 Run 结果及 RunEnd 为准。普通取消后实例仍可工作，Close 不删除、不自动归档会话。关闭等待 context 超时，只结束本次等待，关闭过程、实例登记及所有权仍保留。

故障收尾使用独立、带超时的 context，默认 5 秒。没有无限重试；工具副作用不回滚，结果保存失败不重新执行工具。故障实例拒绝新输入，关闭后重新打开，由数据库恢复已提交事实；未结束记录被标为程序中断，有启动无结果仍是结果未知。

归档活动 Run 会明确失败。归档已打开的空闲实例，先保存归档，再请求关闭并等待；未打开会话的管理操作只短暂持锁。列表只查询摘要，不打开会话或取得各会话锁。

## 数据库版本与兼容性

当前 `PRAGMA user_version = 3`。新增 `sessions.model_selection_json`，四表数量及原有关联结构不变。

升级前需关闭其他实例；本轮没有实现跨版本并存运行或在线迁移协调。

- 新数据库直接建立 v3。
- 已知无版本、v1、v2 数据库在原有结构检查/回填基础上升级，新增列保持 NULL，已有会话数据保留。
- 旧会话缺少模型选择时，OpenSession 返回 `ErrModelRequired`，可以通过 OpenSessionOptions.Model 显式补充。
- 未识别结构或更高版本拒绝打开；失败升级保留原版本，不删除重建数据库。
- 初始化不自动恢复 Run；持有会话所有权的 Runtime 才恢复对应会话。

## 对照计划的实际验收

测试使用临时数据库、假 StreamFunc、假工具；跨进程测试启动真正的测试子进程。当前实际运行平台：Windows amd64。

| 计划验收项 | 运行证据 |
| --- | --- |
| 1. 同会话并发打开复用；不同会话并行 | ConcurrentOpenAndManagement；DifferentSessionsExecuteConcurrently |
| 2–3. 跨进程独占、不同会话打开、空闲持锁、正常退出及被终止后接管 | OwnershipAcrossProcesses（真实子进程，含强制终止） |
| 4. 列表无会话锁；管理短锁；其他拥有者拒绝修改 | ConcurrentOpenAndManagement；OwnershipAcrossProcesses |
| 5. 初始化失败释放登记和锁 | OwnershipPathAliasesAndInitializationFailure，缺失模型/会话和非法工具后重新接管 |
| 6. 只恢复已取得所有权的当前会话，不重放工具 | RecoveryOnlyOwnedSession |
| 7. 原始历史、Origin 和同源签名恢复 | ModelSelectionAndOriginRestore |
| 8. 排队不落库，排队取消不创建 Run | QueuePersistsOnlyStartedRuns；CancelAndCloseTimeoutRetainOwnership |
| 9. StartRun 保存失败不调用模型 | PersistenceFailuresStopExecution/start |
| 10. assistant/工具启动记录失败不执行工具 | PersistenceFailuresStopExecution/assistant、tool_start |
| 11. 快工具结果先保存，不等慢工具 | ParallelResultsAndRepeatedCallIDs |
| 12. 相同 ToolCallID 不跨 assistant 串配 | ParallelResultsAndRepeatedCallIDs |
| 13. 未知和被纠正跳过的工具有结果、没有伪启动 | ParallelResultsAndRepeatedCallIDs；SteerSkipsUnstartedToolsAndPersistsInput |
| 14. 提交后 Handle 才完成；失败结束句柄且不启动下一任务 | CompletionSavedBeforeHandleAndEvents；CompletionEventsObserveSavedResult；FinishFailureDiscardsQueueWithoutStartingIt |
| 15. 取消仍保存 aborted 和工具结果；故障不被取消覆盖 | CancelAndCloseTimeoutRetainOwnership；UserCancelPreservesQueueAndConsumedSteer；六类 PersistenceFailuresStopExecution |
| 16. Close 超时仍持锁、重复关闭一致 | CancelAndCloseTimeoutRetainOwnership；ManagerCloseAttemptsAllInstances |
| 17. 模型选择恢复、无默认、实际来源不受切换污染、无凭据 | ModelSelectionAndOriginRestore；OwnershipPathAliasesAndInitializationFailure；ModelSelectionMigration |
| 18. 初始化工具齐全、未绑定不使用 cwd、绑定后同入口执行 | ToolsStableBeforeAndAfterBinding（实际 write 临时文件并核对） |
| 19. 模型/目录更新与执行边界协调；记录不持 Agent 状态锁 | ConcurrentModelAndDirectoryUpdates；QueuePersistsOnlyStartedRuns；CompletionSavedBeforeHandleAndEvents 中记录回调同步读取 Snapshot |
| 20. 保留原 Agent、Session 和工具用法 | RecorderOptionalAndModelCallbackReentry；旧 CreateSession 与迁移测试；实际基础 write 调用；全项目检查 |

另有模型事件回调重入、归档关闭、保存超时、步骤保存失败、模型选择保存失败保持原配置、v0/v1/v2 升级、失败升级回滚和未知版本拒绝测试。所有永久测试文件保留在相应包中。

`TerminalResponsesAndToolPanic` 还实际检查普通失败、安全拦截、长度截断、工具 panic 的持久化状态、工具执行条件与 StepEnd/RunEnd 唯一顺序。

已运行并通过：

```text
gofmt -w internal/runtime internal/agent internal/session
go test ./... -timeout 90s
go build ./...
go vet ./...
go test ./internal/runtime ./internal/agent ./internal/session/store -count=10 -timeout 60s
go test ./internal/runtime ./internal/agent ./internal/session/store -count=5 -timeout 90s
GOOS=linux  go build ./...
GOOS=darwin go build ./...
```

重复 10 次的检查针对当时已有测试；补齐收尾测试后又重复 5 次，并执行最终全项目检查。不能将前一次测试数量说成覆盖后来新增的测试。

未运行或不在本轮验收范围：

- `go test -race`：已尝试，环境返回 `-race requires cgo`；当前 CGO_ENABLED=0，未找到 gcc/clang，未安装编译器。
- Linux/macOS 锁和关闭的运行行为：仅交叉编译通过，没有在对应系统运行。
- 真实模型服务和真实签名服务端兼容性：本轮未调用 API；签名恢复验证的是本地存储和请求准备链路。
- 主动脱离进程管理、分布式锁、跨版本同时运行、Memory、SubAgent、UI、CLI 命令：明确不实现。

本记录只证明列出的受控场景及代码边界，不宣称覆盖所有磁盘故障、平台或真实服务行为。
