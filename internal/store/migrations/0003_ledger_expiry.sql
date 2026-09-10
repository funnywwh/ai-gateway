-- Gift credit needs its maturity date on the ledger row that granted it, so the
-- expiry job can find matured grants without a second bookkeeping table. Existing
-- rows keep NULL, which means "never expires".
ALTER TABLE ledger_entries ADD COLUMN expires_at INTEGER;
