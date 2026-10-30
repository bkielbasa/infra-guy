CREATE TABLE folders (
    id text PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants (id),
    parent_id text REFERENCES folders (id) ON DELETE RESTRICT,
    name text NOT NULL,
    pass_down boolean NOT NULL DEFAULT false,
    tags_set boolean NOT NULL,
    tags jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE UNIQUE INDEX folders_one_root ON folders (tenant_id) WHERE parent_id IS NULL;
CREATE UNIQUE INDEX folders_sibling_name ON folders (parent_id, lower(name)) WHERE parent_id IS NOT NULL;
CREATE INDEX folders_tenant ON folders (tenant_id);
CREATE INDEX folders_parent ON folders (parent_id);

CREATE TABLE folder_policies (
    folder_id text NOT NULL REFERENCES folders (id) ON DELETE CASCADE,
    policy_id text NOT NULL REFERENCES policies (id) ON DELETE CASCADE,
    PRIMARY KEY (folder_id, policy_id)
);
CREATE INDEX folder_policies_policy ON folder_policies (policy_id);

CREATE TABLE project_policies (
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    policy_id text NOT NULL REFERENCES policies (id) ON DELETE CASCADE,
    PRIMARY KEY (project_id, policy_id)
);
CREATE INDEX project_policies_policy ON project_policies (policy_id);

INSERT INTO folders (id, tenant_id, parent_id, name, pass_down, tags_set, tags, created_at, updated_at)
SELECT replace(gen_random_uuid()::text, '-', ''), id, NULL, 'Default', false, true, '{}'::jsonb, now(), now()
FROM tenants;

ALTER TABLE projects ADD COLUMN folder_id text REFERENCES folders (id) ON DELETE RESTRICT;
ALTER TABLE projects ADD COLUMN tags_overridden boolean NOT NULL DEFAULT false;

UPDATE projects AS p
SET folder_id = f.id,
    tags_overridden = true
FROM folders AS f
WHERE f.tenant_id = p.tenant_id AND f.parent_id IS NULL;

ALTER TABLE projects ALTER COLUMN folder_id SET NOT NULL;

INSERT INTO project_policies (project_id, policy_id)
SELECT id, approval_policy_id
FROM projects
WHERE approval_policy_id IS NOT NULL AND approval_policy_id <> '';

ALTER TABLE projects DROP COLUMN approval_policy_id;
