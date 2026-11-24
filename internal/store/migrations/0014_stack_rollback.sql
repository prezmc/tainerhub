-- Stack rollback. Right before a stack is deployed, pulled & redeployed,
-- updated automatically or rolled back, the hub records which image each of
-- its services runs (a rollback point), keeping the last 5 per stack.
CREATE TABLE stack_rollback_points (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    stack_id     uuid NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    trigger      text NOT NULL CHECK (trigger IN ('deploy', 'pull', 'auto', 'rollback')),
    created_by   text NOT NULL DEFAULT '',
    compose_hash text NOT NULL DEFAULT '',
    services     jsonb NOT NULL DEFAULT '{}' -- service -> {ref, imageId, digest, created}
);
CREATE INDEX stack_rollback_points_stack ON stack_rollback_points (stack_id, created_at DESC);

-- Set while a stack runs a rollback (its override file pins the old
-- images): when the point it went back to was recorded. A deploy clears it.
ALTER TABLE stacks ADD COLUMN rolled_back_to timestamptz;

-- "Roll back automatically if the update fails" (stacks; on by default).
ALTER TABLE auto_update_targets ADD COLUMN auto_rollback boolean NOT NULL DEFAULT true;
