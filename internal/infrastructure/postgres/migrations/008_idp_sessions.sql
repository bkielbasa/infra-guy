INSERT INTO identities (user_id, provider, issuer, subject)
SELECT id, 'github', 'https://github.com', github_user_id::text
FROM users
ON CONFLICT (provider, issuer, subject) DO NOTHING;

ALTER TABLE users DROP COLUMN github_user_id;

CREATE TABLE tenant_memberships (
    tenant_id text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, user_id)
);

CREATE INDEX tenant_memberships_user_idx ON tenant_memberships (user_id);

INSERT INTO tenant_memberships (tenant_id, user_id, role, created_at, updated_at)
SELECT DISTINCT tenant_id, user_id, role, now(), now()
FROM sessions
WHERE tenant_id IS NOT NULL AND tenant_id <> '' AND role <> ''
ON CONFLICT (tenant_id, user_id) DO NOTHING;

ALTER TABLE sessions DROP COLUMN access_token;
