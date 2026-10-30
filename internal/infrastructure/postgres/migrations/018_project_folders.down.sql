ALTER TABLE projects
    ADD COLUMN approval_policy_id text REFERENCES policies (id) ON DELETE SET NULL;

UPDATE projects AS p
SET approval_policy_id = chosen.policy_id
FROM (
    SELECT project_id, min(policy_id) AS policy_id
    FROM project_policies
    GROUP BY project_id
    HAVING count(*) = 1
) AS chosen
WHERE p.id = chosen.project_id;

ALTER TABLE projects DROP COLUMN folder_id;
ALTER TABLE projects DROP COLUMN tags_overridden;

DROP TABLE project_policies;
DROP TABLE folder_policies;
DROP TABLE folders;
