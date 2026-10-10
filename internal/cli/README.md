# CLI

`Main(args)` 提供信号、标准 IO 和默认宿主设置；`Execute(ctx,args,IO,Options)` 是可注入入口。main.go 在资源关闭后返回退出码。CLI 装配独立包、命令与资源，TUI 只展示公开投影和提交意图，Runtime 负责流程交接。完整架构见 [根 README](../../README.md)。

**CLI、TUI、Runtime、Tools、Permission 尚未完成系统性代码审查。当前回归/构建通过不等于审查通过。** 本次发布只进行差异、安全、测试和构建核验，不表示已重构或全面审查这五个模块。

## 进程命令

```text
codeagent [execution flags]                  交互 TUI
codeagent run "任务" [--stdin] [--timeout 30s] [--format text|json]
codeagent chat [--stdin] [--json]
codeagent sessions list [--limit 50] [--offset 0] [--archive-filter active|archived|all]
codeagent sessions show ID
codeagent help | version
```

通用：`--config FILE --settings FILE --data-dir DIR --json`。执行：`--model provider/alias --directory DIR --session ID`，可重复 `--skills DIR --hooks FILE`，`--no-memory` 仅适用于新建。flags 可放在位置参数前后，`--` 后按原文处理。管道/--stdin/--json 保持 headless 协议，JSON chat 每行一个值，诊断写 stderr。查询会话不启动模型。

退出码：0 成功、1 失败、2 参数错误、3 需要审批、4 deferred、124 超时、130 取消。无交互任务不伪造授权；交互支持 once/session/reject。关闭取消并安全等待后台任务，不承诺记忆整理完成。

## 唯一配置与数据

默认模型配置仅来自 EXE 同目录 config.toml；显式 --config 或嵌入 Options.ConfigPath 选择另一份，绝不合并项目 TOML。Provider 的 api 为 `openai-completions` 或 `anthropic`；REF 是 `[providers.PROVIDER.models.ALIAS]` 对应的 PROVIDER/ALIAS，不是远端模型 id。CLI 不打印、保存或重写模型凭据/Headers。

新会话选模：--model → settings.default_model → 排序首个已配置 REF。恢复：--model → 保存 REF。失效默认值回退；显式或保存模型不存在时报错。模型目录在启动时加载，更换 TOML 后重启。

默认运行设置仅来自 EXE 同目录 settings.json；--settings 选择唯一文件。缺失默认文件使用内置值，存在但无效或包含未知字段时报错。settings.example.json 不作为配置入口。保存比较已加载字节，检测外部修改后拒绝覆盖，使用同目录临时文件提交。

支持顶层字段：default_model、instructions、skills、hook_files、policy、compact、memory_directory、memory_scope、memory_limits、subagents、web_fetch。subagents/web_fetch 缺省关闭；网页抓取开启使用既有默认预算。instructions 提供 FileName/GlobalDir/TargetDir/MaxBytes；skills 是 Path/Name 对象；policy 提供 WorkMode/ApprovalMode/PlanDir/DeniedTools（Source/SourceID/ToolID），WorkingDir 绑定一个项目；compact 提供 ReserveTokens/KeepRecentTokens；memory_limits 使用现有 Execution/Storage 字段。

settings 路径相对所选文件目录解析，TargetDir 相对项目；命令行技能/Hook 来源相对项目追加。显式 memory_directory/memory_scope 成对提供，CLI 作为基址/前缀继续追加规范项目键，避免跨项目混用记忆。新会话默认启用长期记忆；会话开关持久化，恢复不覆盖。

当前 Session schema=6，schema=5 明确拒绝；不迁移、删除、重建或回填旧库。默认数据仍在 UserConfigDir/CodeAgent/sessions.sqlite；新版首次启动显式选新目录：

```powershell
& 'C:\Apps\CodeAgent\codeagent.exe' --data-dir 'C:\Apps\CodeAgent\data-v6'
```

原旧库保留。run/chat 不允许将 child session 当主会话打开；sessions show 可查询子会话。恢复使用保存项目目录；目录不一致报错，/open 新建另一项目会话。

## TUI 页面和兼容命令

| 命令 | 页面/直接语法 |
| --- | --- |
| /help /status /settings /config | 导航、模型/项目/状态、有效配置来源和初始化时加载的指令诊断 |
| /model [REF] | F3 搜索模型名称/平台，详情显示 ID/REF；切换使用 REF |
| /new [JSON] | F5 项目输入、模型列表、长期记忆开关；headless `/new {"project":"DIR","model":"REF","memory_enabled":false}` |
| /sessions | F2 搜索，Tab 全部/活动/归档，Enter 打开子菜单 |
| /sessions search TEXT\|all\|archived | 查询列表；子会话过滤后分页 |
| /sessions rename ID TITLE | 重命名 |
| /sessions archive ID / unarchive ID | 当前会话须先切走才归档；历史须取消归档后查看 |
| /sessions history ID / export ID FILE | 只读历史/独占新文件导出，拒绝覆盖，导出遍历所有 Run 页 |
| /session ID /open PATH | 恢复或切换项目；活动工作拒绝切换 |
| /permission [default\|auto_edit\|auto_approve] | 保存当前项目唯一策略；清临时授权，不扩大范围 |
| /plan [内容] /build [内容] | 模式保存成功后才提交可选正文；忙碌时拒绝且不发送 |
| /tools [refresh\|reconnect SERVER_ID] | F7 只展示 MCP 服务器/工具；参数在详情，不删除模型实际内置工具 |
| /skills /hooks | 当前加载状态、诊断；`add PATH`、`edit INDEX PATH`、`remove INDEX`，索引从 1 开始 |
| /tasks | 上方能力设置：子代理/网页抓取两个独立开关；下方真实子任务记录 |
| /tasks result ID /cancel ID | 结果查询/请求取消；只读面板不提交模型输入、不消费交付 |
| /tasks enable\|disable | 子代理开关；也支持 `/tasks subagents enable\|disable` |
| /tasks web_fetch enable\|disable | 网页抓取开关，不创建子任务记录 |
| /cancel /exit | 取消当前主任务/安全退出 |

`/mcp` 保留状态/refresh/reconnect 兼容入口，复用当前 Manager 配置，不重载 TOML。`/memory` 保留只读进度命令，帮助菜单无记忆浏览入口。归档 Session 的公开历史入口仍需先取消归档。

菜单方向键选择，Enter 打开/确认，Tab 字段或过滤，Esc 返回父层，PgUp/PgDn 长详情；新建表单 Space 切换长期记忆，模型从列表选。异步操作禁止重复提交，失败保留输入/位置。路径可含空格，来源移除只修改配置，不删除文件。

Enter/Ctrl+S 发送，Alt+Enter/Ctrl+J 换行，Alt+S 补充输入；F1 详情/本次错误历史，F4 工具展开，F6 设置；Ctrl+C 取消（空闲退出），Ctrl+Q 安全退出。错误短提示会消退，重要通知/审批保持可见。真实 Windows IME 候选窗尚未由用户明确验收，renderer 测试不能替代该验收。

## 生效与业务边界

子代理/网页抓取保存后，在新建、切至另一会话再返回，或重启后的装配生效；直接选当前同一会话不热切换。技能/Hook 来源更改需重启。权限只记一个项目绑定，其他项目用默认策略，再保存会替换绑定。模型/MCP TOML 配置写入、MCP 增删/编辑/启停延期。

Permission 的 Orchestration 是内部任务调度类别，Build 下放行内置调度；Plan、显式禁用、External 与 Hook Ask 仍有原边界。子工具实际权限独立。子 Runtime 默认不自动装 MCP、默认无 shell、禁止孙任务。MCPShared 的刷新/重连由外部拥有者协调，Runtime 只能应用已有目录。

父模型仅接收子任务公开正文/状态/追溯 ID，不接收完整 ContentBlock/thinking/签名。去重按具体 child Run，依据持久化最终工具投影及 child_run 来源；pending、空替换或不完整输出不提前认作收到。同 child 新 Run 独立交付。CLI 以可信 Entry.Kind/SourceKind 展示宿主反馈，不把协议 UserMessage 形状当作真实用户发言；旧记录只投影，不改写。

原始工具结果和原始子会话历史保留用于审计；headless 结构化历史查询与导出仍是审计接口，普通任务面板只展示公开结果。CLI/TUI 不复制 Session、权限、模型循环或长期记忆算法。

## 验证状态

当前假模型/临时数据库正式回归及发布的全量 `go test -mod=readonly ./... -count=1 -timeout=60s` 与 Windows 构建用于验证具体行为，不等于五模块系统性审查通过。无付费 API、真实 Provider/远端 MCP 或用户数据库发布验收。缺少 GCC，race 未完成；Linux 只有历史 Runtime 交叉编译证据，没有运行验收。跨进程文件等待环、后台真实记忆发布的所有关闭/故障时点仍未完整覆盖。
