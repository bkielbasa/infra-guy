DROP TABLE IF EXISTS state_history;

ALTER TABLE states
    ADD COLUMN IF NOT EXISTS changed_outputs jsonb NOT NULL DEFAULT '{}'::jsonb;
