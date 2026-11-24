-- API tokens: long-lived bearer credentials for scripts and tools. Only a
-- SHA-256 hash of the token is kept; the token itself is shown once. A
-- token acts with the lower of its own role and its user's current role.
CREATE TABLE api_tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    name          text NOT NULL,
    prefix        text NOT NULL,                    -- first 8 characters after "thp_", for display
    token_hash    bytea NOT NULL UNIQUE,            -- SHA-256 of the whole token
    role          text NOT NULL CHECK (role IN ('viewer', 'operator', 'admin', 'owner')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz,                      -- NULL: never expires
    last_used_at  timestamptz,
    last_used_ip  text NOT NULL DEFAULT ''
);
CREATE INDEX api_tokens_user ON api_tokens (user_id);
