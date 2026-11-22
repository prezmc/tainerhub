-- Compose stacks managed by TainerHub. The hub keeps the definitions; agents
-- write them to the host when a stack is deployed.

CREATE TABLE stacks (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id        uuid NOT NULL REFERENCES hosts ON DELETE CASCADE,
    name           text NOT NULL,                   -- compose project name
    compose        text NOT NULL,
    env            text NOT NULL DEFAULT '',        -- contents of .env
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    updated_by     text NOT NULL DEFAULT '',
    deployed_at    timestamptz,                     -- last successful deploy
    deployed_hash  text NOT NULL DEFAULT '',        -- hash of compose + env at that deploy
    UNIQUE (host_id, name)
);
