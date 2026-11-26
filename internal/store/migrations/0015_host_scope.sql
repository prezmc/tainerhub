-- Host-scoped access. A user (or the invite that creates one) may be limited
-- to some hosts: their role then applies on those hosts only. NULL means all
-- hosts, which every existing user keeps.
ALTER TABLE users ADD COLUMN host_scope uuid[];
ALTER TABLE invites ADD COLUMN host_scope uuid[];

-- The hosts an audit entry concerns, so a user limited to some hosts sees
-- only the entries about those hosts. NULL for entries that aren't about a
-- host (sign-ins, settings) and for entries written before this column.
ALTER TABLE audit_log ADD COLUMN host_ids uuid[];
CREATE INDEX audit_log_host_ids ON audit_log USING gin (host_ids);
