-- Opt-in automatic updates. A row means "update this automatically in the
-- maintenance window"; turning it off deletes the row. Stacks are tracked
-- by ID (the row goes with the stack); standalone containers by host and
-- name, because a container's ID changes every time it is recreated.
CREATE TABLE auto_update_targets (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind           text NOT NULL CHECK (kind IN ('stack', 'container')),
    stack_id       uuid REFERENCES stacks(id) ON DELETE CASCADE,
    host_id        uuid REFERENCES hosts(id) ON DELETE CASCADE,
    container_name text,
    enabled_by     text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_run_at    timestamptz,
    last_result    text NOT NULL DEFAULT '', -- ok, failed, skipped; '' before the first run
    last_error     text NOT NULL DEFAULT '', -- the error, or why it was skipped
    CHECK (
        (kind = 'stack' AND stack_id IS NOT NULL AND host_id IS NULL AND container_name IS NULL) OR
        (kind = 'container' AND stack_id IS NULL AND host_id IS NOT NULL AND container_name <> '')
    )
);
CREATE UNIQUE INDEX auto_update_targets_stack ON auto_update_targets (stack_id) WHERE kind = 'stack';
CREATE UNIQUE INDEX auto_update_targets_container ON auto_update_targets (host_id, container_name) WHERE kind = 'container';
