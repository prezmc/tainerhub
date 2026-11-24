-- A renewed agent certificate is first recorded as pending: the agent's
-- current certificate keeps working until the agent connects with the new
-- one, so a renewal reply lost on the way never locks the agent out.
ALTER TABLE hosts ADD COLUMN pending_serial text;
