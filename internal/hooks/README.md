# Hooks

独立的 Claude Code command hooks 兼容组件。调用方显式提供已经授权的配置和真实事件数据；本包匹配配置、运行命令、解析和合并结果。调用方负责执行权限、工具 schema 校验、持久化、模型上下文应用以及 Agent.Resolve。

组件已提供 8 个事件入口；当前尚未接入 Runtime。[LoopGuard](../../examples/loopguard/README.md) 位于 `examples/loopguard`，脚本示例位于 `examples/hooks`，后续接入另行处理。

## 使用

配置可以来自 settings JSON 中的 `hooks` 部分，或插件 `hooks/hooks.json`。本包接受完整 JSON 文档，不扫描用户目录，不安装插件。

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "(?i)^(write|edit)$",
        "hooks": [
          {
            "type": "command",
            "shell": "powershell",
            "command": "& \"${CLAUDE_PLUGIN_ROOT}/check-tool.ps1\"",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

上例假设调用方将仓库的 `examples/hooks` 目录作为真实 PluginRoot，脚本为 [check-tool.ps1](../../examples/hooks/check-tool.ps1)。脚本阻止 write/edit 修改 `.env`，兼容 `path` 和 `file_path` 字段。它没有决定的调用继续走宿主原有权限流程。命令 hook 自身失败按兼容协议给出诊断；宿主可以另行要求特定策略 hook 必须成功。

推荐通过 exec 形式引用路径，可以精确传递空格、引号、`$` 和反引号，无需 shell 转义：

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "pwsh",
            "args": ["-NoLogo", "-NoProfile", "-NonInteractive", "-File", "${CLAUDE_PLUGIN_ROOT}/check-tool.ps1"],
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

Go 调用方示例（目录、配置均由外部提供）：

```go
engine, err := hooks.New([]hooks.Source{{
    ID: "project-policy", Kind: hooks.PluginSource,
    JSON: configJSON, PluginRoot: absolutePluginDirectory,
}}, hooks.Options{})
if err != nil { return err }

result, err := engine.PreToolUse(ctx, hooks.PreToolUseInput{
    CommonInput: hooks.CommonInput{
        SessionID: sessionID, CWD: absoluteWorkingDirectory,
        ProjectDir: absoluteProjectDirectory, PermissionMode: "default",
    },
    ToolInput: hooks.ToolInput{
        ToolName: "write", ToolUseID: callID,
        ToolInput: json.RawMessage(`{"path":".env","content":"example"}`),
    },
})
if err != nil { return err } // 事件错误时不应用控制或改写，执行证据仍在 Reports 中。
// 检查 result.Reports 中每个 Report.Error 及底层 Execution。
// StopProcessing、Deny、Ask、Defer 各自需要宿主分支处理。
// UpdatedInput 仍需经过工具 schema 和权限校验。
// Allow 是钩子建议，宿主管理的限制仍然生效；没有决定不等于 Allow。
```

合法动作通过各事件的 Result 直接返回，无需宿主布尔开关。Runtime 必须正确应用每种 Result，包括 ask/defer、输入和输出改写、ContinueTurn、StopProcessing 与 StopLoop，不得静默忽略未实现的控制动作；遇到无法应用的动作必须明确报错。Runtime 接线在后续完成。事件输出白名单、事件适用性与返回类型约束仍由本包校验，未知协议字段仍返回 `ErrUnsupported`。停止后不再应用询问或改写；权限拒绝或延后不再应用输入改写。普通命令或脚本输出问题保留在 `Report.Error`，不伪装成 allow 或 deny。

`Notices` 仅包含 `Reports` 和 `SystemMessages`。`Report.Error` 保存单个钩子的执行或协议解释问题；`Execution` 保留原始状态、退出码、输出、耗时及底层错误，两者中的同一底层错误不会重复合并。事件方法返回的 `error` 用于输入非法、宿主取消和不支持的协议输出的情况，宿主不得吞掉该错误继续执行。有效 stdout 与 stderr 编码问题可以同时存在，因此 `Report.Error` 非 nil 不等于全部控制失效；主动拒绝由事件结果表达。

## 事件与结果

每个事件使用自己的 Go 输入/结果类型，脚本收到 Claude 风格的 JSON。事件名由入口生成，不能由调用方伪造；SessionID、绝对 CWD、工具调用 ID 与参数对象按类型验证。

| 入口 | 主要输入 | 返回语义 |
| --- | --- | --- |
| SessionStart | source、可选 model/title | AdditionalContext、通用停止处理指令 |
| UserPromptSubmit | prompt | Rejected、Reasons、AdditionalContext |
| PreToolUse | tool_name/tool_input/tool_use_id | Permission、完整 UpdatedInput、AdditionalContext |
| PostToolUse | 最终 tool_input、真实结构化 tool_response | Feedback、AdditionalContext、模型可见 UpdatedToolOutput |
| PostToolUseFailure | 最终 tool_input、error/is_interrupt | Feedback、AdditionalContext；没有改成功的能力 |
| PostToolBatch | 完整且有序的 tool_calls | AdditionalContext、StopLoop |
| Stop | stop_hook_active、last_assistant_message、可选任务列表 | ContinueTurn；continue:false 优先停止 |
| SessionEnd | reason | 报告；不返回控制决定，JSON 输出被丢弃 |

输入及嵌套数据在调用期间只读。序列化送给脚本；返回改写保存在独立字节中。`updatedInput` 替换整个对象，不浅合并；`updatedToolOutput` 只更改给模型看的投影，不修改原始结果或工具成功/失败事实。

`BatchTool.Status` 是明确的宿主状态（成功、失败、拒绝、取消、不可用），不作为自创字段发给脚本。批次保留调用方提供的顺序和最终模型可见 tool_response；它与 PostToolUse 的结构化原始输出不同。每个调用 ID 唯一，不能以一个完成节点代替完整批次。

## 执行与合并

- 一个事件的所有匹配命令并行执行；每个脚本看到同一原始事件。报告、上下文和反馈按配置来源/组/handler 顺序合并，不按进程结束先后。
- 相同 settings handler 在匹配之后去重；不匹配的早期组不会遮住后面的匹配。插件副本保持独立。
- PreToolUse 的权限优先级：deny > defer > ask > allow；没有决定维持原有流程。最终 deny/defer 不应用参数改写，defer 也不应用附加上下文。先确定权限，再归并实际会使用的输入改写。
- 多个合法参数/输出改写按配置顺序采用最后一个明确提供的非 nil 值，整体替换，不做深层合并；未提供字段不清空前值。`updatedInput` 必须是对象，空对象合法，null 仍按现有协议校验拒绝；输出字段允许明确的 null，且它与缺席不同。同一 PostToolUse 同时提供两个输出字段时，`updatedMCPToolOutput` 优先于 `updatedToolOutput`，MCP 专用字段仍只适用于 MCP 工具。后配置钩子看到的仍是原始事件，不是前一钩子的改写。
- `continue:false` 高于事件自身的决定，结果只返回停止控制与通知，不再返回许可、改写或续跑请求。PostToolUse 的 block 是反馈，工具副作用已发生；PostToolBatch 的 block 停止下一次模型调用；Stop 的 block 请求继续本轮。
- 有效标准 JSON 在退出码 1 等非零码上仍被解释。退出码 2 的事件效果不能被 allow 覆盖；无效 JSON 同时保留诊断。退出码 2 在工具后是反馈，在 Stop 是续跑，在 SessionEnd 不阻止结束。
- 无效 JSON、启动失败和 command 超时为带诊断的无决定结果，宿主仍保留权限检查。输出超限和取消时丢弃部分/迟到控制结果。宿主 context 取消则返回错误及报告，不返回业务决定。
- stdout 独立承担协议控制。非 UTF-8 stderr 保留原始字节并给出显示诊断，用替换字符生成可读消息，不撤销合法 stdout 决定。UserPromptSubmit 拒绝时不返回不会被应用的附加上下文。

## 兼容矩阵与边界

官方文档基线：2026-10-08 的 [Claude Code Hooks reference](https://code.claude.com/docs/en/hooks)。调研快照 SHA-256：`403645d3a96659f4eaed1755a30ef28fff4477f711fd84acc7833a1949c168dd`。这是日期基线，不声称永远兼容最新版。官方定义覆盖的能力多于本版本。

| 能力 | 本版本 |
| --- | --- |
| 上述 8 个事件、同步 command handler | 支持，独立行为已验证 |
| hooks/事件/matcher/hooks 配置结构 | 支持完整 settings 文档和 hooks.json，来源由宿主显式提供 |
| matcher | 普通名称精确匹配；`Edit|Write`、`Edit, Write` 为精确列表；包含其他字符时使用正则。工具事件匹配工具名，SessionStart 匹配 source，SessionEnd 匹配 reason；无 matcher 事件忽略其 matcher |
| 正则表达式 | Go RE2；不支持 JS lookaround/backreference，编译时明确报错 |
| shell / args / timeout / statusMessage | 支持；args 存在即 exec，空数组也不经过 shell |
| once | 接受但忽略；本版本没有 skill frontmatter 来源 |
| if / async / asyncRewake | 不支持，构造时拒绝，不静默运行 |
| prompt / agent / HTTP / MCP tool handler、其他事件 | 不支持，构造时拒绝 |
| permissionDecision、旧 PreToolUse approve/block | 支持解析；ask/defer 必须由宿主正确应用 |
| updatedInput | 完整对象替换；宿主仍须工具 schema/权限校验 |
| updatedToolOutput / updatedMCPToolOutput | 模型投影；MCP 专用字段仅适用 mcp__ 工具；同一钩子双字段以 MCP 专用字段优先 |
| additionalContext、systemMessage、continue/stopReason | 按事件解释，停止动作须宿主正确应用；SessionEnd 丢弃输出 |
| suppressOutput | 协议仍接受并忽略；Go 结果不提供该字段 |
| SessionStart initialUserMessage/sessionTitle/watchPaths/reloadSkills、classifierContext、terminalSequence | 不支持，诊断而不假装应用 |
| 插件市场、Mods、skills 注册、user_config、环境持久化文件 | 不支持 |
| Claude 工具参数/transcript 格式完全仿真 | 不提供；保留真实工具名/参数，没有自动别名或 schema 转换 |

配置/输出未知字段拒绝并诊断；重复 JSON key、非 UTF-8 和过深 JSON 也拒绝。只支持单个 JSON 输出对象；多条 JSON 对象混合输出会报解析错误，和 Claude 对无控制字段 JSONL 的特殊纯文本回退存在差异。标准字段不在 JSON 中时，SessionStart/UserPromptSubmit 的纯文本成功输出作为上下文；其他事件不把普通 stdout 当决定。

执行器默认 input/config 4 MiB、stdout 1 MiB、stderr 256 KiB、配置最多 256 个 handler，可显式调整。timeout 为秒，允许范围 (0, 86400]。普通事件默认 600 秒，UserPromptSubmit 30 秒；SessionEnd 默认共享 1.5 秒预算，settings timeout 可提高至最多 60 秒，插件 timeout 不提高整体预算。环境变量调整 SessionEnd 预算的机制暂不实现。

Unix 默认 `sh -c`，显式 bash 使用 Bash；Windows 默认 Git Bash，未找到则 PowerShell，显式 bash 不会回退到 PowerShell。不把 Windows WSL 的 bash.exe 当成 Git Bash。宿主可通过 RunnerOptions 指定解释器路径；PowerShell 优先 pwsh，缺失时使用 powershell，显式路径失败不回退。exec 的 `./hook` 等相对程序路径按本次事件 CWD 解析，裸程序名按宿主 PATH 查找。Windows .cmd/.bat 不能按直接程序形式执行，应选择 shell 或底层真实程序。

exec 形式一次性替换 `${CLAUDE_PROJECT_DIR}`、`${CLAUDE_PLUGIN_ROOT}`、`${CLAUDE_PLUGIN_DATA}`，插入的真实路径不会被再次当作占位符展开。Shell 形式使用环境展开，PowerShell 只将这些拼写转换为 `${env:...}`；引号、注释和转义由解释器处理，本组件不自行扫描 shell 语法。单引号中的内容按解释器规则保持字面量，引用真实路径时用双引号或 exec args。未知插件目录不导出，原始配置引用缺失目录的路径占位符时拒绝执行。ProjectDir 可显式提供，不提供时使用真实 CWD；transcript_path 只在宿主提供真实绝对路径时出现。

Windows 使用启动时挂起、加入 kill-on-close Job Object 后恢复的方式，避免进程在被管理之前生成子进程；Unix 使用独立进程组。超时、取消、输出超限和主进程正常退出均回收子进程。本版本没有后台任务，继承 stdout 的子进程未退出时，有限 WaitDelay 可能产生进程失败诊断，进程树仍被回收。

## 原生模型阶段

`ModelHooks` 独立提供具名 `RegisterBeforeModelRequest` 与 `RegisterAfterModelResponse`。请求转换按注册顺序串行执行，首个错误即停止且不返回部分转换。`AfterModelResponse` 返回汇总 error：普通观察错误继续执行后续回调，context 取消时停止；错误带回调名称，以 `%w` 包装并用 `errors.Join` 汇总，保留 `errors.Is/As`。panic 转成错误；注册在一次调用中的快照不变。

它不向脚本发明 BeforeModel/AfterModel 事件，不依赖 agent.RequestContext。`hooks.RequestContext` 只含 SystemPrompt/Messages；不提供模型凭据和工具执行权。输入及共享消息内容递归只读，替换时仅复制消息列表，不每次深拷贝整个历史。回调必须响应 context，本包不能强制杀死一个不返回的 Go 函数。本版本无需 next 框架。

## 实现组织

- `engine.go`：按初始化、事件入口、公共分发、并行执行、结果归并组织。`Engine/New` 在此；`dispatchResult` 只保存内部归并状态。
- `config.go`：声明 `Source/Options`，解析并编译来源、分组、handler、matcher 和去重键，不自行加载文件。
- `events.go`：公开事件、共享输入、权限及报告通知；8 个 Input、8 个 Result 分别集中排列，不放内部协议中间类型。
- `command.go`：声明命令执行 API、进程状态和 `Execution`；准备环境与路径、解析解释器、有界执行、等待、取消、超时及清理。每个输出缓冲区仅取一次快照。
- `protocol.go`：输入验证与编码在前，输出解释与解析在后；`preparedEvent`、wire 和 effect 类型保持内部使用。
- `json.go`：共享 JSON 编码、重复键、深度和形状检查；已经校验的嵌套数据仅解码，不重复检查整个子树。
- `model_hooks.go`：独立的原生 Go 模型回调，不进入脚本或子进程执行主线。
- `process/process_windows.go`、`process/process_unix.go`：平台进程树实现，提供最小的 `Start`、`Tree.Kill/Close`；只依赖标准库与原有平台库，不反向依赖 hooks。

主线阅读顺序：`New` 编译配置 → 事件入口 → `dispatch` 验证编码并选钩子 → `execute` 并行执行 → `buildCommand` 准备命令 → `CommandRunner.Run` 启动、等待并回收进程树 → `interpret` 解释输出 → 按配置顺序归并控制与事件结果。回收发生在 `Run` 返回前，输出解释在回收之后。

## 未来核心接入清单

旧 Runtime/config 的 hooks.Registry、BeforeTool、AfterTool、Completed 引用需要迁移，当前未保留旧 API 双实现，也未修改旧 Runtime。移除 hooks 对 agent 的依赖不等于旧核心立即可构建。

1. ToolStarting → PreToolUse；落实停止、权限和完整参数替换，再调用 Resolve。当前 Continue/Skip/Stop 不足以直接承载 ask/defer。
2. ToolFinished → 依据明确执行事实分发成功/失败；拒绝、取消、未知工具不冒充已执行失败。提供真实最终参数和结构化输出。
3. 工具批次全部结束 → PostToolBatch；使用模型声明顺序，提供最终模型可见内容，落实 StopLoop。
4. 输入正式接受前 → UserPromptSubmit；拒绝时明确持久化处理边界。
5. 正常结束提交前 → Stop；落实 ContinueTurn、stop_hook_active 和连续续跑上限，不能重新执行已完成工具。
6. 会话真实创建/恢复/终止 → SessionStart/SessionEnd；不简单映射 Run 的开始/结束。
7. ModelRequestReady/ModelResponseReady → 原生 ModelHooks；转换成 Agent 自身决策由核心负责。

## 验证

2026-10-09 删除宿主 Capabilities 后，构建、vet、gofmt 及本轮临时 overlay 行为测试通过：20 个行为测试函数、56 个子测试和 2 个进程夹具入口，共 78 个通过条目。合法 ask/defer、输入/输出改写、ContinueTurn、StopProcessing 无需能力开关即可返回，StopLoop 同样直接返回；跨事件输出字段、未知字段、错误事件名、非法输入改写类型和非 MCP 工具的 MCP 专用输出仍拒绝。复用测试同时覆盖权限与停止优先级、改写归并、报告错误、取消、超时和 Windows 进程树清理。生产目录未增加测试文件；本轮未运行 race、跨平台行为测试或全仓构建，Runtime 接线仍待后续完成。以下为此前轮次的独立验证记录。

2026-10-09 的文件组织调整通过 128 个根包声明的 AST 等价核对；平台文件只适配包名和必要导出名称，启动/终止/关闭逻辑及编译指令保持不变。临时 overlay 的 18 个行为测试函数、42 个子测试及 2 个进程夹具入口通过，其中实际验证 Windows 后代进程在正常退出、超时、取消和输出超限后停止心跳。hooks 与 process 子包构建、vet、gofmt 通过；Linux、Darwin 的 amd64 交叉构建通过，仅 Windows 执行了本轮行为验收。测试保留在项目外任务目录，没有生产测试文件。

2026-10-09 的本轮精简使用任务目录中的临时 overlay 测试，不新增生产测试文件。17 个行为测试函数及 38 个子测试通过（另有 1 个子进程夹具入口），构建、vet、gofmt 和 Windows race 检查通过。覆盖配置顺序与完成顺序相反的输入/输出改写、缺席与空对象/null、双输出优先级、权限与停止优先级、单钩子失败继续、有效 stdout 与 stderr 问题共存、非法输出、8 个事件的退出码 2 语义、取消/超时/能力不足，以及模型回调错误链、继续/首错停止和 panic 保护。此记录仅验证 hooks 包，不代表旧 Runtime 已接线或全仓可构建。

2026-10-08 初版验收曾有 93 个测试项通过。审查修正后重新执行 97 个测试项（含子测试），0 失败、0 跳过，覆盖审查中的所有复现、8 个事件 wire 数据与控制、有效/失效改写冲突、输出别名、匹配和去重、并行屏障、并发快照、相对执行路径、字面量参数、JSON 边界、启动失败、输出限制、超时、取消和 SessionEnd 预算。初版及修正验收分别记录，不用初版结果代替修改后的验证。

按用户要求，上述初版与修正验收后已删除当时新增的测试文件及临时测试产物，保留实现、文档和示例。以下命令用于检查交付代码；当前目录不包含行为测试，本轮临时测试保留在项目外任务目录。

```text
go build ./internal/hooks/...
go vet ./internal/hooks/...
```

2026-10-08 验收时，真实 PowerShell 7、Windows PowerShell、Git Bash 均通过注释、单引号字面量、中文及特殊字符环境路径检查。Linux/macOS 仅交叉编译通过，未在这些平台执行行为验收；当时 CGO_ENABLED=0，未执行 race。该轮完整实施与验证记录见 `docs/待做计划.md` 的 Hooks 部分第 10、11 节。
