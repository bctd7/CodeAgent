package sql

// InsertEntrySQL 插入一条新的 Entry 记录。
const InsertEntrySQL = `
INSERT INTO entries (id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id, kind, source_kind, source_id, processing, visible_content_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`

// SelectMaxSequenceSQL 计算指定会话内的下一 Sequence 序号。
const SelectMaxSequenceSQL = `
SELECT COALESCE(MAX(sequence), 0) + 1 FROM entries WHERE session_id = ?
`

// SelectEntryByIDSQL 按会话 ID 和 Entry ID 查询单条记录。
const SelectEntryByIDSQL = `
SELECT id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id, kind, source_kind, source_id, processing, visible_content_json
FROM entries
WHERE id = ? AND session_id = ?
`

// SelectToolResultEntryIDSQL 查询是否已存在指定 assistant_entry_id 与 tool_call_id 的结果记录。
const SelectToolResultEntryIDSQL = `
SELECT id FROM entries WHERE assistant_entry_id = ? AND tool_call_id = ? LIMIT 1
`

// SelectEntriesByRunIDSQL 按 Sequence 正序查询指定 Run 的全部 Entry。
const SelectEntriesByRunIDSQL = `
SELECT id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id, kind, source_kind, source_id, processing, visible_content_json
FROM entries
WHERE run_id = ? AND session_id = ?
ORDER BY sequence ASC
`

// SelectAncestorChainSQL 使用递归公用表表达式追溯指定 HeadID 向上到根节点的祖先链。
const SelectAncestorChainSQL = `
WITH RECURSIVE ancestor_chain(id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id, kind, source_kind, source_id, processing, visible_content_json) AS (
    SELECT id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id, kind, source_kind, source_id, processing, visible_content_json
    FROM entries
    WHERE id = ? AND session_id = ?
    UNION
    SELECT e.id, e.session_id, e.parent_id, e.sequence, e.run_id, e.created_at, e.message_json, e.origin_json, e.assistant_entry_id, e.tool_call_id, e.kind, e.source_kind, e.source_id, e.processing, e.visible_content_json
    FROM entries e
    JOIN ancestor_chain a ON e.id = a.parent_id
    WHERE e.session_id = ?
)
SELECT id, session_id, parent_id, sequence, run_id, created_at, message_json, origin_json, assistant_entry_id, tool_call_id, kind, source_kind, source_id, processing, visible_content_json
FROM ancestor_chain;
`
