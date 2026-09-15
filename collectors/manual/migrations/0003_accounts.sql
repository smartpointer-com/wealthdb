-- ============================================================
-- ACCOUNTS — the pseudo-accounts a manual position is held in
-- ============================================================
-- Until now every manual position landed in one hard-coded gold account
-- (`manual`, kind `other`, taxable_personal / self_directed). That is right
-- for a book of directly-held assets with one owner, and wrong the moment two
-- of them sit in different TAX SLEEVES — a holding inside a trust or a company
-- is not the holder's own taxable property, and rolling both into one account
-- makes every wrapper-grained report wrong.
--
-- An account here is a declaration, not something fetched: it says "these
-- positions are held under this wrapper, managed this way". It buys the sleeve
-- split without a second silver source per sleeve, which was the alternative
-- and costs a directory, a silver DB, a config entry and an account_overrides
-- entry each time.
--
-- `account_kind`, `tax_wrapper` and `management_style` are the canonical gold
-- vocabularies (wealthdb internal/canonical/enums.go), validated by load.py so
-- a typo fails at load rather than silently landing an unknown value in gold.
-- The two nullable ones fall back to the same defaults the single account
-- always had, so a file that sets neither behaves exactly as before.
CREATE TABLE accounts (
    id                TEXT NOT NULL PRIMARY KEY,   -- user-assigned stable id
    display_name      TEXT NOT NULL,               -- PII: synthetic in tracked files
    account_kind      TEXT NOT NULL,               -- canonical account_kind
    tax_wrapper       TEXT,                        -- canonical tax_wrapper; NULL → taxable_personal
    management_style  TEXT,                        -- canonical management_style; NULL → self_directed
    notes             TEXT,
    payload           TEXT NOT NULL                -- JSON object
);

-- Which account holds the position. Nullable in the SCHEMA so the migration
-- applies to a silver that predates it; load.py requires it to resolve, and
-- defaults an omitted CSV column to the account every position used to be in.
ALTER TABLE positions ADD COLUMN account_id TEXT;
CREATE INDEX ix_positions_account ON positions(account_id);

-- Backfill: everything already loaded belongs to the account it was always
-- projected into. The loader truncates and rebuilds from the CSVs on every
-- run, so this only matters for a DB read between this migration and the next
-- load — but that window is exactly when a NULL would reach gold.
INSERT INTO accounts (id, display_name, account_kind, tax_wrapper,
                      management_style, notes, payload)
    VALUES ('manual', 'Manual', 'other', NULL, NULL, NULL, '{}');
UPDATE positions SET account_id = 'manual' WHERE account_id IS NULL;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s', 'now') AS INTEGER));
