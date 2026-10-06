package sql

// InsertToolExecutionSQL 插入一条工具调用的启动事实记录。
const InsertToolExecutionSQL = `
INSERT INTO tool_executions (session_id, run_id, assistant_entry_id, tool_call_id, started_at, finished_at, result_entry_id, arguments_json)
VALUES (?, ?, ?, ?, ?, NULL, NULL, ?)
`

// SelectToolExecutionResultEntryIDSQL 查询指定调用的已关联结果记录。
const SelectToolExecutionResultEntryIDSQL = `
SELECT result_entry_id
FROM tool_executions
WHERE assistant_entry_id = ? AND tool_call_id = ?
`

// SelectToolExecutionExistenceSQL 检查指定调用的启动事实是否存在。
const SelectToolExecutionExistenceSQL = `
SELECT 1
FROM tool_executions
WHERE assistant_entry_id = ? AND tool_call_id = ?
`

// UpdateToolExecutionFinishSQL 更新工具执行的完成时间与结果引用。
const UpdateToolExecutionFinishSQL = `
UPDATE tool_executions
SET finished_at = ?, result_entry_id = ?
WHERE assistant_entry_id = ? AND tool_call_id = ?
`

// SelectToolExecutionsByRunIDSQL 查询指定 Run 下所有工具执行记录。
const SelectToolExecutionsByRunIDSQL = `
SELECT session_id, run_id, assistant_entry_id, tool_call_id, started_at, finished_at, result_entry_id, arguments_json
FROM tool_executions
WHERE run_id = ? AND session_id = ?
ORDER BY strftime('%Y-%m-%dT%H:%M:%f', started_at) ASC, assistant_entry_id ASC, tool_call_id ASC
`
