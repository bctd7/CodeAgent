# 独立终端展示层

`internal/tui` 只依赖官方 Go 模块，不引用 CLI、Runtime、Session、Agent、数据库或 Host。参考 Amadeus README/design 的交互组织，独立实现，没有复制其实现代码。尚未接入实际模型、审批或子代理运行。

入口为 `New(ctx, Config) *Model` 和 `Run(ctx, Config, IO) error`。Config 注入初始 Snapshot、命令候选、Handler 和可选只读 Events 通道。`Program.Send(Event)` 也可注入事件。所有展示状态只在 Bubble Tea Update 中更新；调用方应在自行管理 Program 时取消对应 context。Run 自动管理 UI context；不会替运行层收尾。

消息使用稳定唯一 Item.ID。ItemEvent 替换/追加项目，DeltaEvent 追加助手文本，StatusEvent 更新运行状态，SnapshotEvent 替换整个会话投影；工具名称/状态/短结果默认显示，F4 选择展开参数、输出及已有 Diff 文本。Diff 不由 UI 计算。Scope.SessionID/RunID 用于过滤迟到事件，实际装配应填充会话 ID；会话级状态事件可省略 RunID，运行级内容应填充当前 RunID。空 Scope 仅供显式全局投影或假数据使用。投影发送后不可再修改。

Handler 异步接收 Action，返回已脱敏 Event 或错误。成功仅表示操作受理，不代表运行结束。UI 对正在提交的同类操作抑制重复，取消同一 Run 的请求在终态前只发一次，退出等待后端 ExitEvent。普通消息只在非运行状态发送；Steer 有独立按键及动作，仅在运行时提交。审批支持队列，回传 `once`、`session-tool`、`reject`；session-tool 明确表示同会话授权该工具，包含变化参数。UI 不保存授权规则；Esc 回传 reject。长参数/Diff 可翻页。

输入支持剪贴板及终端 bracketed paste，多行 Enter，Ctrl+S 发送，Alt+S Steer。Up/Down 选择斜杠候选，Tab 补全；原命令文本直接提交 ExecuteCommand，不解析业务。PgUp/PgDn 和鼠标滚动查看历史，主动上滚后增量不强制回底部，Ctrl+End 恢复跟随。F2/F3 按需加载会话/模型选择，Enter 确认、Esc 取消；异步加载取消后不会因返回结果重新弹出。Ctrl+C 在运行时提交取消，否则提交退出；Ctrl+Q 总是提交退出。没有默认模型。

上层须先脱敏所有展示文本、错误、参数、输出和 Diff，禁止注入密钥、请求头、原始配置或执行对象。UI 额外过滤文本中的终端控制序列，这不替代脱敏。

审批队列保存收到请求时的 Scope 与 Approval；空 SessionID/RunID 在收到时绑定当时状态。选择按原始 Scope 回传，不重绑当前 Run；归属失效时移除并提示过期，不提交选择。异步审批结果按原始 Scope+ID 清理，迟到结果不覆盖新 Run 的提示或审批。审批归属使用精确比较，收到时的空 RunID 不作为未来 Run 的通配符。未知 ItemKind 的默认标签也先过滤终端控制序列。

未来 CLI 装配适配器应放在启动/装配层，实现 `cli.TUI.Run`，持有 CLI ChatState，调用 `CLI.Command(ctx, state, input)` 并投影结果为本包 Event，保持依赖方向而不形成导入循环。命令候选也由装配层提供。当前本包 Run 签名有意不直接实现 cli.TUI；Create/Rename/Archive 等业务不在这里重复。

CLI 的 ChatState.CurrentHandle 仅代表当前活动 Run。装配层须保存所有 Submit 返回的 handle，并按 Snapshot.Agent.CurrentRunID 选择当前 handle，不能用新排队 handle 覆盖；取消与退出收尾仍由持有句柄的后端完成。审批请求/选择的投影也由未来装配负责，本包不假装存在实际审批服务。

纯假数据演示：`go run ./internal/tui/demo`。`/stream` 和 `/approval` 仅演示事件投影；不会调用真实模型、执行工具或改变权限。假事件生产 goroutine 由 context 管理并等待退出。

依赖来自 https://proxy.golang.org 的官方模块：Bubble Tea v1.3.10、Bubbles v0.21.0、Lip Gloss v1.1.0、Glamour v0.9.0、x/ansi v0.10.1。此组合为相互兼容的稳定版本；较新 Glamour v0.10/v1.0 的模块契约要求 Lip Gloss 预发布版本。Markdown/代码高亮使用 Glamour，中文与宽字符硬换行使用 x/ansi，输入和滚动使用 Bubbles。

`THIRD_PARTY_LICENSES.txt` 保留此包实际使用的 32 个模块的许可原文与版权声明。go-localereader v0.0.1 官方归档只有 README 中 MIT 声明和作者归属，因此原样保留该 README；其余模块保留 LICENSE/COPYING/NOTICE 文件。分发二进制时应随附这些声明，不删除模块原有声明。

验证记录（Windows，2026-10-02）：临时包内假事件测试覆盖会话/Run 迟到事件过滤、宽字符窗口 1×1/4×6/18×12/80×24/120×40、增量滚动与恢复跟随、复制展示数据、粘贴/多行、发送/Steer/命令路由、重复取消/退出、异步选择取消、工具展开、三种审批与失败保留、事件通道关闭、终端控制序列过滤和后台操作期间窗口调整，全部通过。测试文件在完成后清理，假数据 demo 保留。`go build ./internal/tui/...`、`go vet ./internal/tui/...` 和 `go test ./... -run '^$'` 通过。正常沙箱无法写默认模块缓存及系统 Go 临时构建目录，这些 Go 操作通过本地批准流程执行，未改 ACL 或绕过拒绝。

Windows PTY 自动交互启动成功，实际输入 `/stream` 显示分段中文、宽字符及代码高亮；`/approval` 显示来源/工具/参数与三种选择，输入 2+Enter 回传 session-tool 并明确显示未改变权限或执行工具；F2 假会话弹层及 Esc 返回、Ctrl+Q 退出正常。没有图形截图或人工视觉验收，也没有真实模型、审批、子代理运行验收。

审批 Scope 与未知标签过滤修复后的 5 组临时回归均通过：RunA 请求在 RunB 确认前过期、同/跨 Session 的迟到成功与失败结果隔离、重复确认与当前请求正常受理/拒绝/失败保留、返回事件期间归属变化后停止应用旧结果、未知 kind 中 ANSI/OSC/control 过滤。包 build/vet 通过；本轮临时测试完成后清理。未改变 pending 跨 Session 的现有行为。
