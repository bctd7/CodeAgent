# CodeAgent

CodeAgent 是 Go 编写的终端代码代理，提供交互 TUI、连续聊天、单次任务和会话查询。

> **审查状态：`internal/runtime`、`internal/tools`、`internal/permission`、`internal/tui`、`internal/cli` 尚未完成系统性代码审查。** 已进行局部问题排查与回归验证；测试通过、构建通过或本次发布前差异/安全核验，都不等于这五个模块已审查通过。

## 当前架构

```text
main → CLI：配置选择、依赖装配、命令、资源关闭
         ↕ TUI：展示与操作意图，不执行模型/权限/存储业务
       Runtime Host / Runtime：流程协调与宿主资源边界
         ├─ Agent + 唯一 Driver：模型循环、工具调度、输入和 Run 生命周期
         ├─ Session / Store / SQL：原始事实、工具投影、会话历史和持久化
         ├─ Tools：文件、shell、网页读取、MCP 适配和子代理工具接口
         ├─ Permission：可信能力规则与隔离的会话授权
         ├─ Hooks / Instructions / Skills：事件处理与提示来源
         └─ Compact / Longterm / MCP：独立领域实现及资源生命周期
```

| 模块 | 职责与边界 |
| --- | --- |
| CLI | 选择唯一模型配置与运行设置，解析命令，创建/恢复 Runtime，维护资源归属；不打印凭据 |
| TUI | 消费公开投影，展示菜单、表单、审批和流式文本，提交操作意图；不实现业务或判权限 |
| Runtime | 只编排已有包；唯一 Driver 按节点完成 Hook、权限、持久化与工具结果交接，不另写 Agent 循环或 SQL |
| Agent | 执行模型循环、并发工具、队列/纠正/取消及结束候选；RunHandle.Wait 是完成依据 |
| Session | 保存原始消息与来源；原始工具结果先 pending，Hook 后独立确认模型可见投影；恢复不重放模型/工具/Hook |
| Tools | 实现工具领域行为；子代理适配层转发给绑定父作用域的 Controller，由 Runtime 管理独立子 Session/Agent |
| Permission | 可信宿主提供 Source/Capability/完整最终参数；显式禁用、工作范围和审批分别判断，子会话不复制父授权 |
| Hooks / Instructions / Skills | Hooks 处理事件与建议；Allow 不覆盖普通权限，Ask 仍要求本次确认；指令/技能由现有独立包发现和加载 |
| Compact / Longterm | Runtime 接线预算、来源与保存；整理复用触发时主模型、固定批次和六工具事务，失败不推进进度；不复制恢复算法 |
| MCP | Manager 管理连接与版本；Runtime 使用静态目录，在空闲边界刷新；共享 Manager 由外部拥有者协调连接操作 |

工具调用顺序是前 Hook → 最终参数/schema/目标校验 → Permission/必要审批 → 记录启动 → Agent 执行 → 原始结果入库 → 后 Hook → CompleteToolProcessing → Resolve。空替换与保留原文有不同含义，pending 正文不回退暴露。

子代理仅提供 `spawn_agent`、`send_input`、`wait`、`cancel_agent`。explore 只有只读工具；worker 可受声明文件范围限制，默认没有 shell；子 Runtime 默认不自动安装 MCP，不能创建孙任务。主 Run 正常结束前收齐子任务；只向父模型传公开正文、状态与追溯 ID，原始 thinking/签名留在子会话。交付按具体 child Run 判断：已保存且实际模型可见的完整工具结果不再触发收尾重复反馈，pending、被隐藏或不完整结果不提前消费交付。收尾自动交付复用 `child_run` 来源键；同 child 由 send_input 开启的新 Run 独立交付。查看任务面板不消费交付。

## 权限规则

四个维度分别是：Source（Builtin/External）、Capability（ReadOnly/FileWrite/Shell/Orchestration/Unknown）、WorkMode（Plan/Build）、ApprovalMode（Default/AutoEdit/AutoApprove）。ReadOnly 是可信已知读取操作，不承诺零 I/O；网页抓取也归入读取。

`Orchestration` 仅表示可信宿主内部任务生命周期管理，不是通用免审标记。Build 下内置只读与编排允许；Plan 仍只允许内置只读及指定计划目录的 Markdown 写入。显式 DeniedTools 优先；Build 下 External 询问，Plan 下拒绝。AutoEdit 额外允许工作目录内普通写入；AutoApprove 不突破显式禁用、Plan 或 External 边界。父调度放行不替代子任务真实工具权限、文件范围或 Hook Ask。

## 构建与启动

需要 Go 1.26 或以上。在源码目录执行：

```powershell
go test -mod=readonly ./... -count=1 -timeout=60s
go build -mod=readonly -trimpath -o codeagent.exe .
.\codeagent.exe --version
.\codeagent.exe --help
```

正式发布由已提交的干净源码构建；用完整 Git commit 与 EXE SHA256 追溯，不能只依赖当前显示的 `1.0.0`。`go version -m codeagent.exe` 可查看 VCS revision 与 modified 状态。

从要处理的项目目录运行 EXE；示例安装目录不限定为当前项目：

```powershell
Set-Location C:\Projects\MyProject
& 'C:\Apps\CodeAgent\codeagent.exe' --data-dir 'C:\Apps\CodeAgent\data-v6'
```

**当前 schema 为 6，不自动迁移 schema 5。** 首次使用新版请显式选择独立新 `--data-dir`，不要让默认路径误用旧库。默认路径仍是 UserConfigDir/CodeAgent/sessions.sqlite（Windows 通常 `%APPDATA%/CodeAgent/`）；程序不会自行改默认路径、迁移、清除或回填旧库。

## 配置与生效

只加载 EXE 同目录的 `config.toml`；显式 `--config FILE` 可选择另一份，不搜索或合并项目 TOML。模型 REF 为 `[providers.PROVIDER.models.ALIAS]` 对应的 `PROVIDER/ALIAS`，`id` 是远端模型名，`display_name` 用于界面。协议值为 `openai-completions` 或 `anthropic`。用户本地填写连接字段与凭据，程序不重写它们；更换模型配置需要重启。

只加载 EXE 同目录 `settings.json`，或显式 `--settings FILE` 指定的一份；不合并。缺省文件使用内置默认值，无效/未知字段报错。新会话按 `--model` → settings.default_model → 排序首个配置 REF 选择；恢复使用保存模型，显式参数可覆盖。失效默认值回退，显式或保存的未知 REF 报错。

`subagents`、`web_fetch` 默认关闭；长期记忆新会话默认开启，可在新建表单或 `--no-memory` 关闭，恢复保留会话值。记忆按项目隔离；显式 memory_directory/memory_scope 成对配置，CLI 仍追加项目键。目录设置相对 settings 文件解析；运行项目由会话绑定。

菜单仅修改所选唯一 settings.json，检测外部变更并拒绝覆盖。子代理/网页抓取保存后需新建会话、切到另一会话再返回，或重启；直接选择当前同一会话不会重新装配。技能/Hook 来源更改需重启。策略只保存一份项目绑定，其他项目使用默认策略；重新设置会替换该绑定。完整字段见 [CLI 文档](internal/cli/README.md)。

## 命令与交互

```powershell
codeagent run "任务" --model PROVIDER/ALIAS --data-dir .\data-v6 --json
codeagent run --stdin --timeout 30s --data-dir .\eval-data
codeagent chat --session SESSION_ID --data-dir .\data-v6
codeagent sessions list --data-dir .\data-v6 --json
codeagent sessions show SESSION_ID --data-dir .\data-v6 --json
```

| 页面/命令 | 当前行为 |
| --- | --- |
| /help、/status、/settings、/config | 页面导航、状态及配置来源；没有记忆浏览菜单 |
| /model、/new、/sessions、/session ID、/open PATH | 搜索选模、新会话表单、会话管理与项目/会话切换 |
| /permission、/plan [内容]、/build [内容] | 当前项目策略；模式保存成功后才提交可选正文，忙碌时拒绝 |
| /tools | 仅展示 MCP 服务器和工具；refresh/reconnect 复用现有配置，不重载 TOML；内置实际工具未删除 |
| /skills、/hooks | 加载状态、诊断和来源 add/edit/remove；移除配置不删除源文件 |
| /tasks | 上方子代理/网页抓取两个独立开关，下方任务结果与取消；网页抓取不是子任务 |
| /cancel、/exit | 请求取消，安全等待收尾或退出 |

会话支持搜索、重命名、归档/恢复、历史与导出；导出拒绝覆盖已有文件，归档历史须先取消归档。`/mcp`、`/memory` 和已有直接斜杠命令仍兼容，管理语法见 CLI 文档。

| 按键 | 操作 |
| --- | --- |
| Enter / Ctrl+S；Alt+Enter / Ctrl+J；Alt+S | 发送；换行；活动任务补充输入 |
| F1；F2/F3；F4；F5/F6/F7 | 详情/错误历史；会话/模型；工具展开；新会话/设置/MCP |
| 菜单方向键、Enter、Tab、Esc | 选择、确认、字段/筛选切换、返回上一级 |
| PgUp/PgDn、鼠标滚轮；Ctrl+End | 查看历史；恢复跟随输出 |
| Ctrl+C；Ctrl+Q | 取消任务（空闲退出）；安全退出 |

审批支持 Once、SessionTool、Reject，授权隔离于会话且不能覆盖 Deny。非交互需要审批时退出码 3；其他退出码见 CLI 文档。

## 验证与已知限制

发布在假模型、临时数据库下运行当前源码全量 Go 测试并完成 Windows amd64 构建。**这不是五个模块的系统性代码审查，也不是全部真实环境验收。** 不使用付费 API 或真实用户数据库做发布测试。

- MCP 配置新增/编辑/启停及 TOML 写入延期；刷新/重连不是配置热加载。
- 不支持旧库迁移、多项目权限表或子任务跨进程自动续跑。
- Windows 输入光标有真实 Bubble Tea renderer 回归；用户尚未明确完成真实 IME 候选窗验收，不能宣称彻底解决。
- 当前环境缺少 GCC，race 检查未完成；没有因此安装编译器或宣称 race 通过。
- Linux 仅有历史 Runtime 交叉编译证据，未完成 Linux 运行验收；跨进程文件等待环及所有记忆发布/关闭故障组合未完整覆盖。
- 真实 Provider/远端 MCP/终端信号仍有环境验收边界，测试通过不等于产品无缺陷。

分发时保留 [第三方许可证](THIRD_PARTY_LICENSES.txt)。模块接线见 [Runtime 文档](internal/runtime/README.md)、[CLI 文档](internal/cli/README.md) 和 [TUI 文档](internal/tui/README.md)。
