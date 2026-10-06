# 按 Run 撤销显式文件记录

独立模块，尚未接入 Runtime、Session 或 edit/write。仅接收可信调用方提供的成功写入结果，不自行推断工具执行、文件归属或权限授权，不处理 shell/MCP 效果，不移动 Head 或修改历史。

## API 与调用约定

```go
store, err := filechange.Open(ctx, journalDirectory, filechange.Run{
    SessionID: sessionID,
    RunID: runID,
    ProjectRoot: projectRoot,
})
// 检查 err，使用结束后调用 store.Close()。

err = store.Record(ctx, filechange.Change{
    Path: relativeOrAbsolutePath,
    Before: beforeSnapshot,
    After: afterSnapshot,
})
plan, err := store.Plan(ctx)
result, err := store.Undo(ctx)
```

- `Snapshot{Exists, Data, Mode}` 区分不存在和空文件。After 必须存在；仅支持覆盖或新建普通文件。相同路径保留第一次 Before，更新最终 After；上次 After 与下次 Before 的存在状态、内容摘要和 mode 必须连续。
- 调用方在写入前取得 Before，成功写入后立即 Record，并协调并行写入顺序。Record 还会核对当前文件内容、mode 和普通文件身份。工具写入与 Record 之间存在窗口；Record 失败不能当作已获得撤销保障，也不会自动撤销那次工具写入。
- `Open` 创建或重开一个调用方明确指定的独立日志目录，父目录须存在。一个目录绑定一个 Session/Run/项目身份；日志与项目不得互相包含，路径和祖先目录身份都参与校验。目录不能存无关文件，没有默认全局目录或第二套会话数据库。
- 固定 `lock` 文件上的系统锁保证同一目录只有一个打开者；Close 或进程退出释放锁，不通过删除锁文件接管。Store 内串行化操作，操作等待支持 context 取消；Close 等待正在进行的操作结束。
- `Plan` 只读，返回恢复旧文件或删除新建文件的 Actions、已完成文件及 Conflicts。Undo 重新整批预检，不接收缓存计划。初始已知冲突会整批拒绝，不写目标文件或日志。
- 首次 Undo 通过预检后封存 Run，Record 随后返回 `ErrSealed`。撤销按记录顺序逐文件进行，不删除新建文件的父目录。全部完成后重复 Undo 返回 `AlreadyUndone=true`，不触碰后来对这些文件的新修改。

## 持久化与部分失败

`run.json` 保存格式版本、Session/Run/项目绑定和目录身份、每文件最初 Before bytes/mode、最终 After 摘要/mode/文件身份及撤销进度。不保存模型消息、凭据或其他项目文件。

每文件状态为 `pending → prepared → done`。恢复旧文件时，先在目标同目录写完整临时文件、设置权限并 Sync，将临时文件名和身份持久化为 prepared，再替换目标。新建文件撤销使用只能删除文件的系统调用。日志同样先写完整同目录临时文件并 Sync，再替换正式日志。

若文件已恢复而完成日志失败，重开后只在目标与 Before 的内容/mode 和 prepared 临时文件身份均一致时补记完成。删除的恢复仅以持久化 intent 加目标不存在判断；这不能证明删除者身份。无法确认的状态返回冲突，不凭“内容恰好等于 Before”认定已撤销。

`UndoResult.Completed / Failed / Pending` 分别列出持久化完成、当前失败及未执行文件。`Failed[].Applied=true` 表示文件效果已发生，但后续验证或日志失败，该文件不计入 Completed。中途失败不重写先前完成文件，不自动全回滚；释放阻塞条件后可对相同日志重试。取消在读写块和文件边界检查；已发生的操作会尽量完成日志收尾。

每次 API 调用重新读盘，不缓存旧日志。日志替换后同步失败时，新提交可能已可见，API 仍返回错误；后续操作不会以内存旧状态覆盖它。日志不能解码、版本/作用域/快照校验不符时拒绝操作。准备记录尚未保存就异常退出可能留下 `.filechange-*` 临时文件；不按通配符清扫，以免删除后来出现的其他文件。

## 支持范围与限制

- 通过 Go 1.26 `os.Root` 限制访问范围，并检查父目录、普通文件类型、打开前后身份和摘要。拒绝 symlink、junction、reparse point、设备、目录和多硬链接文件。根目录及其祖先也须是无链接的实际路径。
- 不支持 rename。路径别名被拒绝或核对为同一路径；即使文件内容相同，替换为另一文件身份也会冲突。替换写之后再次 Record 会使用旧路径的当前身份检查别名。
- 只恢复基础权限位，不恢复 ACL、属主、扩展属性或时间戳。Windows mode 表达 readonly，归一为 `0444/0666`。Unix 快照需保留属主读权限，拒绝 `0000/0200` 等重开后无法核验的 mode 和特殊权限位。
- 日志目录在 Unix 收紧为 `0700`，文件为 `0600`。Windows 使用禁止继承的 DACL，允许当前用户及 SYSTEM，由新文件继承。日志须位于可信本地目录，其他程序不能修改日志或替换锁文件。
- Windows 使用原生扩展标志覆盖或删除 readonly 文件，要求系统/文件系统支持相应操作；不支持时返回执行错误和进度，不提前修改目标属性。参考微软的[重命名接口](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_rename_information)和[删除接口](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntddk/ns-ntddk-_file_disposition_information_ex)。本实现未复制外部项目代码。
- 哈希和文件身份只是当前状态条件，不构成作者证明。宿主仍须协调写入；本包不提供全局无 TOCTOU、恶意并发路径替换防护或跨文件原子事务。
- 实现面向 Windows、Linux、macOS。正常进程退出后可复开，不承诺任意断电恢复；Windows 不对目录执行 Sync。实际行为验证在 Windows，Linux/macOS 仅交叉编译。

## 本任务验证记录

Windows 的 15 组临时回归全部通过，涵盖旧文件恢复、新建撤销、连续修改、冲突整批拒绝、中途 I/O 失败清单及重试、取消、日志写失败、复开恢复、进程退出、重复 Undo、路径越界、文件身份替换、只读属性、目录短名隔离、junction/hardlink 拒绝，以及文件替换后通过短名再次 Record 的别名检测。所有项目文件和日志都在 `t.TempDir` 中，没有对真实仓库执行 Undo。独立只读审阅完成，发现的别名和权限边界已修复。

临时回归执行命令为 `go test ./internal/filechange -run TestReview -v -count=1 -timeout=60s`。按任务约定，验证后已删除本任务唯一临时测试文件 `review_windows_test.go`；该命令不再代表保留的回归测试集。

删除临时测试后的最终源码通过：

- `go test -p 2 ./... -run '^$' -count=1 -timeout=90s`（全仓测试编译，不运行业务测试）。
- `go build -p 2 ./...`。
- `go vet -p 2 ./...`。
- `GOOS=linux GOARCH=amd64 go build -p 2 ./internal/filechange`。
- `GOOS=darwin GOARCH=arm64 go build -p 2 ./internal/filechange`。

构建和测试采用本任务独立的临时目录及 Go 缓存，未修改依赖。Linux/macOS 未进行运行时行为验证。
