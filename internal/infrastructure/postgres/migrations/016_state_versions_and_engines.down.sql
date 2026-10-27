ALTER TABLE projects
    DROP COLUMN IF EXISTS engine_config,
    DROP COLUMN IF EXISTS engine;

DROP INDEX IF EXISTS state_versions_project_serial_idx;
DROP TABLE IF EXISTS state_versions;
