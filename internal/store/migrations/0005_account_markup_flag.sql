-- The account-level multiplier override needs to be distinguishable from "not set"
-- (0 is a meaningful override: free usage). Accounting for that with a flag column
-- keeps the pricing precedence chain honest instead of guessing from the value.
ALTER TABLE accounts ADD COLUMN markup_override_set INTEGER NOT NULL DEFAULT 0;
