ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS drift_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS drift_interval_minutes INT NOT NULL DEFAULT 360,
    ADD COLUMN IF NOT EXISTS drift_status TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS last_drift_check_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS ttl_hours INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS auto_destroy BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE runs
    ADD COLUMN IF NOT EXISTS destroy BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS monthly_cost DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS diff_monthly_cost DOUBLE PRECISION NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS cloud_integrations (
    id                             TEXT PRIMARY KEY,
    tenant_id                      TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name                           TEXT NOT NULL,
    provider                       TEXT NOT NULL DEFAULT 'aws',
    auth_mode                      TEXT NOT NULL DEFAULT 'oidc',
    role_arn                       TEXT NOT NULL DEFAULT '',
    region                         TEXT NOT NULL DEFAULT 'us-east-1',
    audience                       TEXT NOT NULL DEFAULT 'sts.amazonaws.com',
    session_duration_seconds       INT NOT NULL DEFAULT 3600,
    external_id                    TEXT NOT NULL DEFAULT '',
    access_key_id                  TEXT NOT NULL DEFAULT '',
    secret_access_key_ciphertext   BYTEA,
    gcp_workload_identity_provider TEXT NOT NULL DEFAULT '',
    gcp_service_account_email      TEXT NOT NULL DEFAULT '',
    azure_client_id                TEXT NOT NULL DEFAULT '',
    azure_tenant_id                TEXT NOT NULL DEFAULT '',
    azure_subscription_id          TEXT NOT NULL DEFAULT '',
    tags                           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at                     TIMESTAMPTZ NOT NULL,
    updated_at                     TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS idx_cloud_integrations_tenant ON cloud_integrations(tenant_id);

CREATE TABLE IF NOT EXISTS project_cloud_integrations (
    project_id     TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    integration_id TEXT NOT NULL REFERENCES cloud_integrations(id) ON DELETE CASCADE,
    position       INT NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, integration_id)
);
