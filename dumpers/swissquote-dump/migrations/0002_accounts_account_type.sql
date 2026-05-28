-- ============================================================
-- migration 0002 — accounts: add account_type column.
--
-- Swissquote labels each account in the eBanking #accountOverview
-- listing as `<TYPE> <CUSTOMER_ID>` (e.g. "Trading 1234567",
-- "Saving plan for investors 1234567", "Invest Easy 1234568").
-- The type is informally exposed only on that listing — not in any
-- of the CSV/XLS exports — but it is the closest thing to a
-- structured "account kind" Swissquote gives retail customers.
--
-- download.py scrapes the listing into a new bronze artefact
-- (accounts.json); load.py reads it and populates this column.
--
-- Backwards-compatible: NOT NULL with default '' so existing rows
-- (loaded from pre-accounts.json bronze dumps) are valid. The PK
-- (snapshot_at, account_external_id) is unchanged — for the
-- single-account-per-customer case it remains unique. If
-- multi-account customers ever appear (e.g. Trading + Savings
-- under one customer ID), we'll add account_type to the PK in a
-- follow-up migration.
-- ============================================================

ALTER TABLE accounts
    ADD COLUMN account_type TEXT NOT NULL DEFAULT '';

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s', 'now') AS INTEGER));
