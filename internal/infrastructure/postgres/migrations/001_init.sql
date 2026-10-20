CREATE TABLE IF NOT EXISTS schema_migrations (
    version text PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS projects (
    id text PRIMARY KEY,
    name text NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    default_branch text NOT NULL,
    working_directory text NOT NULL DEFAULT '',
    auto_approve boolean NOT NULL DEFAULT false,
    locked boolean NOT NULL DEFAULT false,
    github_token text NOT NULL DEFAULT '',
    webhook_secret text NOT NULL DEFAULT '',
    state_token text NOT NULL DEFAULT '',
    active_run_id text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    trigger text NOT NULL,
    status text NOT NULL,
    commit_sha text NOT NULL,
    branch text NOT NULL DEFAULT '',
    commit_message text NOT NULL DEFAULT '',
    author text NOT NULL DEFAULT '',
    triggered_by text NOT NULL DEFAULT '',
    pr_number integer,
    pr_title text NOT NULL DEFAULT '',
    plan_only boolean NOT NULL DEFAULT false,
    auto_approve boolean NOT NULL DEFAULT false,
    plan_text text NOT NULL DEFAULT '',
    plan_file bytea,
    add_count integer NOT NULL DEFAULT 0,
    change_count integer NOT NULL DEFAULT 0,
    destroy_count integer NOT NULL DEFAULT 0,
    summary_present boolean NOT NULL DEFAULT false,
    terraform_version text NOT NULL DEFAULT '',
    error text NOT NULL DEFAULT '',
    approved_by text NOT NULL DEFAULT '',
    approved_at timestamptz,
    created_at timestamptz NOT NULL,
    started_at timestamptz,
    finished_at timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS runs_one_active_per_key
ON runs (project_id, commit_sha, trigger, pr_number)
NULLS NOT DISTINCT
WHERE status IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS steps (
    run_id text NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    name text NOT NULL,
    position integer NOT NULL,
    status text NOT NULL,
    logs text NOT NULL DEFAULT '',
    exit_code integer,
    started_at timestamptz,
    finished_at timestamptz,
    UNIQUE (run_id, name)
);

CREATE TABLE IF NOT EXISTS jobs (
    id bigserial PRIMARY KEY,
    run_id text NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    step_name text NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz,
    locked_at timestamptz,
    locked_by text,
    last_error text
);

CREATE TABLE IF NOT EXISTS states (
    project_id text PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    data bytea,
    lock_id text,
    lock_info bytea,
    serial bigint,
    updated_at timestamptz
);

CREATE TABLE IF NOT EXISTS state_history (
    id bigserial PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    run_id text,
    serial bigint,
    data bytea,
    created_at timestamptz NOT NULL
);
