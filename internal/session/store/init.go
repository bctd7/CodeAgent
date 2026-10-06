package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"CodeAgent/internal/llm"
	"CodeAgent/internal/session/config"
	sessql "CodeAgent/internal/session/sql"
)

// initEmptyDatabase 仅负责空数据库的初始化流程：执行集中定义的建表、索引与版本初始化。
func initEmptyDatabase(ctx context.Context, tx *sql.Tx, version int) error {
	if version != 0 {
		return fmt.Errorf("不支持的数据库结构: 数据库为空但版本为 %d", version)
	}

	if _, err := tx.ExecContext(ctx, sessql.CreateTablesSQL); err != nil {
		return fmt.Errorf("执行建表语句: %w", err)
	}

	if _, err := tx.ExecContext(ctx, sessql.CreateIndexesSQL); err != nil {
		return fmt.Errorf("执行创建索引语句: %w", err)
	}

	if _, err := tx.ExecContext(ctx, sessql.SetUserVersionV4SQL); err != nil {
		return fmt.Errorf("设置数据库版本号: %w", err)
	}

	return nil
}

type tableColumn struct {
	pk int
}

// getExistingTables 查询已有非系统数据表名集合。
func getExistingTables(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, sessql.SelectExistingTablesSQL)
	if err != nil {
		return nil, fmt.Errorf("检查已有数据表: %w", err)
	}
	defer rows.Close()

	existingTables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读取表名: %w", err)
		}
		existingTables[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历表名失败: %w", err)
	}
	return existingTables, nil
}

// getTableColumns 查询指定表的列元数据。
func getTableColumns(ctx context.Context, tx *sql.Tx, tableName string) (map[string]tableColumn, error) {
	var query string
	switch tableName {
	case "sessions":
		query = sessql.TableInfoSessionsSQL
	case "runs":
		query = sessql.TableInfoRunsSQL
	case "entries":
		query = sessql.TableInfoEntriesSQL
	case "tool_executions":
		query = sessql.TableInfoToolExecutionsSQL
	default:
		return nil, fmt.Errorf("未知表名结构查询: %s", tableName)
	}

	rows, err := tx.QueryContext(ctx, query)
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
		if colType != "TEXT" && colType != "INTEGER" {
			return nil, fmt.Errorf("不支持的数据库结构: %s.%s 类型为 %s", tableName, name, colType)
		}
		if (name == "sequence" || name == "steps") != (colType == "INTEGER") {
			return nil, fmt.Errorf("不支持的数据库结构: %s.%s 类型不符", tableName, name)
		}
		cols[name] = tableColumn{pk: pk}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历表 %s 列信息: %w", tableName, err)
	}
	return cols, nil
}

// migrateDatabase 校验已有数据库结构，并在受支持的旧版本上执行向当前版本的升级与数据回填。
func migrateDatabase(ctx context.Context, tx *sql.Tx, version int, existingTables map[string]bool) error {
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
	if toolExecCols["assistant_entry_id"].pk != 1 || toolExecCols["tool_call_id"].pk != 2 {
		return fmt.Errorf("不支持的数据库结构: tool_executions 表主键不符")
	}

	if version >= 2 {
		if _, ok := entriesCols["assistant_entry_id"]; !ok {
			return fmt.Errorf("不支持的数据库结构: 版本为 %d 但 entries 缺少 assistant_entry_id", CurrentSchemaVersion)
		}
		if _, ok := entriesCols["tool_call_id"]; !ok {
			return fmt.Errorf("不支持的数据库结构: 版本为 %d 但 entries 缺少 tool_call_id", CurrentSchemaVersion)
		}
		if err := ensureSchemaConstraints(ctx, tx); err != nil {
			return err
		}
		if version >= 3 {
			if _, ok := sessionsCols["model_selection_json"]; !ok {
				return fmt.Errorf("版本 3 缺少模型选择列")
			}
			if version == 4 {
				if _, ok := toolExecCols["arguments_json"]; !ok {
					return fmt.Errorf("version 4 missing tool arguments column")
				}
				return nil
			}
			if _, ok := toolExecCols["arguments_json"]; ok {
				return fmt.Errorf("legacy schema contains unexpected tool arguments column")
			}
			if _, err := tx.ExecContext(ctx, sessql.AddToolArgumentsSQL); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, sessql.SetUserVersionV4SQL)
			return err
		}
		if _, ok := sessionsCols["model_selection_json"]; ok {
			return fmt.Errorf("旧版本含未识别的模型选择列")
		}
		if _, err := tx.ExecContext(ctx, sessql.AddModelSelectionSQL); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, sessql.AddToolArgumentsSQL); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, sessql.SetUserVersionV4SQL)
		return err
	}

	// 执行从旧版本（version < CurrentSchemaVersion）向 v2 的升级
	if _, ok := entriesCols["assistant_entry_id"]; !ok {
		if _, err := tx.ExecContext(ctx, sessql.AddEntriesAssistantEntryIDColumnSQL); err != nil {
			return fmt.Errorf("添加 entries.assistant_entry_id 列失败: %w", err)
		}
	}
	if _, ok := entriesCols["tool_call_id"]; !ok {
		if _, err := tx.ExecContext(ctx, sessql.AddEntriesToolCallIDColumnSQL); err != nil {
			return fmt.Errorf("添加 entries.tool_call_id 列失败: %w", err)
		}
	}

	// 回填已有工具结果的关联字段
	toolEntryRows, err := tx.QueryContext(ctx, sessql.SelectUnassignedToolEntriesSQL)
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
		msg, err := config.DecodeMessage([]byte(pe.messageJSON))
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
		execRows, err := tx.QueryContext(ctx, sessql.SelectToolExecutionForMigrationSQL, pe.id)
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
			err = tx.QueryRowContext(ctx, sessql.SelectAssistantForMigrationSQL, candidateAsstID, pe.sessionID, pe.runID).Scan(&asstMsgJSON)
			if err != nil {
				return fmt.Errorf("执行记录指向的 assistant %s 不存在: %w", candidateAsstID, err)
			}
			asstDecoded, err := config.DecodeMessage([]byte(asstMsgJSON))
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
			// 若无启动记录，在同 Run 中查找包含该 ToolCallID 的 assistant
			asstRows, err := tx.QueryContext(ctx, sessql.SelectCandidateAssistantsSQL, pe.sessionID, pe.runID)
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
				candDecoded, err := config.DecodeMessage([]byte(aJSON))
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

		_, err = tx.ExecContext(ctx, sessql.UpdateBackfilledToolEntrySQL, candidateAsstID, toolCallID, pe.id)
		if err != nil {
			return fmt.Errorf("回填工具结果 entry %s 失败: %w", pe.id, err)
		}
	}

	// 补齐普通索引并核验约束，失败时回滚整个迁移。
	if err := ensureSchemaConstraints(ctx, tx); err != nil {
		return err
	}

	if _, ok := sessionsCols["model_selection_json"]; ok {
		return fmt.Errorf("旧版本含未识别的模型选择列")
	}
	if _, err := tx.ExecContext(ctx, sessql.AddModelSelectionSQL); err != nil {
		return err
	}
	// 更新版本号
	if _, err := tx.ExecContext(ctx, sessql.AddToolArgumentsSQL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, sessql.SetUserVersionV4SQL); err != nil {
		return fmt.Errorf("更新数据库版本号失败: %w", err)
	}

	return nil
}

// ensureSchemaConstraints 补齐已知索引，拒绝缺少外键、唯一约束或同名错误索引的结构。
func ensureSchemaConstraints(ctx context.Context, tx *sql.Tx) error {
	foreignKeys := map[string]map[string]string{
		"sessions":        {"head_id": "entries.id"},
		"runs":            {"session_id": "sessions.id", "start_head_id": "entries.id", "input_entry_id": "entries.id", "last_entry_id": "entries.id"},
		"entries":         {"session_id": "sessions.id", "parent_id": "entries.id", "run_id": "runs.id", "assistant_entry_id": "entries.id"},
		"tool_executions": {"session_id": "sessions.id", "run_id": "runs.id", "assistant_entry_id": "entries.id", "result_entry_id": "entries.id"},
	}
	for table, expected := range foreignKeys {
		rows, err := tx.QueryContext(ctx, sessql.SelectForeignKeysSQL, table)
		if err != nil {
			return err
		}
		actual := make(map[string]string)
		for rows.Next() {
			var column, target, key string
			if err := rows.Scan(&column, &target, &key); err != nil {
				rows.Close()
				return err
			}
			actual[column] = target + "." + key
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for column, target := range expected {
			if actual[column] != target {
				return fmt.Errorf("不支持的数据库结构: %s.%s 外键不符", table, column)
			}
		}
	}
	var uniqueSequence bool
	if err := tx.QueryRowContext(ctx, sessql.SelectSequenceConstraintSQL).Scan(&uniqueSequence); err != nil {
		return err
	}
	if !uniqueSequence {
		return fmt.Errorf("不支持的数据库结构: 缺少会话 Sequence 唯一约束")
	}
	if _, err := tx.ExecContext(ctx, sessql.CreateIndexesSQL); err != nil {
		return fmt.Errorf("补齐索引: %w", err)
	}
	for _, index := range []struct {
		table, name, columns string
		unique, partial      bool
	}{
		{"runs", "runs_session_started", "session_id,started_at,id", false, false},
		{"entries", "entries_run_sequence", "run_id,sequence", false, false},
		{"tool_executions", "tool_executions_run", "run_id", false, false},
		{"entries", "entries_assistant_tool_call", "assistant_entry_id,tool_call_id", true, true},
	} {
		var columns, definition string
		var unique, partial bool
		if err := tx.QueryRowContext(ctx, sessql.SelectIndexDefinitionSQL, index.name, index.name, index.table, index.name).Scan(&columns, &unique, &partial, &definition); err != nil {
			return err
		}
		if columns != index.columns || unique != index.unique || partial != index.partial {
			return fmt.Errorf("不支持的数据库结构: 索引 %s 定义不符", index.name)
		}
		if index.partial {
			normalized := strings.Join(strings.Fields(strings.ToLower(definition)), " ")
			if !strings.HasSuffix(strings.TrimSuffix(normalized, ";"), "where assistant_entry_id is not null and tool_call_id is not null") {
				return fmt.Errorf("不支持的数据库结构: 工具结果索引条件不符")
			}
		}
	}
	rows, err := tx.QueryContext(ctx, sessql.CheckForeignKeysSQL)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("数据库存在失效的外键关联")
	}
	return rows.Err()
}
