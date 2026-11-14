-- 002_admin.sql: Runtime model overrides and dynamically managed Ollama models for Web Admin.

CREATE TABLE IF NOT EXISTS model_overrides (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL DEFAULT '',
    base_url TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    enabled INTEGER,
    timeout_seconds INTEGER,
    max_concurrent INTEGER,
    dynamic INTEGER NOT NULL DEFAULT 0,
    deleted INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);
