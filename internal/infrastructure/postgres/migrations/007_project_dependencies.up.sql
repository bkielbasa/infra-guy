CREATE TABLE IF NOT EXISTS project_dependencies (
    id text PRIMARY KEY,
    tenant_id text NOT NULL,
    project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    depends_on_project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    trigger_always boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    UNIQUE (project_id, depends_on_project_id)
);

CREATE INDEX IF NOT EXISTS project_dependencies_project_idx ON project_dependencies (project_id);
CREATE INDEX IF NOT EXISTS project_dependencies_upstream_idx ON project_dependencies (depends_on_project_id);

CREATE TABLE IF NOT EXISTS project_dependency_mappings (
    id text PRIMARY KEY,
    dependency_id text NOT NULL REFERENCES project_dependencies(id) ON DELETE CASCADE,
    output_name text NOT NULL,
    input_name text NOT NULL,
    created_at timestamptz NOT NULL,
    UNIQUE (dependency_id, input_name)
);
