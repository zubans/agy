-- 003_worker.sql: Remote container worker URL support for distributed CLI agent workers.

ALTER TABLE model_overrides ADD COLUMN worker_url TEXT NOT NULL DEFAULT '';
