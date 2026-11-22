-- 005_media_and_tools.sql: Multimodal file metadata, server-side tool backends, and published model tool bindings (ADR-004).

ALTER TABLE files ADD COLUMN media_kind TEXT NOT NULL DEFAULT 'text';
ALTER TABLE files ADD COLUMN width INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN height INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN duration_sec REAL NOT NULL DEFAULT 0;

ALTER TABLE published_models ADD COLUMN capabilities_json TEXT NOT NULL DEFAULT '{}';

CREATE TABLE IF NOT EXISTS tool_backends (
    key TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    base_url TEXT NOT NULL DEFAULT '',
    api_key TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 0,
    extra_json TEXT NOT NULL DEFAULT '{}',
    updated_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS published_model_tools (
    published_model_id TEXT NOT NULL REFERENCES published_models(id) ON DELETE CASCADE,
    tool_name TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (published_model_id, tool_name)
);

CREATE INDEX IF NOT EXISTS idx_pmtools_model ON published_model_tools(published_model_id);
