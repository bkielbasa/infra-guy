UPDATE context_entries
SET value_text = ''
WHERE kind <> 'plain_env' AND value_text <> '';

ALTER TABLE context_entries
    DROP CONSTRAINT IF EXISTS context_entries_secret_no_plaintext_chk,
    ADD CONSTRAINT context_entries_secret_no_plaintext_chk
        CHECK (kind = 'plain_env' OR value_text = '');

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS state_token_hash text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS state_token_ciphertext bytea;

UPDATE projects
SET state_token_hash = 'sha256:' || encode(sha256(convert_to(state_token, 'UTF8')), 'hex')
WHERE state_token <> '' AND state_token_hash = '';

UPDATE sessions
SET id = 'sha256:' || encode(sha256(convert_to(id, 'UTF8')), 'hex')
WHERE id NOT LIKE 'sha256:%';
