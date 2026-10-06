# 独立 CLI 适配层

本包提供命令解析、现有后端调用、单次执行输出和会话内交互意图，尚未修改 main/cmd，不包含 TUI 或信号注册。使用标准库；调用方注入 Backend、安全模型列表、TUI、context 和 IO，并负责真实进程入口及共享后端资源生命周期。

`New(Config)` 构造 CLI，`Run(ctx, args, IO)` 返回退出码，不调用 os.Exit。`AdaptHost(*runtime.Host)` 只适配接口返回值，复用现有 Host/Runtime。`ProjectModels(cfg.ModelEntries())` 只保存 Ref、Provider、ID、DisplayName、API、Reasoning、ContextWindow、MaxTokens，不保存或输出 APIKey、Headers、BaseURL、RequestOptions。`Models()` 返回展示数据副本。未注入版本时显示 development。

## 终端命令

| 调用 | 行为 |
| --- | --- |
| `codeagent` | 启动注入的 TUI，初始未选择会话；未注入 TUI 时明确报错。 |
| `codeagent --session ID [--model REF] [--directory DIR]` | 恢复会话；省略 model 时沿用已保存选择。目录只允许首次绑定。 |
| `codeagent --model REF [--directory DIR]` | 明确模型创建并打开新会话，然后进入 TUI。 |
| `codeagent run "任务" [--session ID] [--model REF] [--directory DIR] [--json]` | 提交并等待一轮任务。已有会话不要求再次指定模型；新会话必须指定模型。 |
| `codeagent session list [--search TEXT] [--limit N] [--offset N] [--archive-filter active\|archived\|all] [--json]` | 标题字面包含搜索、分页、归档过滤分别传给后端。默认 active、limit 50，后端 limit 上限 200。 |
| `codeagent session create ["标题"] --model REF [--directory DIR] [--json]` | 只创建会话元数据，不打开 Runtime。 |
| `codeagent session rename ID "标题" [--json]` | 请求重命名。 |
| `codeagent session archive ID [--json]` | 请求归档；不先取消任务或另行调用关闭。 |
| `codeagent session unarchive ID [--json]` | 恢复归档状态，不自动打开。 |
| `codeagent models [--json]` | 只输出安全模型字段。 |
| `codeagent --help` / `-h` / `--version` | 独立使用，不执行后端操作。 |

选项支持 `--name value` 和 `--name=value`，可位于命令前后；未知、重复或不适用选项报错。用 `--` 分隔以 `-` 开头的位置参数。任务和标题各为一个参数，含空格时由终端引号分组。

新建没有默认模型。恢复时可显式覆盖 model；已绑定目录的会话须省略 directory，不能更换、解绑或重复绑定。模型解析、目录存在性、会话所有权、活动任务限制和持久化仍由后端校验，不在 CLI 复制。

## 会话内命令

`Command(ctx, ChatState, input)` 返回结构化 CommandResult，不打印聊天 UI。TUI 根据 Intent 更新界面并保留提交返回的句柄，不使用 `--json`。

| 输入 | 结果或意图 |
| --- | --- |
| 普通文本 | 原样提交任务，返回 task_submitted 和 Handle。 |
| `/new ["标题"] --model REF [--directory DIR]` | 创建并打开，返回 enter_session、Runtime 和新会话元数据。 |
| `/sessions [--search TEXT] [--limit N] [--offset N] [--archive-filter active\|archived\|all]` | 返回会话列表。 |
| `/sessions ID [--model REF] [--directory DIR]` | 打开指定会话，返回 enter_session。 |
| `/rename "标题"` | 重命名当前会话。 |
| `/archive` | 归档当前会话；成功返回 select_session。失败不主动取消任务。 |
| `/close` | 关闭当前 Runtime，返回 select_session，不删除或归档。 |
| `/quit` | Backend.Close 等待整体收尾，返回 quit。 |
| `/model [REF]` | 无参数返回安全状态展示和模型列表；有参数调用 SetModel。 |
| `/directory [DIR]` | 返回安全状态展示中的目录或请求首次绑定。 |
| `/status` | 返回安全 StatusInfo 展示。 |
| `/steer "指令"` | 提交活动 Run 的补充指令，由后端安全消费。 |
| `/cancel` | 只取消已持有句柄对应的当前任务，不接受 Run ID。 |
| `/history` | 返回当前分支持久化历史。 |
| `/runs [--limit N] [--offset N]` | 返回本会话 Run 列表。 |
| `/run ID` | 返回 Run 详情、消息、工具事实和用量。 |
| `/usage [ID]` | 查询指定或当前活动 Run；无活动任务且未给 ID 时拒绝，不是会话累计统计。 |
| `/help` | 返回帮助文本。 |

ChatState 只保留当前句柄。多次 Submit 时 TUI 必须按 Run ID 保留所有返回的句柄，并随 Runtime.Snapshot().Agent.CurrentRunID 变化选择 CurrentHandle；不能用最新排队句柄覆盖当前任务。CLI 遇到归属不匹配会拒绝取消，不能从后端按 ID 找回被丢弃的句柄。

CommandResult.Snapshot 为 CLI 的 StatusInfo，不直接序列化后端 Snapshot。只包含会话 ID、运行时状态、固定故障说明、已选择模型引用、目录、Agent 状态、当前任务、队列长度和 ID、历史条数以及当前/待应用模型的安全 ModelInfo。模型展示与 ProjectModels 共用字段投影，不输出 BaseURL、Pricing、RequestOptions、APIKey、Headers 或整份 History。原始故障文本不通过此展示入口返回；/history 仍是专门的历史入口。当前/待应用 llm.Model 不携带配置引用，ModelRef 单独表示已保存的选择，不推测模型别名。

`Split` 只用单/双引号分组，反斜杠永远保留。例如 `/directory "C:\工作 空间\项目\"` 保留结尾反斜杠。可用另一种引号包住含引号的文本；没有反斜杠转义、变量、通配符、管道或命令展开。

## 输出与收尾

单次 run 文本 stdout 只含本轮 assistant 文本块，按发生顺序以换行连接；不输出思考块、工具消息或日志。JSON stdout 输出一个 RunOutput 对象及换行：session_id、run_id、state、text、steps、total_usage、可选 stop_reason/error。解析成功后的创建、提交和等待失败也用此对象报告；语法错误只写 stderr，不输出 JSON。其他命令的 JSON 输出为对应安全 DTO，修改操作返回 action/id。

诊断写 IO.Err。退出码为成功 0、执行或输出失败 1、参数错误 2、调用方取消/超时或明确 aborted 130。调用者 context 仍有效时，Wait 的 DeadlineExceeded 只是等待失败，返回 1，不调用 Cancel，也不将状态标为 aborted。RunResult 的 Go error、内部模型配置及整个会话历史不参与单次结果序列化；注入后端和事件消费者也必须将日志写到 stderr，不能自行写 stdout。

调用者 context 真正结束时，单次 run 请求该句柄 Cancel，再用保留 context 值但去掉取消的 context 等待收尾。交互层可在 EOF/中断时调用 EndInput；TUI.Run 返回最后状态后，CLI 也会执行此收尾并关闭 Backend。自然 EOF 正常退出，清理失败仍报错。取消为合作式，不承诺强杀 Go 回调。

普通管理命令和单次 run 返回时不关闭共享 Backend；独立进程的调用方须在退出前 Close。/quit 和 TUI 退出会调用 Close。CLI 不写数据库，不实现权限锁或任务生命周期，不提供按 Run ID 远程取消、会话删除或 SwitchBranch/undo。

当前没有审批接口：CLI 不自动批准操作，无法承接需要人工确认的无人值守任务；需要批准时必须由后端拒绝执行或由未来交互审批层处理。MCP、Skills、Memory、子代理、权限模式和 compact 未接入，不提供占位命令。
