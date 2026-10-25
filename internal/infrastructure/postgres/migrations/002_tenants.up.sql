DELETE FROM projects;

CREATE TABLE tenants (
    id text PRIMARY KEY,
    name text NOT NULL,
    github_account_id bigint NOT NULL UNIQUE,
    account_type text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE users (
    id text PRIMARY KEY,
    github_user_id bigint NOT NULL UNIQUE,
    login text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE identities (
    user_id text NOT NULL REFERENCES users (id),
    provider text NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    PRIMARY KEY (provider, issuer, subject)
);

CREATE TABLE installations (
    id text PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants (id),
    github_installation_id bigint NOT NULL UNIQUE,
    account_login text NOT NULL,
    account_type text NOT NULL,
    active boolean NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE UNIQUE INDEX installations_one_active_per_tenant
ON installations (tenant_id) WHERE active;

CREATE TABLE sessions (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users (id),
    tenant_id text REFERENCES tenants (id),
    role text NOT NULL DEFAULT '',
    login text NOT NULL DEFAULT '',
    access_token text NOT NULL DEFAULT '',
    expires_at timestamptz NOT NULL
);

CREATE TABLE oauth_states (
    state text PRIMARY KEY,
    expires_at timestamptz NOT NULL
);

ALTER TABLE projects
    ADD COLUMN tenant_id text NOT NULL REFERENCES tenants (id),
    ADD COLUMN github_repo_id bigint NOT NULL;

CREATE INDEX projects_tenant_repo ON projects (tenant_id, github_repo_id);
