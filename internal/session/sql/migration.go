package sql

const SelectForeignKeysSQL = `SELECT "from", "table", "to" FROM pragma_foreign_key_list(?)`
const CheckForeignKeysSQL = `PRAGMA foreign_key_check`
const SelectSequenceConstraintSQL = `
SELECT EXISTS (
    SELECT 1 FROM pragma_index_list('entries') i
    WHERE i."unique" = 1 AND i.partial = 0
    AND (SELECT group_concat(name, ',') FROM
        (SELECT name FROM pragma_index_info(i.name) ORDER BY seqno)) = 'session_id,sequence'
)`
const SelectIndexDefinitionSQL = `
SELECT (SELECT group_concat(name, ',') FROM
    (SELECT name FROM pragma_index_info(?) ORDER BY seqno)),
    i."unique", i.partial, (SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?)
FROM pragma_index_list(?) i WHERE i.name = ?`

// SelectUserVersionSQL 读取数据库 user_version。
const SelectUserVersionSQL = `PRAGMA user_version`

// SelectExistingTablesSQL 查询用户创建的全部非系统数据表名。
const SelectExistingTablesSQL = `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`

// TableInfo 查询各表的列结构及主键定义。
const (
	TableInfoSessionsSQL       = `PRAGMA table_info(sessions)`
	TableInfoRunsSQL           = `PRAGMA table_info(runs)`
	TableInfoEntriesSQL        = `PRAGMA table_info(entries)`
	TableInfoToolExecutionsSQL = `PRAGMA table_info(tool_executions)`
)

// AddEntriesAssistantEntryIDColumnSQL 在升级 v2 时向 entries 表添加 assistant_entry_id 列。
const AddEntriesAssistantEntryIDColumnSQL = `ALTER TABLE entries ADD COLUMN assistant_entry_id TEXT REFERENCES entries(id)`

// AddEntriesToolCallIDColumnSQL 在升级 v2 时向 entries 表添加 tool_call_id 列。
const AddEntriesToolCallIDColumnSQL = `ALTER TABLE entries ADD COLUMN tool_call_id TEXT`

// SelectUnassignedToolEntriesSQL 查询尚未关联 assistant 信息的旧 entries 记录。
const SelectUnassignedToolEntriesSQL = `SELECT id, session_id, run_id, message_json FROM entries WHERE assistant_entry_id IS NULL`

// SelectToolExecutionForMigrationSQL 根据 result_entry_id 查询执行事实。
const SelectToolExecutionForMigrationSQL = `SELECT session_id, run_id, assistant_entry_id, tool_call_id FROM tool_executions WHERE result_entry_id = ? LIMIT 2`

// SelectAssistantForMigrationSQL 检验指定 assistant 记录是否合法。
const SelectAssistantForMigrationSQL = `SELECT message_json FROM entries WHERE id = ? AND session_id = ? AND run_id = ?`

// SelectCandidateAssistantsSQL 查询同一 Run 内的所有候选 assistant 记录。
const SelectCandidateAssistantsSQL = `SELECT id, message_json FROM entries WHERE session_id = ? AND run_id = ?`

// UpdateBackfilledToolEntrySQL 回填 entries 记录的 assistant 关联字段。
const UpdateBackfilledToolEntrySQL = `UPDATE entries SET assistant_entry_id = ?, tool_call_id = ? WHERE id = ?`

const AddModelSelectionSQL = `ALTER TABLE sessions ADD COLUMN model_selection_json TEXT`

const AddToolArgumentsSQL = `ALTER TABLE tool_executions ADD COLUMN arguments_json TEXT`
