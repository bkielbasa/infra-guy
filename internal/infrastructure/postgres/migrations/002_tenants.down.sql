DROP INDEX IF EXISTS projects_tenant_repo;
ALTER TABLE projects
    DROP COLUMN IF EXISTS github_repo_id,
    DROP COLUMN IF EXISTS tenant_id;
DROP TABLE IF EXISTS oauth_states;
DROP TABLE IF EXISTS sessions;
DROP INDEX IF EXISTS installations_one_active_per_tenant;
DROP TABLE IF EXISTS installations;
DROP TABLE IF EXISTS identities;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;
