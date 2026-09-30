package sql

const SelectRunSessionIDSQL = `SELECT session_id FROM runs WHERE id = ?`

// InsertRunSQL 创建一条初始状态为 pending 的 Run 记录（初始 last_entry_id 为 NULL，待写入 Entry 后更新）。
const InsertRunSQL = `
INSERT INTO runs (
    id, session_id, start_head_id, input_entry_id, last_entry_id,
    started_at, finished_at, state, steps, stop_reason, raw_stop_reason, error_message
) VALUES (?, ?, ?, ?, NULL, ?, NULL, ?, 0, '', '', '')
`

// SelectRunByIDSQL 按会话 ID 和 Run ID 查询 Run 详情。
const SelectRunByIDSQL = `
SELECT id, session_id, start_head_id, input_entry_id, last_entry_id, started_at, finished_at, state, steps, stop_reason, raw_stop_reason, error_message
FROM runs
WHERE id = ? AND session_id = ?
`

// SelectActiveRunIDSQL 检查指定会话是否存在活动 Run。
const SelectActiveRunIDSQL = `
SELECT id FROM runs WHERE session_id = ? AND finished_at IS NULL LIMIT 1
`

// SelectRunStepsAndFinishedAtSQL 查询用于 RecordStep 的步骤计数与结束状态。
const SelectRunStepsAndFinishedAtSQL = `
SELECT steps, finished_at FROM runs WHERE id = ? AND session_id = ?
`

// SelectRunLastEntryAndFinishedAtSQL 查询用于分支切换的目标 Run 信息。
const SelectRunLastEntryAndFinishedAtSQL = `
SELECT last_entry_id, finished_at FROM runs WHERE id = ? AND session_id = ?
`

// UpdateRunLastEntryIDSQL 更新 Run 的最后一条 Entry 引用。
const UpdateRunLastEntryIDSQL = `
UPDATE runs SET last_entry_id = ? WHERE id = ?
`

// IncrementRunStepsSQL 更新 Run 的已执行步数。
const IncrementRunStepsSQL = `
UPDATE runs SET steps = ? WHERE id = ?
`

// UpdateRunFinishSQL 更新 Run 终态（供 FinishRun 与 MarkRunAborted 共同复用）。
const UpdateRunFinishSQL = `
UPDATE runs
SET finished_at = ?, state = ?, stop_reason = ?, raw_stop_reason = ?, error_message = ?
WHERE id = ?
`

// SelectListRunsSQL 按输入 Entry 的 Sequence 倒序查询会话的所有 Run。
const SelectListRunsSQL = `
SELECT r.id, r.session_id, r.start_head_id, r.input_entry_id, r.last_entry_id,
       r.started_at, r.finished_at, r.state, r.steps, r.stop_reason, r.raw_stop_reason, r.error_message
FROM runs r
JOIN entries e ON r.input_entry_id = e.id
WHERE r.session_id = ?
ORDER BY e.sequence DESC, r.id ASC
LIMIT ? OFFSET ?
`

// SelectUnfinishedRunsBySessionSQL 查询会话内未结束的 Run。
const SelectUnfinishedRunsBySessionSQL = `
SELECT id, session_id, start_head_id, input_entry_id, last_entry_id, started_at, finished_at, state, steps, stop_reason, raw_stop_reason, error_message
FROM runs
WHERE session_id = ? AND finished_at IS NULL
ORDER BY strftime('%Y-%m-%dT%H:%M:%f', started_at) ASC, id ASC
`

// SelectAllUnfinishedRunsSQL 跨会话查询所有未结束的 Run。
const SelectAllUnfinishedRunsSQL = `
SELECT id, session_id, start_head_id, input_entry_id, last_entry_id, started_at, finished_at, state, steps, stop_reason, raw_stop_reason, error_message
FROM runs
WHERE finished_at IS NULL
ORDER BY strftime('%Y-%m-%dT%H:%M:%f', started_at) ASC, id ASC
`
