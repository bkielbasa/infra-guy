DROP INDEX IF EXISTS project_contexts_project_idx;
DROP TABLE IF EXISTS project_contexts;
DROP INDEX IF EXISTS context_entries_context_idx;
DROP TABLE IF EXISTS context_entries;
DROP INDEX IF EXISTS contexts_tenant_idx;
DROP TABLE IF EXISTS contexts;
ALTER TABLE projects DROP COLUMN IF EXISTS terraform_version;
