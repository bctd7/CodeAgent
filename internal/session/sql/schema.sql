-- 时间统一保存为 UTC RFC3339Nano 文本；可选 ID 和时间未设置时使用 NULL。
-- Message、Origin 和 Usage 使用 JSON 文本，编解码由 Go 层负责。
-- Store 应为每个连接启用 PRAGMA foreign_keys = ON。
-- Run 与首条 Entry 相互引用，创建时须在同一事务内写入。
-- 关联记录所属会话、Run 和消息角色由 Store 写入逻辑检查。

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY NOT NULL,
    title TEXT NOT NULL,
    working_dir TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    archived_at TEXT,
    head_id TEXT REFERENCES entries(id) DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS runs (
    id TEXT PRIMARY KEY NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    start_head_id TEXT REFERENCES entries(id),
    input_entry_id TEXT NOT NULL REFERENCES entries(id) DEFERRABLE INITIALLY DEFERRED,
    last_entry_id TEXT REFERENCES entries(id),
    started_at TEXT NOT NULL,
    finished_at TEXT,
    state TEXT NOT NULL,
    steps INTEGER NOT NULL DEFAULT 0,
    stop_reason TEXT NOT NULL DEFAULT '',
    raw_stop_reason TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS entries (
    id TEXT PRIMARY KEY NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    parent_id TEXT REFERENCES entries(id),
    sequence INTEGER NOT NULL,
    run_id TEXT NOT NULL REFERENCES runs(id) DEFERRABLE INITIALLY DEFERRED,
    created_at TEXT NOT NULL,
    message_json TEXT NOT NULL,
    origin_json TEXT,
    assistant_entry_id TEXT REFERENCES entries(id),
    tool_call_id TEXT,
    UNIQUE (session_id, sequence)
);

CREATE TABLE IF NOT EXISTS tool_executions (
    session_id TEXT NOT NULL REFERENCES sessions(id),
    run_id TEXT NOT NULL REFERENCES runs(id),
    assistant_entry_id TEXT NOT NULL REFERENCES entries(id),
    tool_call_id TEXT NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    result_entry_id TEXT REFERENCES entries(id),
    PRIMARY KEY (assistant_entry_id, tool_call_id)
);

-- 会话内列出任务、按任务读取消息和工具执行记录。
CREATE INDEX IF NOT EXISTS runs_session_started ON runs(session_id, started_at, id);
CREATE INDEX IF NOT EXISTS entries_run_sequence ON entries(run_id, sequence);
CREATE INDEX IF NOT EXISTS tool_executions_run ON tool_executions(run_id);
CREATE UNIQUE INDEX IF NOT EXISTS entries_assistant_tool_call ON entries(assistant_entry_id, tool_call_id) WHERE assistant_entry_id IS NOT NULL AND tool_call_id IS NOT NULL;

