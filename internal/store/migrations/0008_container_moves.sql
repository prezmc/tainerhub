-- Moves of a single standalone container share the stack_moves table:
-- kind says which, and stack_name holds the container's name (stack_id is
-- NULL). Leftovers may also name the container itself.
ALTER TABLE stack_moves ADD COLUMN kind text NOT NULL DEFAULT 'stack' CHECK (kind IN ('stack', 'container'));
-- At most one move of a container (by host and name) at a time.
CREATE UNIQUE INDEX stack_moves_one_container ON stack_moves (from_host, stack_name) WHERE state = 'running' AND kind = 'container';
CREATE INDEX stack_moves_containers ON stack_moves (started_at DESC) WHERE kind = 'container';
