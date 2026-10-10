# Runtime 接线

当前为第一阶段可构建实现，尚未完成待做计划的全部验收。不会恢复旧 Runtime API。宿主须提供已初始化的当前版本 Session Store、真实项目目录、稳定所有权目录、明确模型引用和模型配置；不读取 config.toml、不迁移数据库、不改变进程工作目录。

## 入口与资源

- `NewHost(ctx, Options)` 装配依赖；`Host.Create`、`Open`、`Opened`、`Close` 管理会话与资源。
- `Runtime.Submit` 返回 Agent RunHandle；`Steer` 交给 Agent；`CancelRun` 取消句柄。
- `Snapshot`、`Subscribe` 用于观察。每个观察者有独立有界缓冲及 panic 隔离，慢回调不阻塞 Driver 或资源关闭；溢出通过 Dropped 提示重读 Snapshot。已经进入的用户回调不能由 Go 强制终止。
- `Approve(id, permission.Choice)` 接受真实用户选择，`Revoke(tool)` 撤销具体工具授权。审批 ID 只作展示，Confirmation 保留在所属会话内存。
- `SetModel`、`SetPolicy`、`SetHooks` 通过 Agent 原子空闲检查更新；项目目录固定，新项目创建另一 Host。
- `RefreshMCP(ctx, serverID, reconnect)` 更新后重建静态工具快照；空 serverID 只应用 Manager 已有目录。
- `Spawn`、`Send`、`Wait`、`Cancel` 实现现有 subagent.Controller；`Cancel` 是子任务接口，主 Run 使用 `CancelRun`。

`Options.Tools=nil` 显式装配 read、平台命令、edit、write、grep、find、ls；非 nil 空切片关闭基础工具。可信身份、能力和目标提取函数由宿主提供，不从模型输入或 MCP readonly hint 取得。嵌套输入及模型配置按只读借用，使用中不得由宿主修改。提交的同一消息对象消费前不能重复提交。

Store 与 MCP Manager 的关闭责任分别由 CloseStore/CloseMCP 明确声明。关闭先拒绝新提交，取消 Agent、子任务和记忆任务，继续保存 Driver 收尾事实，再执行 SessionEnd、清空授权和释放拥有资源。恢复先取得 OS 会话锁，再把没有旧执行者的未结束 Run 记为 aborted；不重放旧模型、工具或 Hook。

## 文件边界

| 文件 | 职责 |
| --- | --- |
| host.go / runtime.go | 宿主装配、准入、展示和关闭 |
| driver.go | 唯一 Next 读取者；工具前置等待不阻塞其他工具事实交接 |
| persistence.go | Session API 及当前 Run 的 ResponseID→EntryID 关联 |
| approval.go | 最终参数、可信目标、权限与真实审批 |
| tools.go / mcp.go | 显式可信绑定、静态工具目录及空闲更新 |
| context.go | 指令/技能、ProjectHistory 身份关联、预算和摘要应用 |
| memory.go | 正常主 Run 保存后的后台整理、六工具 Agent 与执行预算 |
| subagent.go | 父作用域、普通子 Session/Agent、具体子 Run 交付 |
| workspace.go / ownership | 真实路径和持有到 Run 收尾的 OS 文件占用 |

原始工具结果先保存为 pending，完成 Hook 后独立确认模型可见 Content，再用同一替换 Resolve。nil 保持原文，非 nil 空切片清空；恢复省略 pending 原文，由 Agent 补缺失结果说明。PreToolUse 改写后仅一次 schema 校验，审批期间固定参数；Hook Allow 不覆盖普通权限，Hook Ask 独立要求当前确认。

摘要来源调用 Agent.ProjectHistory；生成说明只有临时 compact ID。保存时按真实祖先路径重排 SourceIDs、换算覆盖数，保存成功后应用；每次请求重新应用摘要尾部，原生请求 Hook 改写不入库。窗口未知只诊断。压缩失败后原请求可容纳则继续，已知超限停止。

后台记忆使用触发时主模型和请求选项，不装对话 Hooks、基础工具、MCP 或子代理。只在 Consolidate 成功/回执去重后 AdvanceMemoryProgress；失败停止，等待下次正常主 Run 触发。没有定时器或持久重试队列。

## 当前验证及未完成事项

验收副本位于 `C:/Users/27902/Documents/Codex/2026-10-05/task-6/runtime-work/acceptance`，不往项目写永久测试。第一批 10 个顶层用例通过；覆盖主链、审批选择、并行等待取消、队列取消、原生模型 Hook 错误、来源换算、锁取消、记忆预算和子任务交付。Windows/amd64 与 Linux/amd64 目标构建通过；Linux 只交叉编译。

以下尚未满足完整交付，不能据编译通过标记 Runtime 完成：

- MCP 刷新已冻结同一 Host 全部会话的准入并检查空闲；`MCPShared` 借用外部已启动 Manager，只允许应用已有目录，刷新/重连由外部拥有者协调。刷新不清空会话授权，连接代次变化仍按新可信身份重新评估。
- 文件占用允许取得空闲的逆序目标。同一 Host 内仅保留占用请求的短期等待边，并检测 worker 间及父子生命周期等待环；形成环时拒绝当前占用请求，不释放已有生命周期占用或中止其他 Run。普通占用等待响应取消。跨 Host/进程的 OS 锁不提供持有者身份，不能据此保证跨进程环检测。
- worker 默认不装 shell；Unknown 只有在可信宿主提供完整非空文件目标提取时才可安装，并同样执行范围校验、审批及占用。可信外部/MCP 适配绑定可经 Options.Tools 注入，不因来源额外禁用。自动发现 MCP 没有可信写目标，仍不进入 worker；不能把 readonly hint 当成可信能力。主会话 shell/无法枚举目标的外部工具不提供具体文件占用保证。
- 命令 Hook 参数改写、空输出替换、defer 和 visibleOutput 已通过定向验证；后续记忆回执、MCP 部分失败/stale、持久化故障的正式流程验收见下节。
- 尚未更新 CLI/UI 调用者，未构建全项目，未调用真实模型、外网、实际配置、用户数据库或真实业务工具。

## 2026-10-10 复杂审查阶段

本轮仅修改 approval.go、mcp.go、memory.go 与本文，保留前轮未提交实现。审批 Evaluate、登记等待和再次评估与 Grant/Revoke 共用 Runtime 锁，消除并发 SessionTool 授权丢失唤醒的窗口。后台 Driver 将包括 unavailable 在内的任何不成功工具结果视为整理失败；输出预算同时覆盖尚未解析进 Snapshot 的流式 Delta，同轮取增量累计与快照大小的最大值，最终响应再次补齐计数，不重复累加相同输出。

以下为该阶段发现，后续处理结果见“续接修复与正式流程验收”：

- 逆序文件拒绝、父子文件等待限制、worker 排除 Unknown/MCP 是既有额外限制，计划未逐项授权；本轮没有扩张或重写它们。
- `Host.Open(childSessionID)` 恢复时默认 role 为空，可能把已持久化的 explore/worker 按主会话装配。Session 公共入口只提供按父会话查询子关系；缺少按子会话查询父关系、角色和文件范围的入口。底层 Store 仅有 IsChildSession 布尔查询，不能恢复完整角色。需补齐 Session 公开接线后处理，不在 Runtime 写 SQL 或扫描全部会话猜测归属。
- `Send` 在已结束子 Run 上继续时覆盖 childExecution.handle/result；若旧 Run 尚未进入 collectChildren，收齐时只能看到最新 Run。需要覆盖“同一父 Run 多次运行同一子任务”的具体交付用例，并落实未交付句柄保留。
- 后台固定批次提交成功但 Advance 失败的回执恢复、MCP 部分失败/stale 不重放、持久化失败关闭准入仍需最小集成验收。不得以本轮局部检查替代这些条件。

本轮材料：`C:/Users/27902/GolandProjects/Agent/runtime-review-20261010/`。validation 复用先前 acceptance 的非 Runtime Go 源码，经直接文本比较与当前源码一致；before 复用旧副本并补齐本轮开始时已有的 UpdatedInput 和 MCPShared 改动，未用于覆盖工作树。首次临时目录随工具沙箱销毁，交付材料已放在上述稳定目录。SHA-256 批次为 0。

实际检查记录（均在 validation，GOPROXY/GOSUMDB=off、GOTOOLCHAIN=local、CGO_ENABLED=0）：

- `go test -mod=readonly -count=1 -timeout=30s -run '^TestReview(MemoryUnknownTool|MemoryStreamingDelta|MCPKeepsSessionGrant)$' -v ./internal/runtime`：首次因旧临时夹具 commandHooks 缺少右花括号而 setup failed；仅修复仓库外夹具后重跑同命令，三项均复现预期缺陷。分别记录于 defect-reproduction.log、defect-reproduction-after-fixture-fix.log。
- `go test -mod=readonly -count=1 -timeout=45s -run '^(TestReview.*|TestApprovals|TestCancelledApprovalAndQueuedInput|TestHookRewriteAskAndEmptyReplacement|TestHookDeferNoReplay|TestVisibleOutputForms|TestMemoryBudget)$' -v ./internal/runtime`：12 个顶层用例通过，记录于 targeted-verification.log。覆盖本轮修复及明确未决 Hook/观察者风险，未重跑完整套件。流关闭只验证确实关闭，允许 Agent 的幂等重复 Close。
- `go build -mod=readonly ./internal/runtime/...`：GOOS=windows/linux、GOARCH=amd64 各一次通过，日志为 windows-build.log、linux-build.log。原因是本轮修改了两平台共有源码；Linux 只证明交叉编译，不代表运行验收。

前轮最小验证已通过：压缩失败仍可容纳、超限停止、未知窗口、已保存摘要每轮应用、请求 Hook 临时改写不入库、pending 恢复不泄漏原文；另在前轮准入和锁修复后，只重跑 7 个受影响主链/取消/子任务/目录/锁用例，两平台重编译通过。日志在 runtime-work 的 context-risk-tests.log、boundary-risk-tests.log 与对应 build.log。没有第二次完整套件运行。

## 续接修复与正式流程验收

本阶段修改 subagent.go、workspace.go、host.go、tools.go、approval.go 与本文。没有新增对外入口，也没有修改 Session 或其他独立包。备份位于 runtime-review-20261010/medium-before，测试继续复用 validation；断线前 subagent.go 已落盘，medium_test.go 未落盘，重连后才重新写入临时夹具。未重复此前已完成的完整验收、未计算哈希。

连续 Send 复用同一子 Runtime，但开启新的 child Run。每个已启动 Run 仅保留父/子 Run ID 的短期交付关联；collectChildren 从 Session.GetRun 读取正文及终态，通过 DeliverChildResult 交付，成功后移除关联，父 Run 收尾清理残留。不新增持久队列，不保存第二份待交付历史。RunEnding 的 Repeat 收齐所有相关 Run，再次进入结束候选时不重复交付。

对“子会话重开”的纠正：目前新 Host.Open 没有 CLI/UI 生产调用者，旧 CLI 仍引用已移除的旧 Runtime API。正常子任务由父 Controller 创建并持有，Send 不重新打开；进程退出后的旧 Run 中断，也不会自动重播。读取历史应使用 Session 查询，Host.Open 则是宿主选择主会话后装配可 Submit 新 Run 的执行入口。前阶段把任意 child ID 调用通用 Open 当作当前产品恢复流程阻塞，表述过重。保留的只是未来宿主接入风险：当前入口依赖宿主只传入主会话 ID，不能自行验证任意历史 ID 的角色。没有新增子会话恢复功能、禁止所有主会话恢复或据此要求扩展 Session；若未来正式支持子任务跨进程续跑，再明确受支持路径及查询需求。

文件占用不再使用字典序拒绝。环检测只读取同一 Host 的实时占用、尚未结束的等待请求，以及“父 Run 必须收齐子 Run”的生命周期依赖；若当前新增请求闭合等待环，返回普通占用错误，由现有 Driver/Agent 反馈。它不是自动调度器：不选择并中止已有 Run，不转移或自动释放已持有文件。请求退出会移除等待边，Run 收尾释放占用和对应父子依赖。

观察者每个订阅只有一个 goroutine，缓冲为 64 条，不为每个事件启动 goroutine。溢出会丢旧通知，在后续 Event.Dropped 中累计报告；Snapshot 仅能恢复当前状态，不能重放全部流式增量或诊断。panic 隔离，关闭不等待用户回调。永久阻塞回调仍占用其自身 goroutine，反复注册不返回的回调可能累积；没有声称可强制终止任意用户代码。

### 本阶段检查记录

所有外部模型/工具均为假实现，数据库及记忆目录均为临时目录；MCP 使用内存 HTTP RoundTripper，不打开网络连接。保持 GOPROXY/GOSUMDB=off、GOTOOLCHAIN=local、CGO_ENABLED=0。

| 命令（validation 中） | 结果和追加原因 |
| --- | --- |
| `go test -mod=readonly -count=1 -timeout=60s -run '^(TestMedium.*\|TestChildLifecycleAndDelivery\|TestLockCancellation)$' -v ./internal/runtime` | medium-targeted.log：8 项中 5 项通过。记忆发布受到 Windows 沙箱 Rename 错误阻塞；MCP、可信外部 worker 两项因夹具遗漏外部工具真实审批而超时。前者未到达预定进度故障，后两项不是产品缺陷。直接占用调用仅是内部并发单元检查，Controller 直调用不冒充正式模型工具端到端验收。 |
| `go test -mod=readonly -count=1 -timeout=45s -run '^TestMedium(ChildConsecutiveSend\|MemoryReceiptAfterProgressFailure\|MCPPartialFailureAndStaleNoReplay\|TrustedUnknownWorker)$' -v ./internal/runtime` | medium-formal-recheck.log：3 项通过，记忆仍被沙箱 Rename 阻塞。按最新要求改为正式 spawn_agent/send_input/wait 工具链；外部工具经 Approve；stale 通过拥有 Manager 的 Host.RefreshMCP 刷新、借用 Host 保留旧绑定触发。只重跑失败或流程有调整的用例。 |
| `go test -mod=readonly -count=1 -timeout=30s -run '^TestMediumMemoryReceiptAfterProgressFailure$' -v ./internal/runtime` | medium-memory-unsandboxed.log：非沙箱执行通过，同样只有临时依赖。证明两次正式 Submit 间，无文件内容变更批次的成功回执在进度写失败后阻止重复模型执行，并补齐进度。未伪造回执或修改存储实现。 |
| `go test -mod=readonly -count=1 -timeout=30s -run '^TestMediumFormalFileClaims$' -v ./internal/runtime` | medium-formal-fileclaims.log：3 个正式流程子场景通过；Submit→spawn_agent→worker 文件工具验证空闲逆序目标、worker AB/BA 环、父子文件环。假文件工具仅替代文件副作用，不绕过审批、占用或 Driver。 |
| `go build -mod=readonly ./internal/runtime/...`，GOOS=windows/linux、GOARCH=amd64 | 两平台各一次通过，medium-windows-build.log、medium-linux-build.log。新增子任务/占用公共源码改变，因此需要重新目标编译；Linux 仍仅交叉编译。 |

### 对照计划第 21.4 节

| 项目 | 实证与范围 |
| --- | --- |
| 主链 | 前轮假模型/工具→持久事实→终态通过，本轮正式子工具链经过同一 Driver。 |
| 输入 | 前轮已消费输入、排队取消检查通过，未重复运行。 |
| 最终参数 | 前轮 Hook 改参后执行/保存一致通过，未重复运行。 |
| 审批 | 前轮 Once/SessionTool/Reject、Hook Ask、授权唤醒通过；本轮外部工具通过 Approve 放行。 |
| 取消 | 前轮审批/队列取消通过；本轮正式父 Run 取消使活动 child Run 中止并完成清理。 |
| 工具结果 | 前轮原文、空替换、pending 恢复不回退通过。 |
| defer | 前轮不执行、不重放通过。 |
| 压缩 | 前轮来源换算、保存摘要重复应用、临时 Hook 不入历史通过；没有新增历史分支产品入口或声称覆盖所有分支恢复组合。 |
| 压缩失败 | 前轮可容纳继续、超限停止、未知窗口通过。 |
| 子任务 | 正式连续 Send 产生三个具体 child Run，三份结果全部交付；Repeat 后未重复交付，父取消通过。 |
| 记忆 | 正式 Submit 后触发固定批次；临时数据库拒绝进度写入，下次 Submit 通过成功回执跳过已提交批次并推进。覆盖无内容变更批次回执，不声称完成所有真实文件发布故障组合。 |
| 后台关闭 | 前轮流式预算取消并关闭流通过；尚未覆盖真实文件发布过程中的每个关闭时点。 |
| MCP | 正式 Host 启动部分失败保留基础及可用工具；工具失败调用一次；借用 Host 的 stale 绑定产生失败事实且不重放。 |
| 持久失败 | 正式 Submit→工具启动保存遇到临时数据库写失败，工具执行计数为零、Runtime.Fault 设置、后续 Submit 被拒绝。 |

真实剩余限制：CLI/UI 未接入新 API；Linux 未运行，仅目标编译；没有真实 Provider、外网或用户数据库验收；跨 Host/进程等待环没有持有者身份可检测；不支持子任务跨进程自动续跑。上述限制不通过扩包、虚构恢复入口或重跑全仓测试掩盖。
