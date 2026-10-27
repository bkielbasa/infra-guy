CREATE TABLE IF NOT EXISTS helm_revisions (
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    release_name text NOT NULL,
    namespace text NOT NULL DEFAULT '',
    revision integer NOT NULL,
    updated text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT '',
    chart text NOT NULL DEFAULT '',
    app_version text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, revision)
);

ALTER TABLE runs
    ADD COLUMN IF NOT EXISTS helm_rollback_revision integer NOT NULL DEFAULT 0;
