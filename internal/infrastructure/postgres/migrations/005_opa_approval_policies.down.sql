ALTER TABLE runs DROP COLUMN IF EXISTS plan_json;
ALTER TABLE projects DROP COLUMN IF EXISTS approval_policy_id;
DROP INDEX IF EXISTS idx_policies_tenant_id;
DROP TABLE IF EXISTS policies;
