-- ============================================================
-- relevate silver schema, migration 0002 — historical snapshots
-- reconstructed from Quartalsbericht PDFs in the bronze archive.
--
-- Two new tables, both end-of-quarter-keyed:
--
--   historical_position_snapshots  — per-security holdings
--                                    (ISIN, units, market value)
--                                    from the "Portfolio Detail"
--                                    section of each quarterly
--                                    report PDF.
--   historical_cash_balances       — Liquidity row from the same
--                                    section; same snapshot_at as
--                                    the position rows in that PDF.
--
-- These live in PARALLEL to the live-fetch `positions` and
-- `cash_balances` tables; the identity models differ:
--
--   - PDF snapshots are stable end-of-quarter bank-of-record
--     values: holder's ACTUAL units held, with market value in
--     CHF. The live `positions` table holds the model portfolio's
--     TARGET ALLOCATION (a 0..1 fraction) — distinct semantics.
--   - PDFs key the position row on ISIN; the live table keys on
--     Relevate's internal `security.id` (a numeric string the
--     PDF never surfaces).
--
-- The Go adapter (snapshots.go) UNIONs `snapshot_at` from both
-- live and historical tables in its time-dispatch query, then
-- emits one canonical `SnapshotBatch` per distinct snapshot_at.
-- For dates that have only historical coverage (every quarter-end
-- before 2026-05-27), this is the only source of position +
-- cash data — and gold's `report_positions(p_asof)` /
-- `report_portfolios(p_asof)` will pick the latest snapshot_at
-- <= p_asof per silver_source, which now extends back to Q2 2025.
-- ============================================================

PRAGMA foreign_keys = ON;

BEGIN;


-- One row per (snapshot_at, account, security) from the
-- Portfolio Detail section of one Quartalsbericht.
--
-- snapshot_at is the PDF's stated "Valuation date" parsed as
-- midnight UTC at the quarter-end.
--
-- ISIN is part of the PK because every observed Portfolio Detail
-- position row carries one. If a future Relevate strategy
-- introduces an ISIN-less security row, the loader logs + skips
-- it (alternative would be to widen the PK with security_name,
-- but that's noisier than the corpus warrants today).
CREATE TABLE historical_position_snapshots (
    snapshot_at           INTEGER NOT NULL,                 -- Unix seconds UTC, quarter-end midnight
    account_external_id   TEXT    NOT NULL,                 -- NNNN.NNNNNN.N from the PDF's 'Reference no.'
    isin                  TEXT    NOT NULL,                 -- ISO 6166, 12 chars
    security_name         TEXT    NOT NULL,                 -- as printed in the Security column
    asset_class           TEXT,                              -- 'Stocks' | 'Bonds' | 'Real Estate' | 'Alternative' | 'Liquidity'
    currency              TEXT    NOT NULL,                 -- ISO 4217. Tracks `market_value`'s denomination, NOT the instrument's trading currency. Relevate's 'Allocation in CHF' column is always the portfolio reference (CHF) regardless of the instrument trading USD/EUR/etc.
    units                 REAL,                              -- shares/units held; NULL allowed but always present in observed data
    allocation_pct        REAL,                              -- 0..1 fraction (PDF prints as percentage; loader divides by 100)
    market_value          REAL    NOT NULL,                 -- in `currency`; from the 'Allocation in CHF' column
    document_id           INTEGER,                           -- documents.relevate_doc_id this row came from
    source_sha256         TEXT    NOT NULL,                 -- sha256 of the source PDF (= documents.content_sha256)
    payload               TEXT    NOT NULL,                 -- raw parsed row dict (includes the instrument's native trading currency)
    PRIMARY KEY (snapshot_at, account_external_id, isin)
);

CREATE INDEX ix_hist_pos_account
    ON historical_position_snapshots(account_external_id);
CREATE INDEX ix_hist_pos_isin
    ON historical_position_snapshots(isin);
CREATE INDEX ix_hist_pos_doc
    ON historical_position_snapshots(document_id);


-- The Portfolio Detail section's 'Liquidity' row (and any
-- 'Accrued interest' row) parsed as cash, one entry per
-- (snapshot_at, account, currency, balance_kind).
--
-- balance_kind values observed:
--   'cash'              — 'Liquidity CHF' row
--   'accrued_interest'  — 'Accrued interest' row (when present)
--
-- The kinds intentionally MATCH the live `cash_balances`
-- table's vocabulary for 'cash', so adapter code that filters
-- on balance_kind='cash' picks up both live and historical rows
-- uniformly.
CREATE TABLE historical_cash_balances (
    snapshot_at           INTEGER NOT NULL,
    account_external_id   TEXT    NOT NULL,
    currency              TEXT    NOT NULL,                 -- ISO 4217
    balance_kind          TEXT    NOT NULL,                 -- 'cash' | 'accrued_interest'
    amount                REAL    NOT NULL,                 -- in `currency` (CHF)
    document_id           INTEGER,
    source_sha256         TEXT    NOT NULL,
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, currency, balance_kind)
);

CREATE INDEX ix_hist_cash_account
    ON historical_cash_balances(account_external_id);
CREATE INDEX ix_hist_cash_doc
    ON historical_cash_balances(document_id);


INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
