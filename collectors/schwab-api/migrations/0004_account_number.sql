-- ============================================================
-- schwab-api-dump silver schema, migration 0004 — promote
-- account_number on accounts.
--
-- The accountNumber from /accounts/accountNumbers is already stored
-- inside accounts.payload (added in migration 0001 / refined in 0003).
-- Promoting it as a real column lets downstream consumers — notably
-- the wealthdb gold layer's bridge against schwab-web-dump (see
-- INTEROP.md §1) — join on the plaintext account number directly,
-- without a json_extract per row.
--
-- Existing rows are left NULL; the next dump load fills the column
-- via load_accounts (which already has the value in-hand from the
-- merged per-account dict).
-- ============================================================

ALTER TABLE accounts ADD COLUMN account_number TEXT;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s','now') AS INTEGER));
