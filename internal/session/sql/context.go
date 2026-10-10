package sql

const SetToolProjectionSQL = `UPDATE entries SET processing = ?, visible_content_json = ? WHERE id = ? AND session_id = ? AND processing = 'pending'`
const SelectToolArgumentsSQL = `SELECT arguments_json FROM tool_executions WHERE assistant_entry_id = ? AND tool_call_id = ?`
const SelectContextSourceSQL = `SELECT id FROM entries WHERE run_id = ? AND kind = 'context' AND source_kind = ? AND source_id = ?`

const InsertCompactionSQL = `INSERT INTO compactions(id,session_id,head_id,previous_id,source_ids_json,first_kept_index,summary,origin_json,usage_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`
const SelectCompactionSQL = `SELECT id,session_id,head_id,previous_id,source_ids_json,first_kept_index,summary,origin_json,usage_json,created_at FROM compactions WHERE id = ? AND session_id = ?`
const ListCompactionsSQL = `SELECT id,session_id,head_id,previous_id,source_ids_json,first_kept_index,summary,origin_json,usage_json,created_at FROM compactions WHERE session_id = ? ORDER BY created_at DESC,id DESC`

const InsertChildTaskSQL = `INSERT INTO child_tasks(id,parent_session_id,parent_run_id,child_session_id,role,file_scope_json,created_at) VALUES(?,?,?,?,?,?,?)`
const SelectChildTaskSQL = `SELECT id,parent_session_id,parent_run_id,child_session_id,role,file_scope_json,created_at FROM child_tasks WHERE id = ? AND parent_session_id = ?`
const ListChildTasksSQL = `SELECT id,parent_session_id,parent_run_id,child_session_id,role,file_scope_json,created_at FROM child_tasks WHERE parent_session_id = ? ORDER BY created_at,id`
const IsChildSessionSQL = `SELECT EXISTS(SELECT 1 FROM child_tasks WHERE child_session_id = ?)`

const SelectMemoryProgressSQL = `SELECT covered_entry_id,updated_at FROM memory_progress WHERE session_id = ? AND scope_key = ?`
const UpsertMemoryProgressSQL = `INSERT INTO memory_progress(session_id,scope_key,covered_entry_id,updated_at) VALUES(?,?,?,?) ON CONFLICT(session_id,scope_key) DO UPDATE SET covered_entry_id=excluded.covered_entry_id,updated_at=excluded.updated_at`
