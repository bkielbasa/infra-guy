ALTER TABLE runs
    DROP COLUMN IF EXISTS helm_rollback_revision;

DROP TABLE IF EXISTS helm_revisions;
