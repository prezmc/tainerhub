-- Settings changed from the UI (e.g. the HTTPS address). Environment
-- variables, when set, take precedence.
CREATE TABLE settings (
    key         text PRIMARY KEY,
    value       text NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);
