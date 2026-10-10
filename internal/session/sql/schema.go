package sql

// CreateTablesSQL 定义空数据库初始化时的建表语句。
const CreateTablesSQL = `
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY NOT NULL,
    title TEXT NOT NULL,
    working_dir TEXT NOT NULL,
    model_selection_json TEXT,
    memory_enabled INTEGER NOT NULL DEFAULT 1 CHECK (memory_enabled IN (0,1)),
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
    kind TEXT NOT NULL DEFAULT 'message' CHECK (kind IN ('message','context')),
    source_kind TEXT,
    source_id TEXT,
    processing TEXT NOT NULL DEFAULT 'original' CHECK (processing IN ('pending','original','replaced')),
    visible_content_json TEXT,
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
    arguments_json TEXT,
    status TEXT NOT NULL,
    executed INTEGER,
    error_message TEXT NOT NULL DEFAULT '',
    started_at TEXT,
    finished_at TEXT,
    result_entry_id TEXT REFERENCES entries(id),
    PRIMARY KEY (assistant_entry_id, tool_call_id)
);
CREATE TABLE IF NOT EXISTS compactions (
    id TEXT PRIMARY KEY NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    head_id TEXT NOT NULL REFERENCES entries(id),
    previous_id TEXT REFERENCES compactions(id),
    source_ids_json TEXT NOT NULL,
    first_kept_index INTEGER NOT NULL,
    summary TEXT NOT NULL,
    origin_json TEXT NOT NULL,
    usage_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS child_tasks (
    id TEXT PRIMARY KEY NOT NULL,
    parent_session_id TEXT NOT NULL REFERENCES sessions(id),
    parent_run_id TEXT NOT NULL REFERENCES runs(id),
    child_session_id TEXT NOT NULL UNIQUE REFERENCES sessions(id),
    role TEXT NOT NULL CHECK (role IN ('explore','worker')),
    file_scope_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS memory_progress (
    session_id TEXT NOT NULL REFERENCES sessions(id),
    scope_key TEXT NOT NULL,
    covered_entry_id TEXT NOT NULL REFERENCES entries(id),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (session_id, scope_key)
);

`

// CreateIndexesSQL 定义数据库索引创建语句。
const CreateIndexesSQL = `
CREATE INDEX IF NOT EXISTS runs_session_started ON runs(session_id, started_at, id);
CREATE INDEX IF NOT EXISTS entries_run_sequence ON entries(run_id, sequence);
CREATE INDEX IF NOT EXISTS tool_executions_run ON tool_executions(run_id);
CREATE INDEX IF NOT EXISTS compactions_session ON compactions(session_id, created_at, id);
CREATE INDEX IF NOT EXISTS child_tasks_parent ON child_tasks(parent_session_id, parent_run_id);
CREATE UNIQUE INDEX IF NOT EXISTS entries_context_source ON entries(run_id, source_kind, source_id) WHERE kind = 'context';
CREATE UNIQUE INDEX IF NOT EXISTS entries_assistant_tool_call ON entries(assistant_entry_id, tool_call_id) WHERE assistant_entry_id IS NOT NULL AND tool_call_id IS NOT NULL;
`

// SetUserVersionSQL 只初始化当前版本，不升级已有数据库。
const SetUserVersionSQL = `PRAGMA user_version = 6`
