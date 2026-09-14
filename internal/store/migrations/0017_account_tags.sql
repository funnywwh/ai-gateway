-- Account-level tag names are inherited by every API key in the account.
ALTER TABLE accounts ADD COLUMN tags_json TEXT NOT NULL DEFAULT '';
