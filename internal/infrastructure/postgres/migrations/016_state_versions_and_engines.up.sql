CREATE TABLE IF NOT EXISTS state_versions (
    id bigserial PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    serial bigint NOT NULL,
    data bytea NOT NULL,
    size_bytes int NOT NULL DEFAULT 0,
    run_id text NOT NULL DEFAULT '',
    actor text NOT NULL DEFAULT '',
    resource_count int NOT NULL DEFAULT 0,
    changed_outputs jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, serial)
);

CREATE INDEX IF NOT EXISTS state_versions_project_serial_idx
    ON state_versions (project_id, serial DESC);

INSERT INTO state_versions (project_id, serial, data, size_bytes, changed_outputs, created_at)
SELECT project_id, serial, data, octet_length(data), COALESCE(changed_outputs, '[]'::jsonb), COALESCE(updated_at, now())
FROM states
WHERE data IS NOT NULL AND serial IS NOT NULL
ON CONFLICT (project_id, serial) DO NOTHING;

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS engine text NOT NULL DEFAULT 'terraform',
    ADD COLUMN IF NOT EXISTS engine_config jsonb NOT NULL DEFAULT '{}'::jsonb;
