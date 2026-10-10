# CodeAgent 1.0

CodeAgent 是 Go 编写的终端代码代理，提供交互 TUI、连续聊天、单次任务和会话查询。Runtime 负责连接现有 Agent、Session、权限、Hook、压缩、记忆和 MCP；CLI/TUI 负责装配与展示。

## 构建与启动

需要 Go 1.26 或以上。项目目录中运行：

```powershell
go test -mod=readonly ./...
go build -mod=readonly -o codeagent.exe .
.\codeagent.exe version
.\codeagent.exe help
.\codeagent.exe
```

安装版使用同级 `应用/codeagent.exe`，从需要处理的项目目录执行 `codeagent` 即进入 TUI。模型调用需要配置和网络连接；`help`、`version` 不调用模型。

## 唯一模型配置

默认只读取 EXE 同目录的 `config.toml`，不搜索或合并当前项目的 TOML。`--config FILE` 可显式选择另一份配置。私有配置不纳入 Git；安装更新不覆盖已有配置。

模型使用 `[providers.PROVIDER.models.ALIAS]`，REF 是 `PROVIDER/ALIAS`，远端模型名填写 `id`，界面名称填写 `display_name`。Provider 连接字段为 `base_url`、`api_key`、`api`；协议值是 `openai-completions` 或 `anthropic`。模型窗口与输出上限由 `context_window`、`max_tokens` 提供。

新会话按 `--model`、显式 settings 的 `default_model`、配置内排序后的首个 REF 选择；失效的 settings 默认值回退，显式或已保存的未知 REF 报错。凭证由用户填写，程序不重写或打印。配置更改后重启。

`--settings FILE` 指定可信宿主 JSON，可配置 Hook、技能、权限、长期记忆及子代理。能力未配置时保持关闭。设置及完整协议见 [CLI 说明](internal/cli/README.md)。

## 常用命令与按键

```powershell
codeagent run "任务" --model PROVIDER/ALIAS --json
codeagent run --stdin --timeout 30s --data-dir .\eval-data
codeagent chat --session SESSION_ID
codeagent sessions list --json
codeagent sessions show SESSION_ID --json
```

会话命令包括 `/help`、`/status`、`/config`、`/model [REF]`、`/open PATH`、`/new`、`/sessions`、`/session ID`、`/cancel`、`/exit`。

| 按键 | 操作 |
| --- | --- |
| Enter / Ctrl+S | 发送 |
| Alt+Enter / Ctrl+J | 换行 |
| Alt+S | 向正在运行的任务补充输入 |
| F1 | 详情及本次 UI 的错误历史 |
| F2 / F3 | 会话 / 模型选择 |
| F4 | 展开工具参数、输出和 Diff |
| PgUp / PgDn、鼠标滚轮 | 查看历史 |
| Ctrl+End | 恢复跟随输出 |
| Ctrl+C | 取消任务；空闲时退出 |
| Ctrl+Q | 安全退出 |

审批独立显示，支持一次授权、会话工具授权和拒绝。非交互任务遇到需要审批时退出码为 3；其他退出码见 CLI 说明。普通错误提示约 3 秒后消失，F1 历史保留至本次 UI 结束。

## 数据与限制

默认数据库在系统用户配置目录的 `CodeAgent/sessions.sqlite`（Windows 通常为 `%APPDATA%/CodeAgent/`）。评测使用 `--data-dir` 隔离数据。当前数据库结构不自动迁移旧版本；模型和工具失败不会自动重放。

IME 修复经过真实 Bubble Tea renderer 的光标定位测试；仍需用户在 Windows 终端手动验证输入法组合窗口。Linux 已有 Runtime 交叉编译证据，未做 Linux 运行验收。真实 Provider、付费调用和用户数据库未用于发布测试。跨进程文件等待环、子任务跨进程自动续跑及所有记忆发布故障组合未覆盖。

源码保留后续接线与扩展接口，零调用不等于无用。旧 demo 验收入口已由正式 CLI 替代；有效 Hook 示例及 TUI 假数据演示保留。

分发 EXE 时随附 [第三方许可证](THIRD_PARTY_LICENSES.txt)。更多边界见 [TUI 说明](internal/tui/README.md) 和 [Runtime 说明](internal/runtime/README.md)。
