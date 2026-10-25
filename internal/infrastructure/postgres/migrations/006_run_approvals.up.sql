CREATE TABLE IF NOT EXISTS run_approvals (
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL DEFAULT '',
    user_login TEXT NOT NULL,
    user_role TEXT NOT NULL DEFAULT '',
    approved_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (run_id, user_login)
);
