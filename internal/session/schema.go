package session

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	"CodeAgent/internal/llm"
)

//go:embed sql/schema.sql
var schemaSQL string

// CurrentSchemaVersion 是当前支持的表结构和消息格式版本。
const CurrentSchemaVersion = 2

type tableColumn struct {
	name    string
	notnull bool
	pk      int
}

func getTableColumns(ctx context.Context, tx *sql.Tx, tableName string) (map[string]tableColumn, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return nil, fmt.Errorf("查询表 %s 结构: %w", tableName, err)
	}
	defer rows.Close()

	cols := make(map[string]tableColumn)
	for rows.Next() {
		var (
			cid     int
			name    string
			colType string
			notnull bool
			dfltVal sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &colType, &notnull, &dfltVal, &pk); err != nil {
			return nil, fmt.Errorf("读取表 %s 列信息: %w", tableName, err)
		}
		cols[name] = tableColumn{name: name, notnull: notnull, pk: pk}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历表 %s 列信息: %w", tableName, err)
	}
	return cols, nil
}

// initSchema 检查数据库版本并初始化或迁移表结构。
func (s *Store) initSchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启数据库初始化事务: %w", err)
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("读取数据库版本: %w", err)
	}

	if version > CurrentSchemaVersion {
		return fmt.Errorf("数据库版本 %d 高于当前程序支持的版本 %d", version, CurrentSchemaVersion)
	}

	// 检查已有数据表
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return fmt.Errorf("检查已有数据表: %w", err)
	}
	defer rows.Close()

	existingTables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("读取表名: %w", err)
		}
		existingTables[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历表名失败: %w", err)
	}

	// 空库情况
	if len(existingTables) == 0 {
		if version != 0 {
			return fmt.Errorf("不支持的数据库结构: 数据库为空但版本为 %d", version)
		}
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("执行建表与索引语句: %w", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", CurrentSchemaVersion)); err != nil {
			return fmt.Errorf("设置数据库版本号: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交数据库初始化事务: %w", err)
		}
		return nil
	}

	// 非空库：严格校验表和已知结构
	knownTables := map[string]bool{
		"sessions":        true,
		"runs":            true,
		"entries":         true,
		"tool_executions": true,
	}
	for name := range existingTables {
		if !knownTables[name] {
			return fmt.Errorf("不支持的数据库结构: 发现未知的表 %q", name)
		}
	}
	for reqTable := range knownTables {
		if !existingTables[reqTable] {
			return fmt.Errorf("不支持的数据库结构: 缺少数据表 %q", reqTable)
		}
	}

	// 校验各表的关键列和主键
	sessionsCols, err := getTableColumns(ctx, tx, "sessions")
	if err != nil {
		return err
	}
	for _, req := range []string{"id", "title", "working_dir", "created_at", "updated_at", "archived_at", "head_id"} {
		if _, ok := sessionsCols[req]; !ok {
			return fmt.Errorf("不支持的数据库结构: sessions 表缺少列 %q", req)
		}
	}
	if sessionsCols["id"].pk != 1 {
		return fmt.Errorf("不支持的数据库结构: sessions 表主键不符")
	}

	runsCols, err := getTableColumns(ctx, tx, "runs")
	if err != nil {
		return err
	}
	for _, req := range []string{"id", "session_id", "start_head_id", "input_entry_id", "last_entry_id", "started_at", "finished_at", "state", "steps", "stop_reason", "raw_stop_reason", "error_message"} {
		if _, ok := runsCols[req]; !ok {
			return fmt.Errorf("不支持的数据库结构: runs 表缺少列 %q", req)
		}
	}
	if runsCols["id"].pk != 1 {
		return fmt.Errorf("不支持的数据库结构: runs 表主键不符")
	}

	entriesCols, err := getTableColumns(ctx, tx, "entries")
	if err != nil {
		return err
	}
	for _, req := range []string{"id", "session_id", "parent_id", "sequence", "run_id", "created_at", "message_json", "origin_json"} {
		if _, ok := entriesCols[req]; !ok {
			return fmt.Errorf("不支持的数据库结构: entries 表缺少列 %q", req)
		}
	}
	if entriesCols["id"].pk != 1 {
		return fmt.Errorf("不支持的数据库结构: entries 表主键不符")
	}

	toolExecCols, err := getTableColumns(ctx, tx, "tool_executions")
	if err != nil {
		return err
	}
	for _, req := range []string{"session_id", "run_id", "assistant_entry_id", "tool_call_id", "started_at", "finished_at", "result_entry_id"} {
		if _, ok := toolExecCols[req]; !ok {
			return fmt.Errorf("不支持的数据库结构: tool_executions 表缺少列 %q", req)
		}
	}
	if toolExecCols["assistant_entry_id"].pk == 0 || toolExecCols["tool_call_id"].pk == 0 {
		return fmt.Errorf("不支持的数据库结构: tool_executions 表主键不符")
	}

	if version == CurrentSchemaVersion {
		// 校验当前版本必需的关联字段
		if _, ok := entriesCols["assistant_entry_id"]; !ok {
			return fmt.Errorf("不支持的数据库结构: 版本为 %d 但 entries 缺少 assistant_entry_id", CurrentSchemaVersion)
		}
		if _, ok := entriesCols["tool_call_id"]; !ok {
			return fmt.Errorf("不支持的数据库结构: 版本为 %d 但 entries 缺少 tool_call_id", CurrentSchemaVersion)
		}
		return nil
	}

	// 执行从旧版本（version < CurrentSchemaVersion）向 v2 的迁移
	if _, ok := entriesCols["assistant_entry_id"]; !ok {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE entries ADD COLUMN assistant_entry_id TEXT REFERENCES entries(id)"); err != nil {
			return fmt.Errorf("添加 entries.assistant_entry_id 列失败: %w", err)
		}
	}
	if _, ok := entriesCols["tool_call_id"]; !ok {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE entries ADD COLUMN tool_call_id TEXT"); err != nil {
			return fmt.Errorf("添加 entries.tool_call_id 列失败: %w", err)
		}
	}

	// 回填已有工具结果的关联字段
	toolEntryRows, err := tx.QueryContext(ctx, "SELECT id, session_id, run_id, message_json FROM entries WHERE assistant_entry_id IS NULL")
	if err != nil {
		return fmt.Errorf("查询待回填的 entries: %w", err)
	}
	defer toolEntryRows.Close()

	type pendingEntry struct {
		id          string
		sessionID   string
		runID       string
		messageJSON string
	}
	var pending []pendingEntry
	for toolEntryRows.Next() {
		var pe pendingEntry
		if err := toolEntryRows.Scan(&pe.id, &pe.sessionID, &pe.runID, &pe.messageJSON); err != nil {
			return fmt.Errorf("读取待回填 entry 行: %w", err)
		}
		pending = append(pending, pe)
	}
	if err := toolEntryRows.Err(); err != nil {
		return fmt.Errorf("遍历待回填 entries: %w", err)
	}
	toolEntryRows.Close()

	assignedPairs := make(map[string]string)
	for _, pe := range pending {
		msg, err := decodeMessage([]byte(pe.messageJSON))
		if err != nil {
			return fmt.Errorf("解析 entry %s 消息失败: %w", pe.id, err)
		}
		toolMsg, ok := msg.(*llm.ToolResultMessage)
		if !ok {
			continue
		}
		if toolMsg.ToolCallID == "" {
			return fmt.Errorf("工具结果 entry %s 的 tool_call_id 为空，无法回填", pe.id)
		}
		toolCallID := toolMsg.ToolCallID

		var candidateAsstID string
		// 先按结果 ID 查找，不能把调用 ID 冲突或重复关联当成无启动记录。
		execRows, err := tx.QueryContext(ctx, "SELECT session_id, run_id, assistant_entry_id, tool_call_id FROM tool_executions WHERE result_entry_id = ? LIMIT 2", pe.id)
		if err != nil {
			return fmt.Errorf("查询工具执行记录失败: %w", err)
		}
		var execSessionID, execRunID, execAsstID, execToolCallID string
		execCount := 0
		for execRows.Next() {
			execCount++
			if execCount > 1 {
				execRows.Close()
				return fmt.Errorf("工具结果 entry %s 被多条执行记录引用，无法唯一确定", pe.id)
			}
			if err := execRows.Scan(&execSessionID, &execRunID, &execAsstID, &execToolCallID); err != nil {
				execRows.Close()
				return fmt.Errorf("读取工具执行记录失败: %w", err)
			}
		}
		if err := execRows.Err(); err != nil {
			execRows.Close()
			return fmt.Errorf("遍历工具执行记录失败: %w", err)
		}
		execRows.Close()

		if execCount == 1 {
			if execToolCallID != toolCallID {
				return fmt.Errorf("工具结果 entry %s 的调用 ID %q 与执行记录 %q 不一致", pe.id, toolCallID, execToolCallID)
			}
			if execSessionID != pe.sessionID || execRunID != pe.runID {
				return fmt.Errorf("工具结果 entry %s 与执行记录不属于同一会话和 Run", pe.id)
			}
			candidateAsstID = execAsstID
			// assistant 必须与执行记录和工具结果处于同一 Run。
			var asstMsgJSON string
			err = tx.QueryRowContext(ctx, "SELECT message_json FROM entries WHERE id = ? AND session_id = ? AND run_id = ?", candidateAsstID, pe.sessionID, pe.runID).Scan(&asstMsgJSON)
			if err != nil {
				return fmt.Errorf("执行记录指向的 assistant %s 不存在: %w", candidateAsstID, err)
			}
			asstDecoded, err := decodeMessage([]byte(asstMsgJSON))
			if err != nil {
				return fmt.Errorf("解码 assistant %s 消息失败: %w", candidateAsstID, err)
			}
			asstMsg, ok := asstDecoded.(*llm.AssistantMessage)
			if !ok {
				return fmt.Errorf("记录 %s 不是 assistant 消息", candidateAsstID)
			}
			cnt := 0
			for _, b := range asstMsg.Content {
				if b.Type == llm.TypeToolCall && b.ID == toolCallID {
					cnt++
				}
			}
			if cnt == 0 {
				return fmt.Errorf("assistant %s 未包含 tool_call_id %s", candidateAsstID, toolCallID)
			}
			if cnt > 1 {
				return fmt.Errorf("assistant %s 包含重复的 tool_call_id %s", candidateAsstID, toolCallID)
			}
		} else {
			// 2. 若无启动记录，在同 Run 中查找包含该 ToolCallID 的 assistant
			asstRows, err := tx.QueryContext(ctx, "SELECT id, message_json FROM entries WHERE session_id = ? AND run_id = ?", pe.sessionID, pe.runID)
			if err != nil {
				return fmt.Errorf("查询候选 assistant 失败: %w", err)
			}
			var matches []string
			for asstRows.Next() {
				var aID, aJSON string
				if err := asstRows.Scan(&aID, &aJSON); err != nil {
					asstRows.Close()
					return fmt.Errorf("读取候选 assistant 行: %w", err)
				}
				candDecoded, err := decodeMessage([]byte(aJSON))
				if err != nil {
					asstRows.Close()
					return fmt.Errorf("解码候选 entry %s 消息失败: %w", aID, err)
				}
				if asst, ok := candDecoded.(*llm.AssistantMessage); ok {
					cnt := 0
					for _, b := range asst.Content {
						if b.Type == llm.TypeToolCall && b.ID == toolCallID {
							cnt++
						}
					}
					if cnt == 1 {
						matches = append(matches, aID)
					} else if cnt > 1 {
						asstRows.Close()
						return fmt.Errorf("assistant %s 存在重复 tool_call_id %s", aID, toolCallID)
					}
				}
			}
			asstRows.Close()
			if err := asstRows.Err(); err != nil {
				return fmt.Errorf("遍历候选 assistant 失败: %w", err)
			}
			if len(matches) == 0 {
				return fmt.Errorf("无法确定工具结果 entry %s (tool_call_id %s) 关联的 assistant entry", pe.id, toolCallID)
			}
			if len(matches) > 1 {
				return fmt.Errorf("存在多个候选 assistant 匹配工具结果 entry %s (tool_call_id %s)，无法唯一确定", pe.id, toolCallID)
			}
			candidateAsstID = matches[0]
		}

		key := candidateAsstID + ":" + toolCallID
		if prevID, exists := assignedPairs[key]; exists {
			return fmt.Errorf("发现重复工具结果 (%s, %s)，冲突 entry %s 与 %s", candidateAsstID, toolCallID, prevID, pe.id)
		}
		assignedPairs[key] = pe.id

		_, err = tx.ExecContext(ctx, "UPDATE entries SET assistant_entry_id = ?, tool_call_id = ? WHERE id = ?", candidateAsstID, toolCallID, pe.id)
		if err != nil {
			return fmt.Errorf("回填工具结果 entry %s 失败: %w", pe.id, err)
		}
	}

	// 创建唯一约束索引
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS entries_assistant_tool_call ON entries(assistant_entry_id, tool_call_id) WHERE assistant_entry_id IS NOT NULL AND tool_call_id IS NOT NULL;`); err != nil {
		return fmt.Errorf("创建工具结果唯一索引失败: %w", err)
	}

	// 更新版本号
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", CurrentSchemaVersion)); err != nil {
		return fmt.Errorf("更新数据库版本号失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交数据库迁移事务: %w", err)
	}
	return nil
}
