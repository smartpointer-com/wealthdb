-- ============================================================
-- ubs-web silver schema, migration 0005 — historical mortgages.
--
-- The live `mortgages` table (migration 0004) captures the current
-- snapshot of open mortgages from positions.csv. To
-- reconstruct the historical lifecycle of mortgages that pre-date
-- the first live dump (or were paid off before then), we parse the
-- per-mortgage "Maturity notice" PDFs in the documents archive.
--
-- Each Maturity notice PDF documents one mortgage at one quarterly
-- settlement date and carries:
--   - the UBS-internal mortgage account no. (`BBBB AAAAAAAA.MMM
--     NNNN`),
--   - the product line ('UBS SARON Mortgage CHF', 'UBS Fixed-Rate
--     Mortgage CHF', …) which yields the rate type and currency,
--   - the collateral property line,
--   - "As at DD.MM.YYYY" — the balance date,
--   - "Current debt capital N NNN NNN.NN" — the outstanding
--     principal at that date (positive in the source, negated to a
--     liability sign on insert).
--
-- One row per (as_of_date, account_external_id). Quarterly cadence
-- is typical; UBS issues a Maturity notice for each fixed-rate /
-- SARON interest-roll period.
--
-- Gold projects these into the canonical positions stream via the
-- UBS adapter's historical path (same shape as the live mortgage
-- emitter, just keyed by as_of_date instead of snapshot_at).
-- ============================================================

PRAGMA foreign_keys = ON;

CREATE TABLE historical_mortgages (
    as_of_date              INTEGER NOT NULL,        -- Unix seconds UTC
    account_external_id     TEXT    NOT NULL,        -- UBS mortgage no.
    currency_iso            TEXT    NOT NULL,
    outstanding_balance     REAL,                    -- negative (liability)
    product_name            TEXT,                    -- 'UBS SARON Mortgage', …
    rate_type               TEXT,                    -- 'fixed'|'variable'|NULL (SARON maps to 'variable')
    collateral_description  TEXT,
    source_doc_token        TEXT    NOT NULL,
    payload                 TEXT    NOT NULL,
    PRIMARY KEY (as_of_date, account_external_id)
);

CREATE INDEX ix_hist_mortgages_account
    ON historical_mortgages(account_external_id);


INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (5, CAST(strftime('%s', 'now') AS INTEGER));
