# Session 架构整理与收尾验证

日期：2026-10-01。范围仅为 Session 包与本次文档；未接入 Runtime，未修改 Agent、LLM、Provider、工具、demo 或实际配置，未提交或推送 Git。

## 当前结构与边界

```text
internal/session/
  session.go          Service 会话管理、归档、目录绑定、显式中断
  runs.go             Session 的 Run、普通消息与用量
  history.go          当前分支历史
  tools.go            工具调用配对和结果保存编排
  config/
    types.go          共享记录、查询参数、状态和错误
    codec.go          消息 JSON 编解码
  store/
    store.go          连接、Open／Close、事务
    init.go           初始化、旧库迁移、数据库约束检查
    sessions.go       会话元数据读写
    runs.go           Run 记录读写
    entries_tools.go        Entry 与 ToolExecution 读写
  sql/                集中 SQL 常量
```

依赖为 Session → Store → SQL；Session 与 Store 都引用 config。config 只依赖 LLM 等共享基础类型，不依赖 Session、Store 或 TOML 配置包。没有类型别名层、存储后端接口或数据库公开句柄。

Store 的五个正式文件只负责数据库与记录恢复：执行语句、事务、参数绑定、扫描、调用外部 codec 恢复消息、初始化与数据迁移。迁移中的消息检查用于修复旧库关联，不决定模型或工具执行。业务层不直接执行 SQL，业务判断与修改使用同一个 Store 事务。

## 接口位置变化

- 数据库入口改为 `store.Open(ctx, path)`；显式调用 `db.Init(ctx)`，再调用 `session.NewService(db)`。
- 事务类型为 `store.Tx`，不公开内部 `sql.Tx`。
- 持久化记录、查询参数、状态和可判定错误位于 `session/config`，JSON 标签和枚举值保持原定义。
- 编解码入口为 `config.EncodeMessage`、`config.DecodeMessage`。
- `ListUnfinishedRuns`、`MarkRunAborted` 的业务入口在 Service。Store 提供查询与终态写入原语，不判断是否应中断任务。
- 当前无其他业务包的 Session 装配调用需要迁移；未来 Runtime 使用上述入口。

## 本次修复

- Rename、BindWorkingDir 不再绕过 Store 操作私有 SQL 事务。
- SetTitleIfEmpty 在同一事务检查归档状态，归档后不修改标题。
- 归档冲突、幂等处理、首次目录绑定和显式中断判断移到 Service；工具配对留在 Session。
- StartRun 校验非空 Head 的会话归属；SwitchBranch 校验 LastEntry 的会话及 Run 归属。追加消息与工具结果校验当前 Head 与活动 Run 的末尾一致。
- GetRun 拒绝缺失或不属于该 Run 的输入、末尾记录，以及缺少来源的 assistant，避免返回不完整详情或漏算用量。
- 旧库升级及当前版本 Init 补齐普通索引；检查外键、Sequence 唯一约束、索引列及工具结果唯一索引条件。同名错误索引不被 IF NOT EXISTS 掩盖。
- 迁移失败在同一事务回滚，不重建数据库，不提高当前版本 2，不修改旧 usage_json 数据。
- 所有列表 SQL 集中到 SQL 子包；Entry 的单条与列表扫描共用实现，删除废弃 SQL。

## 实际离线验证

在 Windows 上使用临时 SQLite 数据库和临时 Go 测试文件实际运行 8 组验证，包含以下场景。临时文件在完成后删除，没有新增永久测试文件。

| 场景 | 实际结果 |
|---|---|
| 新库、重复 Init、关闭重开 | 通过 |
| 中文、空格、`#`、`&`、`%` 数据库文件名；外键、5000ms busy timeout、单连接设置 | 通过 |
| 版本 0／1 四表旧库升级，工具结果关联回填，保留 usage_json，补齐四个索引 | 通过 |
| 迁移歧义、坏 JSON 回滚，版本和新列不留下部分修改 | 通过 |
| 高版本、未知表、缺表、缺外键、缺 Sequence 唯一约束、错误同名索引 | 正确拒绝 |
| 空标题条件补全、已有标题不覆盖、归档标题禁止修改、归档与取消归档重复操作不刷新时间 | 通过 |
| 活动 Run 归档拒绝、归档可读不可打开或开始 Run、三态列表、字面 `%`／`_` 搜索 | 通过 |
| 首次目录绑定、重复绑定拒绝、空白目录拒绝 | 通过 |
| user／assistant／tool 消息、思考签名、redacted 数据、图片 base64、大整数 json.Number 往返 | 通过 |
| 同名模型在不同 Provider 下分组计费，工具用量不重复计入，Reasoning 不再次加到 Total | 通过 |
| 失败 assistant 原始记录保留；通用消息入口拒绝工具结果 | 通过 |
| 工具启动与结果、重复启动／结果拒绝、未知调用拒绝、无启动的未执行结果、有启动无结果 | 通过 |
| 同会话两个并发 StartRun 仅一个成功；不同会话可分别开始 Run | 通过 |
| 12 个工具结果并发追加不丢失，Sequence 连续唯一 | 通过；并发场景另重复 10 次 |
| 显式中断保存原始消息，重复中断／结束拒绝覆盖 | 通过 |
| A→B，切回 A 开始 C；Run 列表包含所有分支，当前历史不包含 B | 通过 |
| 非当前分支的坏 JSON 不影响当前历史，读取对应坏 Run 会报错 | 通过 |
| 跨会话 Head、跨会话 Run 末尾、历史环及断链 | 正确拒绝 |
| 写入后注入错误，开始 Run 的 Entry 插入失败 | 事务回滚；标题不变、不留下孤立 Run |
| 未知角色、未知消息字段、尾随 JSON、非法 UTF-8、无法编码的工具参数 | 正确报错 |

实际命令：

```text
go test ./internal/session/... -run TestReview -v -count=1 -timeout=60s
go test ./internal/session -run TestReviewConcurrencyAndRecovery -count=10 -timeout=60s
gofmt -w internal/session
go build ./...
go vet ./...
```

上述行为验证实际通过；最终删除临时文件后重新执行全项目 build、vet 和格式检查。

## 尚未验证及交付限制

- 未执行 race 检查：当前 CGO_ENABLED=0，PATH 未找到 gcc／clang。并发用例通过不等于通过竞态检测。
- 本次只在 Windows 实际运行；未验证 Linux/macOS 特殊路径。
- 未调用真实模型 API，也未验证 Runtime／Agent 接入或真实进程崩溃时的恢复编排。
- 未模拟断电、磁盘满、真实系统 I/O 故障；事务回滚验证使用明确注入的失败。
- 本轮没有重跑前次报告的 10002 条历史链压力场景。
- 当前列表按 SQLite 毫秒时间与稳定 ID 排序，不声称纳秒排序精度。

本报告是本轮修复与验证记录，供后续更强模型独立复核，不宣称全部存储语义或所有异常场景均已覆盖。后续复核应先读当前代码及本报告，再独立生成临时验证，不把报告中的“通过”代替自己的运行结论。
