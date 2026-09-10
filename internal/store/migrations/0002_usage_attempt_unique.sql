-- The settlement primitive relies on (request_id, attempt_no) identifying one metered
-- attempt: without this uniqueness a replay after a crash would insert a second usage
-- row, and the invariant sum(charge ledger) == sum(usage.charge_micros) would break.
CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_attempt ON usage_records(request_id, attempt_no);
