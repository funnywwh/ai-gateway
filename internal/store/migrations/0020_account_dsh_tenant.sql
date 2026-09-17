-- M52-rev2: the dshgw tenant this account enters once DSH is enabled. The console owns
-- the mapping; the dshgw portal trusts POST /v1/dshgw/authorize to hand it back. Empty
-- while disabled (or while a legacy prefix-bound deployment has not been migrated).
ALTER TABLE accounts ADD COLUMN dsh_tenant TEXT NOT NULL DEFAULT '';
