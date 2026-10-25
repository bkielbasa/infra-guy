DROP TABLE IF EXISTS project_cloud_integrations;
DROP INDEX IF EXISTS idx_cloud_integrations_tenant;
DROP TABLE IF EXISTS cloud_integrations;

ALTER TABLE runs
    DROP COLUMN IF EXISTS diff_monthly_cost,
    DROP COLUMN IF EXISTS monthly_cost,
    DROP COLUMN IF EXISTS destroy;

ALTER TABLE projects
    DROP COLUMN IF EXISTS auto_destroy,
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS ttl_hours,
    DROP COLUMN IF EXISTS last_drift_check_at,
    DROP COLUMN IF EXISTS drift_status,
    DROP COLUMN IF EXISTS drift_interval_minutes,
    DROP COLUMN IF EXISTS drift_enabled;
