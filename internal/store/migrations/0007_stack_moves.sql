-- Moves of a stack from one host to another. A finished move leaves the old
-- copy's data (volumes, stack folder) on the source host until an admin
-- deletes it; leftovers records what and where.
CREATE TABLE stack_moves (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    stack_id              uuid REFERENCES stacks ON DELETE SET NULL,
    stack_name            text NOT NULL,
    from_host             uuid NOT NULL REFERENCES hosts ON DELETE CASCADE,
    to_host               uuid NOT NULL REFERENCES hosts ON DELETE CASCADE,
    state                 text NOT NULL CHECK (state IN ('running', 'done', 'failed', 'interrupted')),
    error                 text NOT NULL DEFAULT '',
    bytes                 bigint NOT NULL DEFAULT 0,   -- data copied
    leftovers             jsonb,                       -- {"host_id", "volumes": [...], "dir": "..."} still on a host
    leftovers_removed_at  timestamptz,
    started_by            text NOT NULL,
    started_at            timestamptz NOT NULL DEFAULT now(),
    finished_at           timestamptz
);
CREATE INDEX stack_moves_stack ON stack_moves (stack_id, started_at DESC);
-- At most one move of a stack at a time.
CREATE UNIQUE INDEX stack_moves_one_running ON stack_moves (stack_id) WHERE state = 'running';
