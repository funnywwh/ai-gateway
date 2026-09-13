-- The effective reasoning effort is execution metadata, not recorded request content:
-- it is captured after the canonical model policy is applied to the provider request.
-- It remains available when record_input=off. Empty means no effort was applied (or an
-- older/local-rejected row, which never reached routing and an upstream request).
ALTER TABLE request_logs ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT '';
