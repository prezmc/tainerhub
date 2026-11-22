-- One-time invitations: an admin creates one with a role; whoever opens the
-- link picks a username and password and joins with that role.
CREATE TABLE invites (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash  bytea NOT NULL UNIQUE,              -- SHA-256 of the link's token
    role        text NOT NULL CHECK (role IN ('admin', 'operator', 'viewer')),
    note        text NOT NULL DEFAULT '',           -- who it's for, shown to admins
    created_by  uuid REFERENCES users ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    used_by     uuid REFERENCES users ON DELETE SET NULL
);
