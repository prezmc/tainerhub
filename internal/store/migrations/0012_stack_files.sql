-- A stack's files besides compose.yaml and .env (Dockerfile, scripts,
-- config), as [{"path": ..., "content": ..., "executable": true}]. Kept on
-- the stack's row so they're saved, versioned (updated_at) and hashed for
-- "changes not deployed" together with the compose file.
ALTER TABLE stacks ADD COLUMN files jsonb NOT NULL DEFAULT '[]'::jsonb;
