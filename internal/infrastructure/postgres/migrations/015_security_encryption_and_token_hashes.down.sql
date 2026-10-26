ALTER TABLE projects
    DROP COLUMN IF EXISTS state_token_ciphertext,
    DROP COLUMN IF EXISTS state_token_hash;

ALTER TABLE context_entries
    DROP CONSTRAINT IF EXISTS context_entries_secret_no_plaintext_chk;
