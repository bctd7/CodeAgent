# CLI

`Execute(ctx, args, IO, Options) int` 是正式包入口，负责装配及关闭它创建的资源。
`Main(args) int` 提供进程信号、标准 IO 和默认宿主设置，返回退出码，不调用 os.Exit。
根 main.go 现在薄转发至 cli.Main，待它返回并完成 defer 后再 os.Exit，保留 CLI 退出码。
原 demo 的 10 个文件已经删除，可从 Git 检查点恢复；没有通过 init 接管程序。

Run from the project directory. Configure a model before submitting a prompt:

```powershell
go build -o codeagent.exe .
.\codeagent.exe help
.\codeagent.exe run "任务" --json
.\codeagent.exe chat
```

Config selection: explicit `--config` > `Options.ConfigPath` > executable-directory `config.toml`; cwd TOML is ignored and only one file is loaded.
REF 对应 `[providers.PROVIDER.models.ALIAS]`，不是模型的远端 ID。
新会话按显式 `--model`、CLI settings 的 `default_model`、已配置 REF 排序后的第一个选择。
CLI 不添加、打印或重写凭证。短期 eval 应用 `--data-dir` 隔离数据库；
编译后的程序直接返回退出码，go run 的包装退出码不适合 eval。

## 命令协议

```text
run "任务" [--model provider/alias] [--directory DIR] [--session ID]
run --stdin [--model provider/alias] [--timeout 30s] [--format text|json]
chat [--model provider/alias] [--session ID] [--stdin] [--json]
sessions list [--limit 50] [--offset 0] [--archive-filter active|archived|all] [--json]
sessions show ID [--json]
help
version
```

通用设置为 `--config FILE`、`--settings FILE`、`--data-dir DIR`。
执行还支持重复的 `--skills DIR`、`--hooks FILE`。
flags 可以放在位置参数前后；`--` 后的参数按原文处理。
run/chat 使用相同的确定性选模逻辑；排序按 REF 字符串，不按 TOML 文件顺序。
恢复使用保存的目录及模型，忽略全局默认；可通过 `--model` 显式切换到启动时装配的
另一个 REF。显式或保存 REF 不存在时明确报错；失效的 settings 默认 REF 使用配置内排序后的第一个模型；
没有配置模型也明确报错。工作目录不一致会报错。启动时在 stderr 显示 `Model: REF`，
JSON stdout 不混入启动文字。裸 `codeagent` 直接进入会话；交互终端使用 TUI。
默认数据库位置为 UserConfigDir/CodeAgent/sessions.sqlite；eval 应提供隔离的数据目录。
Store.Init 复用现有结构校验，不迁移旧数据库。

chat 的命令为 `/help`、`/exit`、`/status`、`/model`、`/model REF`。
`/model` 显示排序后的 REF 列表及当前选择；`/model REF` 通过 Runtime.SetModel 空闲切换，
失败时保留当前选择。每条任务顺序执行，不并发
预读用户输入；任务执行时需要审批会提示 once/session/reject，也支持 /help、/status、/exit。
普通聊天命令在任务间处理，模型切换遵守 Runtime 的空闲限制。
JSON chat 是逐行 JSON；提示和诊断只写 stderr。status 的 text 输出也采用结构化 JSON。
sessions show 输出结构化 JSON，包括元数据、当前历史、有限数量的 Run；归档会话仅显示
元数据与 Run，并明确标记缺少公开归档历史入口，查询不会启动 Runtime 或改写旧 Run。
list 过滤子会话后应用分页；show 可以查看子会话，run/chat 不允许将子会话当主会话打开。

## 集中装配

现有模型/MCP TOML 通过 config.Load、ModelEntries、MCPServers 复用。CLI 不记录或输出
APIKey、Headers、RequestOptions、MCP 地址/环境；模型状态只投影公开字段。
宿主原生集成可通过 Options 注入模型条目、Stream、可信 ToolBinding、WebFetch、原生
ModelHooks、HookSources、Instructions、Skills、Policy、Compact、长期记忆设置和子代理开关。
注入 MCP 时 Manager 必须已经启动，CLI 借用它，不关闭或重连它。

`--settings` 是用户显式选择的可信宿主 JSON 文件，不自动从项目文件读取权限设置。
相对 skill、Hook、全局指令、PlanDir 和 memory_directory 路径相对该文件目录；
Instructions.TargetDir 仍相对执行项目。CLI --skills/--hooks 则相对项目目录。
示例：

```json
{
  "default_model":"PROVIDER/ALIAS",
  "instructions": {"FileName":"AGENTS.md", "GlobalDir":"global-instructions"},
  "skills": [{"Path":"skills", "Name":"personal"}],
  "hook_files": ["hooks.json"],
  "policy": {"WorkMode":"build", "ApprovalMode":"default"},
  "compact": {"ReserveTokens":16384, "KeepRecentTokens":20000},
  "memory_directory":"project-memory",
  "memory_scope":"project-a",
  "subagents":true
}
```

`default_model` 是可选字段，只用于新会话；省略或空字符串使用排序兜底。
请把样例占位符换为配置中实际存在的 REF，或删除该字段。它只存在于 CLI settings JSON，
不扩展模型 TOML，不增加配置文件自动搜索。配置在启动时读取，修改后需重启。

未配置 skills、Hook、记忆、MCP、子代理或 WebFetch 时如实显示未启用，不偷偷发现来源。
默认内置工具在 Options.Tools 为 nil 时开启；空切片明确关闭它们。
项目 AGENTS.md 和配置的全局/目标指令由 Runtime 打开时加载。技能先发现元数据目录，
模型通过普通 read 工具按需读取正文/附件；技能不授权工具、不自动执行脚本。
DisableModelInvocation 技能没有专用显式调用命令，AllowedTools 仍仅是元数据。
ModelHooks 是原生 Go 回调，不能用脚本 JSON 冒充。
压缩复用 Runtime/compact 的预算、摘要和来源记录；默认采用 compact.DefaultSettings。
未知模型窗口不会自动压缩。可通过 Options 或 settings 提供不同设置。

后台记忆要求 directory 和 scope 成对提供；检索工具自动挂载。主 Run 正常成功后 Runtime
触发后台整理。RunHandle.Wait 不等待整理；关闭会取消并安全等待后台任务结束，不能宣称
单次 run 已完成记忆整理，JSON 的 memory_shutdown 字段明确这个语义。
子代理由模型工具触发；Host.OnEvent、Host.Opened、所有 Runtime.Snapshot 统一协调主子
会话审批。文件范围、占用、交付和循环等待检测留在 Runtime。自动发现的 MCP 不挂到
子代理；可信自定义工具仍受 Runtime 的角色和目标限制。

## 审批、终态与关闭

终态取本次 Submit 返回的 RunHandle.Wait；事件只提供唤醒和诊断。100ms 定时快照检查
恢复丢失的主子会话审批。回复后的审批 ID 在快照移除前不会重复提示。
非交互及 --stdin 模式不读取人工审批，不伪造 Grant；遇到 Ask 取消本次 Run，输出
approval_required 和退出码 3。终端 Reject 正常交给 Runtime，模型仍可继续回答。
最终 text 只取本次 Run 最后一条 assistant 的文本，不拼接过程消息；如果最后一条还
含工具调用，则没有最终文本。JSON 不直接序列化 Go error 或完整运行配置。
已装配的凭证值会从错误/最终输出中脱敏。

退出码：0 成功；1 失败（包含初始化、执行及清理失败）；2 参数错误；3 需交互审批；
4 deferred；124 超时；130 取消。success+length 保留 stop_reason=length，eval 可识别截断。
run 的 timeout 覆盖 stdin 读取和执行；chat 对每条任务应用 timeout。EOF 结束空闲 chat，
审批 EOF 取消当前任务。Ctrl+C 由 Main 转为 context 取消；Execute 的宿主自行提供信号 context。
取消后显式 Cancel 并用独立清理 context 等待；默认清理等待 15s，可由 Options 调整。
Host 拥有创建的 Store/MCP，关闭超时后仍继续资源收尾，CLI 不提前关闭正在使用的 Store。

输入只有一个按需读取者，不预读 chat 行以抢走审批输入。如果运行在提示输入期间自然
结束，尚未消费的那一次读取留给下一轮。任意 io.Reader 不一定可中断；拥有阻塞 reader 的
嵌入宿主应提供 IO.CloseInput。Main 提供 stdin.Close；不宣称强制停止任意第三方 reader。

## 文件职责及验收

types.go：公开入口、可信 Options、IO 和安全输出类型。
parse.go：命令语法和 flags。
assembly.go：宿主设置、模型/工具/Hook/MCP/Store 装配、角色查询及资源关闭。
input.go：单一按需输入读取。
execute.go：进程适配、命令分发、安全输出及通知。
run.go：RunHandle、审批和取消协调。
chat.go：连续对话、四个命令及状态投影。
sessions.go：查询和主会话列表分页。

临时验收副本及日志位于仓库外的 ../cli-review-20261010；无永久测试写入本包。
仅正式 Execute 流程，临时数据库/目录、假模型、假工具、假 MCP HTTP 传输；脚本 Hook 使用
测试程序自身作为可控 helper，没有真实 shell 业务、用户数据库或模型网络调用。

1. 聚合：`go test -mod=readonly -count=1 -timeout=90s -run '^TestCLI' -v ./internal/cli`。
   编译成功；9 组正式验收加 1 个 helper，3 组因回复审批后重复提示失败，其余通过。
2. 修复后限定重查：`go test -mod=readonly -count=1 -timeout=45s -run '^TestCLIFormal(ApprovalAndStdin|ChildApprovalAndRole|BorrowedMCP)$' -v ./internal/cli`，3 组通过。
3. 生产包构建：`go build -mod=readonly ./internal/cli`。
4. Windows 目录大小写对齐后，以及 TOML 装配和 chat/审批共享输入两个未决衔接风险：
   `go test -mod=readonly -count=1 -timeout=30s -run '^TestCLIFormal(RunRestoreHistory|ConfiguredAssembly|ChatApprovalInput)$' -v ./internal/cli`。
   恢复和共享输入通过；TOML 测试因夹具使用错误协议值 openai 失败。
5. 仅修复外部夹具为仓库定义的 openai-completions：
   `go test -mod=readonly -count=1 -timeout=30s -run '^TestCLIFormalConfiguredAssembly$' -v ./internal/cli`，通过。
   最终目录处理代码更新后再构建一次 CLI 包。总计 4 次定向测试调用、2 次 CLI 包构建，
   没有全仓测试、race、全仓 vet 或 hash。

覆盖：run/恢复/最终文本/持久化/list/show、chat/模型切换/JSON、Once/SessionTool/Reject/
非交互/管道/审批EOF、timeout/context取消/stdin超时/chatEOF、技能按需读取/项目指令/
脚本与原生 Hook、主子会话审批/主角色拒绝、借用 MCP 调用与所有权、记忆读取/关闭语义、
自动压缩及保存摘要、错误脱敏/help/version/参数错误/length。
另验证正常 config.Load/ModelEntries/MCPServers 装配：纯合成 TOML、禁用的 MCP 条目，
不读取真实配置、不发网络请求。11 组正式验收分别有通过记录，加 1 个 helper。
这些通过记录不是同一最终源码快照上的一次全套通过；没有重新运行整套测试。

| 正式测试名称 | 最近通过时的 CLI 源码 |
| --- | --- |
| TestCLIFormalRunRestoreHistory | 最终源码，已包含 Windows 路径大小写对齐 |
| TestCLIFormalConfiguredAssembly | 最终源码；只修正外部 TOML 夹具的协议值后重查 |
| TestCLIFormalChatApprovalInput | 最终源码 |
| TestCLIFormalApprovalAndStdin | 审批修复及最终文本修复后，目录大小写调整前 |
| TestCLIFormalChildApprovalAndRole | 审批修复及最终文本修复后，目录大小写调整前 |
| TestCLIFormalBorrowedMCP | 审批修复及最终文本修复后，目录大小写调整前 |
| TestCLIFormalChatAndModel | 首次聚合源码，后续两个修复前 |
| TestCLIFormalTimeoutCancellationInput | 首次聚合源码，后续两个修复前 |
| TestCLIFormalSkillsInstructionsHooks | 首次聚合源码，后续两个修复前 |
| TestCLIFormalMemoryAndCompression | 首次聚合源码，后续两个修复前 |
| TestCLIFormalOutputSafetyErrors | 首次聚合源码，后续两个修复前 |

入口集成后只新增根构建 `go build -mod=readonly -o ../cli-review-20261010/codeagent.exe .`，通过。
实际二进制的 help/version 返回 0；run --json 的参数错误返回 2；run 搭配合成 REF、
明确不存在的 config 文件和隔离 data-dir 返回有效 JSON，state=error、exit_code=1。
没有调用真实模型或 MCP。参数错误检查的外部捕获脚本先因空 stdout 未创建文件失败，
预创建输出文件后仅重查该场景并执行尚未运行的 JSON 场景；没有产品修复或扩大测试。
4 个入口场景共执行 5 次进程调用，记录见 entry-smoke.json；root-build.log 记录根构建。
未覆盖：真实终端 OS Ctrl+C、真实模型/远端 MCP、Linux 执行、任意第三方 reader 中断、
后台记忆文件发布边界的自然完成；已有 Runtime 剩余验收不能因此宣称全部完成。

已完成入口集成：main.go 改为 os.Exit(cli.Main(os.Args[1:]))；清理 internal/demo 的
agent.go、agent_test.go、audit.go、chat.go、complete.go、diagnostic.go、offline_acceptance.go、
online_acceptance.go、run.go、tool.go。源码中只有 main.go 导入 demo。
这些未提交修改与 CLI 重建结果一起留供审查，没有再次提交或推送。

## 默认选模 v2 验收

本次仅改 CLI 的装配、聊天展示、启动提示及本文档。针对选模改动在既有临时副本执行一次
最小聚合检查：`go test -mod=readonly -count=1 -timeout=60s -run '^TestCLI(Default|Formal(RunRestoreHistory|ChatAndModel|ConfiguredAssembly)$)' -v ./internal/cli`。
8 个顶层检查全部通过，覆盖默认值、排序确定性、显式优先、保存优先与切换持久化、
空模型/无效 REF、列表和切换、JSON 无污染，以及已有恢复/聊天/TOML 装配。
新增测试仅保存在 ../cli-review-20261010/validation，不引入永久测试。
日志为 ../cli-review-20261010/default-model-aggregate.log，未重跑全套、race 或全仓 vet。

正式根入口 Windows 构建通过。v2 发布包仅包含 exe、公开 TOML 样例、settings 样例和中文说明。
安装 exe 与发布 exe 逐字节一致，原用户 PATH 未变，未复制真实配置、凭证或数据库。
新 PowerShell 公开入口 7 项通过：裸帮助、help、version、chat 排序默认、settings 默认、
显式覆盖以及缺配置失败。chat 仅输入 /model、/model REF、/exit，无真实模型请求。
记录位于 ../cli-review-20261010/default-model-entry/report.json。

## Interactive TUI v3

`chat` uses the TUI only for an interactive terminal without `--json` or `--stdin`.
Pipes retain the headless protocol. The shared command registry supplies help,
completion and dispatch for both frontends. Enter sends, Alt+Enter/Ctrl+J inserts
newlines, Ctrl+C cancels, and Ctrl+Q cancels and waits for safe shutdown.

Configuration selection is explicit `--config`, embedded `Options.ConfigPath`,
then executable-directory `config.toml`; current-directory TOML is ignored.
An existing invalid file is never skipped. Configuration location does not change
the project directory. `/open` creates a new session in a validated directory;
`/session` restores the saved directory. Active work prevents either switch.

The terminal adapter queues observer events without blocking the Runtime driver,
recovers approvals from Snapshot, routes each approval to its originating Runtime,
and uses the submitted RunHandle.Wait receipt for completion. CLI owns shutdown
of its MCP Manager across project/session transfers. `/mcp refresh` and reconnect
reuse saved Manager configuration; neither reloads TOML. Tool and memory views
state the limits of their existing public interfaces.

Validation lives outside this repository in ../cli-review-20261010/validation.
The v3 aggregate passed 16 of 17 selected top-level tests; the remaining child
approval fixture was corrected and the main/child approval recheck passed.
The targeted layout test passed after the compact status/footer update. The owned
MCP test passed with a controlled localhost SDK server after sandbox restrictions
on loopback sockets were lifted for that check. It covers refresh, reconnect,
Manager transfer and no config hot reload. No real model, MCP service or user
storage was used, and no permanent tests, full-suite/race/vet checks were added.
The rare Host.Close timeout branch was reviewed but not forced in a separate test.

## Frontend v4

Transient command receipts now expire after three seconds and are not transcript
items. Command inspection uses the existing scrollable dialog; `/model` and
`/sessions` open existing choice dialogs. Headless result values and JSON schemas
are unchanged. Errors and Runtime diagnostics/notices are retained outside chat
until explicitly acknowledged; snapshots recover them without reordering chat.
Esc dismisses an error in the normal view, Ctrl+L acknowledges the first retained
notice, and F1 opens details when no approval is active. Approvals keep their own
scope, controls and Snapshot recovery. The frontend does not inject model identity.

The external v4 targeted suite selected seven top-level tests across CLI/TUI,
including real toast timers, stale timers, repeated commands/model changes,
selection cancellation, busy restrictions, session switches, error acknowledgement,
new answers without old success notices, main/child approvals and headless JSON.
All passed. No permanent tests or whole-repository/race/vet checks were added.

The current model appears first in the status line so a long session title cannot
hide it. Model selection and session creation use concise success receipts.

## Single default configuration and direct entry

Bare `codeagent` enters interactive chat; `chat`, `help`, `version`, `run` and
noninteractive flags remain available. Default configuration comes only from
`config.toml` next to the executable, independent of the current project directory.
Explicit --config/embedding overrides select one file and never merge catalogs.
Platform/model menus come entirely from that selected file. An unavailable settings
default_model falls back to the first sorted configured REF; an explicitly selected
unknown REF remains an error. Saved sessions still require a configured REF.

TUI 模型菜单优先显示 display_name（空名称回退 REF），详情显示实际 ID 和 REF；选择仍使用 REF。/status 首屏先列模型名称、实际 ID、项目目录、会话状态，再展示运行和能力详情。普通操作错误的短提示自动消退，F1 查看本次 UI 错误历史；重要通知及待审批请求保持可见。
