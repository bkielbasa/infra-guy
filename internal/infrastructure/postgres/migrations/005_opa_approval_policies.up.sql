CREATE TABLE IF NOT EXISTS policies (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    rego TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS idx_policies_tenant_id ON policies(tenant_id);

ALTER TABLE projects ADD COLUMN IF NOT EXISTS approval_policy_id TEXT REFERENCES policies(id) ON DELETE SET NULL;

ALTER TABLE runs ADD COLUMN IF NOT EXISTS plan_json BYTEA;
