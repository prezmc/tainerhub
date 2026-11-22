-- TainerHub schema v1 (phase 1: auth, hosts, audit)

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username      text NOT NULL UNIQUE,
    display_name  text NOT NULL,
    role          text NOT NULL CHECK (role IN ('owner', 'admin', 'operator', 'viewer')),
    webauthn_id   bytea NOT NULL UNIQUE,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE passkeys (
    id            bytea PRIMARY KEY,               -- WebAuthn credential ID
    user_id       uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    name          text NOT NULL,
    credential    jsonb NOT NULL,                  -- go-webauthn Credential
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz
);
CREATE INDEX passkeys_user ON passkeys (user_id);

CREATE TABLE recovery_codes (
    id         bigserial PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    code_hash  bytea NOT NULL,
    used_at    timestamptz
);
CREATE INDEX recovery_codes_user ON recovery_codes (user_id);

CREATE TABLE sessions (
    token_hash    bytea PRIMARY KEY,               -- SHA-256 of the cookie value
    user_id       uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    ip            text NOT NULL DEFAULT '',
    user_agent    text NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE hosts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    enrolled_at   timestamptz,
    cert_serial   text NOT NULL DEFAULT '',
    last_seen_at  timestamptz,
    info          jsonb                            -- last Hello from the agent
);

CREATE TABLE join_tokens (
    id           text PRIMARY KEY,
    host_id      uuid NOT NULL REFERENCES hosts ON DELETE CASCADE,
    secret_hash  bytea NOT NULL,
    expires_at   timestamptz NOT NULL,
    used_at      timestamptz
);

CREATE TABLE audit_log (
    id       bigserial PRIMARY KEY,
    at       timestamptz NOT NULL DEFAULT now(),
    user_id  uuid,
    actor    text NOT NULL,
    action   text NOT NULL,
    target   text NOT NULL DEFAULT '',
    detail   jsonb,
    ip       text NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_at ON audit_log (at DESC);
