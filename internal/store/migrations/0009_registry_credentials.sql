-- Logins for private container registries. The secret (password or token)
-- is encrypted by the hub with a key kept in its data folder, not here.
CREATE TABLE registry_credentials (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    registry     text NOT NULL UNIQUE, -- normalized host: ghcr.io, docker.io, myreg.lan:5000
    username     text NOT NULL,
    secret       bytea NOT NULL,
    created_by   uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);
