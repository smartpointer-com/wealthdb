-- ============================================================
-- ubs-web silver schema, migration 0002 — historical snapshots
-- reconstructed from PDF documents in the bronze archive.
--
-- Two new tables, both as-of-date-keyed:
--
--   historical_position_snapshots  — semi-annual full-portfolio
--                                    positions reconstructed from
--                                    "Statement of assets" PDFs.
--   historical_cash_balances       — monthly opening/closing
--                                    balances per cash account
--                                    reconstructed from "Account
--                                    Statement" PDFs.
--
-- These live in PARALLEL to the live-fetch `positions` and
-- `accounts` tables; we don't try to merge them because the
-- identity models differ:
--
--   - PDF statements use UBS's `BBB-AAAAAAAA-NN` portfolio numbering
--     (matches PSN's `PrtflId`). Live `positions.csv` uses 4-char
--     portfolio codes (e.g. RNNN, NNNN) — different surface.
--   - PDF snapshots are stable end-of-period bank-of-record. Live
--     snapshots are intra-day customer-side fetches.
--
-- Gold can prefer PDF snapshots for historical dates that pre-date
-- PSN's go-live, and let PSN take over from then on.
-- ============================================================

PRAGMA foreign_keys = ON;


-- Semi-annual full-portfolio snapshot, one row per
-- (as_of_date, portfolio, account or instrument).
--
-- `portfolio_external_id` is the PSN-aligned form
-- 'BBBBAAAAAAAANN' (e.g. 'BBBBAAAAAAAANN01' for portfolio 01)
-- so this table joins directly to PSN's portfolios.
--
-- Mixed identity within one table:
--   - cash positions:    instrument_isin NULL, account_external_id = IBAN
--   - security positions: instrument_isin set, account_external_id = ''
--     (UBS doesn't surface the safekeeping account in the PDF
--     text in a way we can extract reliably)
CREATE TABLE historical_position_snapshots (
    as_of_date               INTEGER NOT NULL,             -- Unix seconds UTC, end-of-period
    portfolio_external_id    TEXT    NOT NULL,             -- 'BBBBAAAAAAAANN01' etc.
    account_external_id      TEXT    NOT NULL,             -- IBAN no-spaces for cash; '' for securities
    instrument_isin          TEXT,                         -- NULL for cash
    currency_iso             TEXT,                         -- instrument currency
    units                    REAL,                         -- shares, units, or currency amount
    market_value             REAL,                         -- in `market_value_currency`
    market_value_currency    TEXT,                         -- portfolio base currency
    cost_price               REAL,                         -- per-unit cost basis
    market_price             REAL,                         -- per-unit market price
    accrued_interest         REAL,
    exchange_rate_to_base    REAL,                         -- FX rate for non-base-ccy positions
    description              TEXT,                         -- e.g. 'Reg.shs Example Equity AG (XMPL)'
    sector                   TEXT,
    source_doc_token         TEXT    NOT NULL,             -- FK to documents.doc_token
    payload                  TEXT    NOT NULL,             -- raw parsed text block
    PRIMARY KEY (as_of_date, portfolio_external_id,
                 account_external_id, instrument_isin)
);

CREATE INDEX ix_hist_pos_date         ON historical_position_snapshots(as_of_date);
CREATE INDEX ix_hist_pos_isin         ON historical_position_snapshots(instrument_isin);
CREATE INDEX ix_hist_pos_account      ON historical_position_snapshots(account_external_id);


-- Monthly cash-balance snapshot per cash account, one row per
-- (period_end, account). Each "Account Statement" PDF covers
-- one calendar month for one account; we extract the opening
-- balance, closing balance, and turnover totals.
CREATE TABLE historical_cash_balances (
    period_end               INTEGER NOT NULL,             -- Unix seconds UTC, last day of period
    account_external_id      TEXT    NOT NULL,             -- IBAN no-spaces uppercase
    currency_iso             TEXT    NOT NULL,
    period_start             INTEGER NOT NULL,             -- Unix seconds UTC, first day of period
    opening_balance          REAL,
    closing_balance          REAL,
    total_debits             REAL,
    total_credits            REAL,
    source_doc_token         TEXT    NOT NULL,
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (period_end, account_external_id)
);

CREATE INDEX ix_hist_cash_account_period
    ON historical_cash_balances(account_external_id, period_end);


INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s', 'now') AS INTEGER));
