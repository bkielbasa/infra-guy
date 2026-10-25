ALTER TABLE states DROP COLUMN IF EXISTS changed_outputs;

CREATE TABLE IF NOT EXISTS state_history (
    id bigserial PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    run_id text,
    serial bigint,
    data bytea,
    created_at timestamptz NOT NULL
);
