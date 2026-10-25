ALTER TABLE sessions ADD COLUMN IF NOT EXISTS access_token text NOT NULL DEFAULT '';
DROP INDEX IF EXISTS tenant_memberships_user_idx;
DROP TABLE IF EXISTS tenant_memberships;
ALTER TABLE users ADD COLUMN IF NOT EXISTS github_user_id bigint;
