-- 001_init.sql: Metadata schema for CLI AI Gateway (metadata only, no large prompt payloads).

CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    name TEXT NOT NULL,
    key_hash TEXT NOT NULL UNIQUE,
    allowed_models_json TEXT NOT NULL DEFAULT '["*"]',
    max_concurrent INTEGER NOT NULL DEFAULT 4,
    revoked INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_api_keys_owner ON api_keys(owner_id);

CREATE TABLE IF NOT EXISTS files (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    filename TEXT NOT NULL,
    purpose TEXT NOT NULL,
    bytes INTEGER NOT NULL,
    content_type TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    storage_name TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    active_refs INTEGER NOT NULL DEFAULT 0,
    deleted INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_files_owner_created ON files(owner_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_files_expires ON files(expires_at) WHERE deleted = 0;

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    provider_session_id TEXT NOT NULL DEFAULT '',
    last_response_id TEXT NOT NULL DEFAULT '',
    active_run_id TEXT NOT NULL DEFAULT '',
    turn_count INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_owner_updated ON sessions(owner_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS responses (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    previous_response_id TEXT NOT NULL DEFAULT '',
    run_id TEXT NOT NULL,
    model TEXT NOT NULL,
    provider TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('in_progress', 'completed', 'failed', 'interrupted', 'cancelled')),
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    completed_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_responses_owner_created ON responses(owner_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_responses_session ON responses(session_id, created_at ASC);

CREATE TABLE IF NOT EXISTS runs (
    id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    response_id TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL,
    provider TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'failed', 'timed_out', 'cancelled', 'interrupted')),
    input_bytes INTEGER NOT NULL DEFAULT 0,
    file_ids_json TEXT NOT NULL DEFAULT '[]',
    exit_code INTEGER,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    error_type TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    finished_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);
CREATE INDEX IF NOT EXISTS idx_runs_session_created ON runs(session_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_created ON runs(created_at);
