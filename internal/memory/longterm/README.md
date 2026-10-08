# 长期记忆

本包提供 Markdown 主题存储、索引、字面检索和有界模型整理。只依赖标准库与 `internal/llm`，不导入 memory 父包或 compact，也没有接入 Runtime、Session、Agent 或主对话工具目录。

调用方提供目录、消息批次、显式模型和 Stream 回调，负责何时调用、选择当前历史路径、分配用户/项目目录、主对话注入。不同会话打开同一目录即可共享主题；本包不认识 SessionID，不调度任务，不创建默认 provider，也不读取 API Key 环境变量。

## 不使用模型

```go
s, err := longterm.Open(directory, longterm.Options{})
if err != nil { return err }
defer s.Close()
err = s.Put(ctx, longterm.Entry{
    Path: "orm.md", Summary: "核心链路的 SQL 控制约束", Type: "project",
    Content: "项目不用 ORM，因为需要直接控制 SQL。",
    Sources: []string{"message-1"},
})
if err != nil { return err }
document, err := s.Read(ctx, "orm.md")
if err != nil { return err }
matches, err := s.Search(ctx, "SQL", 10)
if err != nil { return err }
fmt.Println(document.Content, matches)
// document 与 matches 由调用方使用；Forget 删除主题并同步索引。
```

类型只允许 user、feedback、project、reference。主题采用 description、type、sources frontmatter；摘要单行、最多 200 个 Unicode 字符。MEMORY.md 由代码按文件名排序生成，Put 和模型均不能直接写索引。Search 是支持中文的大小写归一字面匹配，返回路径、行号和片段，不是语义检索。

路径限于目录内普通 Markdown 文件，拒绝穿越、绝对路径、斜杠、ADS、设备名、内部文件和索引保留名。使用 os.Root、普通文件与 UTF-8 检查；不沿符号链接访问目录外内容。旧主题正文仍可读取。

## 假模型整理示例

```go
turn := 0
stream := func(ctx context.Context, request llm.Request) (llm.StreamReader, error) {
    reader, writer := llm.NewEventStream(ctx, llm.AssistantMessage{State: llm.StatePending})
    message := llm.AssistantMessage{Role: llm.RoleAssistant,
        State: llm.StateSuccess, StopReason: llm.StopStop}
    if turn == 0 {
        message.StopReason = llm.StopToolUse
        message.Content = []llm.ContentBlock{{Type: llm.TypeToolCall,
            ID: "write-1", Name: "memory_write", Arguments: map[string]any{
                "path": "orm.md", "summary": "SQL 控制约束", "type": "project",
                "content": "项目不用 ORM，因为需要直接控制 SQL。",
                "source_ids": []string{"message-1"},
            }}}
    }
    turn++
    writer.Finish(message)
    return reader, nil
}
s, err := longterm.Open(freshDirectory, longterm.Options{
    Model: llm.Model{Provider: "fake", ID: "offline", API: llm.APIKindOpenAI},
    Stream: stream,
})
if err != nil { return err }
defer s.Close()
outcome, err := s.Consolidate(ctx, longterm.Batch{
    ID: "batch-1", Cursor: "cursor-1",
    Sources: []longterm.Source{{ID: "message-1", Message: &llm.UserMessage{
        Role: llm.RoleUser,
        Content: []llm.ContentBlock{{Type: llm.TypeText,
            Text: "项目不用 ORM，因为我要直接控制 SQL。"}},
    }}},
})
if err != nil { return err } // Usage 不代表已经保存。
fmt.Println(outcome)
```

例外更新流程：第一批保存“不用 ORM，以直接控制 SQL”；第二批输入“后台管理模块允许 ORM，核心链路还是不用”，模型先 memory_read 旧主题，再 memory_write 更新为“核心链路不用 ORM，以直接控制 SQL；后台管理模块允许 ORM”。最终只有一份包含范围、例外和原因的主题，索引同步更新。更新主题时，代码合并本轮实际读取主题的旧来源与本批依据，支持合并主题后保留出处；新主题仍只声明本批来源。模型不能虚构旧来源。来源集合保守保留已读主题的引用，不证明逐句真实性；假模型验证机制也不证明真实模型的语义准确率。

只有整理模型本轮产生的 memory_list/read/search/write/edit/delete 可执行。历史工具调用只是证据，不会执行。现有主题必须读后修改；新建后再次 edit/delete 也须先读当前暂存版本。list/search 不替代 read。edit 只在正文作一次精确替换，未知工具、参数、来源、重复调用 ID 或执行错误会终止整批，丢弃尚未提交的暂存结果。

## 批次、预算与取消

- Batch.ID 在目录内唯一，Sources 非空，来源 ID 非空且批内唯一；拒绝 nil/typed-nil。有效精简转录与 Cursor 决定稳定摘要，不包含旧记忆、时间、鉴权等无关字段。同 ID/同有效输入命中 Duplicate；不同输入或 Cursor 返回 ErrBatchConflict。重复批次返回该批原回执 Cursor，即使目录游标已经推进；去重无需模型。
- Cursor 是不透明标记，Cursor() 返回整个目录最后成功批次的标记，不是某个会话的进度。本包不比较游标或查询历史。无改动整理也登记成功回执，Changed 只表示主题或索引改变。
- 旧 `.memory-state.json` 的批次标记保留；旧批次摘要算法无法与新投影安全比较，因此对应 ID 返回 ErrLegacyBatch，须由外部明确迁移，不重新提炼或清空状态。新回执使用状态版本 2。
- 默认 Turns=5、ToolCalls=24、InputBytes=256000、OutputBytes=256000、FileBytes=64000、Files=256、IndexLines=200、IndexRunes=25000；零值补默认，负值拒绝。它们是资源预算，不是 token 估算。MaxTokens=0 默认 4096，并受正值 Model.MaxTokens 限制；负值拒绝。
- 每次调用检查系统提示、实际消息及工具定义的 JSON 字节数，不计传输鉴权。输出预算累计最终 Content JSON 和工具结果 JSON；delta 与每份快照分别检查增长，不重复累计快照。检查终态与工具调用一致性、文件、索引、状态和日志预算，不自动分块、重试或截掉来源。
- 初始输入只有精简来源、索引和清单，正文按需读取。不传来源的 Details、Thinking、签名、redacted 数据或图片 base64；图片只标记未经视觉提炼。可见文本和工具参数仍可能包含秘密，本包不能保证识别任意凭据。传输选项只在内存，不能作为记忆内容或日志输出。
- 输入、嵌套参数、选项及流快照共享只读；Go 不冻结对象，也不保证并发修改安全。只值复制需要改输出上限的 RequestOptions。后续请求列表重新构造，流快照不原地修改。
- Stream 须响应 context，reader.Close 须解除 Result 阻塞，Next 须遵守自身 context 契约。取消等待锁、建流、读流或终态时正确退出。模型阶段失败或取消不会发布提交，失败 Outcome 的 Changed/Cursor 无成功含义，但 Usage 可记录消耗。StateSuccess+StopStop+无调用可以空文本正常结束；length、未知/失败终态及调用不一致均失败。

## 提交与恢复

Put、Forget 与 Consolidate 使用同一有界提交日志 `.memory-pending.json`。锁内先校验全部目标主题、最终索引、批次状态和实际序列化字节数；状态固定上限 8 MiB，日志固定上限 32 MiB，自定义文件预算不会绕过它们。

1. 临时文件写入、Sync、Rename 发布 pending；发布前失败或取消不改变主题、索引、成功状态。
2. 发布日志是不可撤销的恢复点。从此不按普通取消打断提交，按排序写主题、索引、删除旧主题、写状态，最后移除日志。
3. 发布后 I/O 失败返回可判断的 ErrCommitPending，并保留日志。此时磁盘可能部分改变，不能声称已经回滚。日志移除失败也属于待恢复。
4. Open 和每次后续数据操作在共享锁内先重放 pending；恢复成功前不读部分结果、不启动新模型或覆盖日志。重放不调用模型，删除不存在文件可幂等完成，恢复后的同批次命中去重。
5. 恢复检查版本、路径、大小、目标内容、索引一致性与状态结构。损坏日志保留并明确失败，不静默忽略。重开使用的限制须能容纳已发布目标，否则明确失败，须调整限制后恢复。

同目录的实例在本进程内串行；从读取旧记忆到模型整理与提交一直持锁。模型回调不得重入同目录 Service，避免死锁。Close 不与操作并发，也不会撤销其他实例的锁。跨进程写入、外部编辑和外部直接读取由调用方协调；外部读取没有跨文件瞬时原子视图。这里只承诺已发布日志的 I/O 失败/进程中断重放，不保证所有平台突然断电或磁盘损坏后的事务安全。

离线验收用临时目录、假 Stream 和仓库外 Go overlay；故障注入仅在 overlay 中插入于本地 I/O 边界，生产包没有公开文件系统替身或故障接口。未调用真实模型，也未读取用户记忆。
