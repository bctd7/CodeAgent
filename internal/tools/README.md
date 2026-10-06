# 工具目录

`tools.New(tools.Config{WorkingDir: dir})` 固定工作目录，默认依次返回 `read`、平台命令工具（Windows 为 `powershell`，其他平台为 `bash`）、`edit`、`write`。相对路径基于固定目录解析，不切换进程工作目录。

默认构造入口为 `read.New`、`shell.New`、`basic.NewEdit`、`basic.NewWrite`。平台命令工具也可显式选择 `shell.NewPowerShell` 或 `shell.NewBash`。

单文件工具直接位于 `basic`：`edit.go`、`write.go`、`find.go`、`grep.go`、`ls.go`。可选入口为 `basic.NewFind`、`basic.NewGrep`、`basic.NewLS`，由调用方显式加入工具集合；find 依赖 fd，grep 依赖 rg。

多文件工具独立成包：`basic/read` 的 `read.go` 负责读取与头部截断，`image.go` 提供尚未接入 read 的图片准备辅助能力；read 仍按原方式返回原始图片附件。`basic/shell` 包含 `shell.go` 和两个带原 build tags 的平台进程文件，尾部截断及 UTF-8 边界处理留在 shell.go。

多工具共享的路径与输出辅助函数仅在 `basic/common`；专用截断函数留在所属工具文件。

扩展工具独立、不默认注册、不依赖 basic/common：`extend/webfetch.New` 构造 `webfetch`，`NewFetcher` 构造抓取器；实现位于 `webfetch.go`、`text.go`、`transport.go`。`extend/subagent.New(controller)` 返回 `spawn_agent`、`send_input`、`wait`、`cancel_agent`，仅提供契约和适配，真实子任务执行尚未接入。

工具 schema、名称、参数及执行语义保持原样。顶层只显式装配默认四个工具，不自动扫描、不创建 registry、不增加兼容转发层。