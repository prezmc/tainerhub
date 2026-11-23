-- Usernames are unique regardless of case ("Bob" and "bob" are one name),
-- so look-alike accounts can't be created. An older install could already
-- hold two names differing only in case; then the index is skipped rather
-- than failing startup, and the check in AcceptInvite still stops new ones.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM users GROUP BY lower(username) HAVING count(*) > 1) THEN
        CREATE UNIQUE INDEX users_username_lower_key ON users (lower(username));
    END IF;
END
$$;
