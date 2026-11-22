-- Password sign-in, so a fresh install works over plain HTTP before HTTPS
-- and passkeys are set up. Empty means the user has no password.
ALTER TABLE users ADD COLUMN password_hash text NOT NULL DEFAULT '';
