-- 004_model_pools.sql: Provider visibility, model discovery cache, published/composite models, and per-(provider, model) cooldowns.

ALTER TABLE model_overrides ADD COLUMN public INTEGER;

CREATE TABLE IF NOT EXISTS provider_models_cache (
    provider_id TEXT NOT NULL,
    model_name TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'manual',
    is_default INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (provider_id, model_name)
);

CREATE TABLE IF NOT EXISTS published_models (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT 'composite' CHECK (kind IN ('single', 'composite')),
    strategy TEXT NOT NULL DEFAULT 'priority_order' CHECK (strategy IN ('priority_order', 'round_robin')),
    enabled INTEGER NOT NULL DEFAULT 1,
    default_cooldown_seconds INTEGER NOT NULL DEFAULT 300,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS published_model_targets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    published_model_id TEXT NOT NULL REFERENCES published_models(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    provider_model TEXT NOT NULL DEFAULT '',
    priority INTEGER NOT NULL DEFAULT 1,
    position INTEGER NOT NULL DEFAULT 0,
    enabled INTEGER NOT NULL DEFAULT 1,
    UNIQUE (published_model_id, provider_id, provider_model)
);

CREATE INDEX IF NOT EXISTS idx_pmt_model ON published_model_targets(published_model_id, priority ASC, position ASC);

CREATE TABLE IF NOT EXISTS provider_model_cooldowns (
    provider_id TEXT NOT NULL,
    provider_model TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    cooling_until INTEGER NOT NULL,
    source TEXT NOT NULL DEFAULT 'default',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (provider_id, provider_model)
);

CREATE INDEX IF NOT EXISTS idx_pmc_until ON provider_model_cooldowns(cooling_until);

ALTER TABLE sessions ADD COLUMN target_provider_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN target_model TEXT NOT NULL DEFAULT '';

ALTER TABLE runs ADD COLUMN target_provider_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN target_model TEXT NOT NULL DEFAULT '';

ALTER TABLE responses ADD COLUMN target_provider_id TEXT NOT NULL DEFAULT '';
ALTER TABLE responses ADD COLUMN target_model TEXT NOT NULL DEFAULT '';
