ALTER TABLE projects
    ADD COLUMN terraform_version text NOT NULL DEFAULT '1.9.8';

CREATE TABLE contexts (
    id         text PRIMARY KEY,
    tenant_id  text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (tenant_id, name)
);

CREATE INDEX contexts_tenant_idx ON contexts (tenant_id);

CREATE TABLE context_entries (
    id               text PRIMARY KEY,
    context_id       text NOT NULL REFERENCES contexts(id) ON DELETE CASCADE,
    kind             text NOT NULL CHECK (kind IN ('plain_env', 'secret_env', 'secret_file')),
    key              text NOT NULL,
    value_text       text NOT NULL DEFAULT '',
    value_ciphertext bytea,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    UNIQUE (context_id, kind, key)
);

CREATE INDEX context_entries_context_idx ON context_entries (context_id);

CREATE TABLE project_contexts (
    project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    context_id text NOT NULL REFERENCES contexts(id) ON DELETE CASCADE,
    position   integer NOT NULL,
    PRIMARY KEY (project_id, context_id)
);

CREATE INDEX project_contexts_project_idx ON project_contexts (project_id, position);
