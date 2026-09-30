package sql

const SelectSessionsSQL = `
SELECT id, title, working_dir, created_at, updated_at, archived_at, head_id FROM sessions
WHERE (? = 'all' OR (? = 'active' AND archived_at IS NULL) OR (? = 'archived' AND archived_at IS NOT NULL))
AND (? = '' OR INSTR(LOWER(title), LOWER(?)) > 0)
ORDER BY strftime('%Y-%m-%dT%H:%M:%f', updated_at) DESC, id ASC LIMIT ? OFFSET ?`

// InsertSessionSQL 插入一条新的会话记录。
const InsertSessionSQL = `
INSERT INTO sessions (id, title, working_dir, created_at, updated_at, archived_at, head_id)
VALUES (?, ?, ?, ?, ?, ?, ?)
`

// SelectSessionByIDSQL 按会话 ID 查询完整的会话元数据。
const SelectSessionByIDSQL = `
SELECT id, title, working_dir, created_at, updated_at, archived_at, head_id
FROM sessions
WHERE id = ?
`

// UpdateSessionTitleSQL 强制更新会话标题。
const UpdateSessionTitleSQL = `
UPDATE sessions
SET title = ?, updated_at = ?
WHERE id = ?
`

// UpdateSessionTitleIfEmptySQL 仅在会话当前标题为空时更新标题。
const UpdateSessionTitleIfEmptySQL = `
UPDATE sessions
SET title = ?, updated_at = ?
WHERE id = ? AND (title = '' OR title IS NULL)
`

// UpdateSessionWorkingDirSQL 首次绑定会话的工作目录。
const UpdateSessionWorkingDirSQL = `
UPDATE sessions
SET working_dir = ?, updated_at = ?
WHERE id = ?
`

// UpdateSessionArchiveSQL 将会话标记为归档。
const UpdateSessionArchiveSQL = `
UPDATE sessions
SET archived_at = ?, updated_at = ?
WHERE id = ?
`

// UpdateSessionUnarchiveSQL 取消会话归档。
const UpdateSessionUnarchiveSQL = `
UPDATE sessions
SET archived_at = NULL, updated_at = ?
WHERE id = ?
`

// UpdateSessionHeadSQL 更新会话的历史 Head 指针与更新时间。
const UpdateSessionHeadSQL = `
UPDATE sessions
SET head_id = ?, updated_at = ?
WHERE id = ?
`
