-- ============================================================
-- schwab-web silver schema, migration 0002
-- — per-statement position snapshots and cash-flow summaries.
--
-- Schwab Trader API coverage starts mid-2024;
-- everything before that has to be reconstructed from monthly /
-- quarterly statement PDFs. Migration 0001 captured the
-- transactions side only. This migration adds the two missing
-- pieces gold needs to build a pre-api position + cash history:
--
--   * historical_position_snapshots — one row per (statement
--     period_end, account, security). Parsed from the
--     "Positions - <SECTION>" blocks in the statement PDF
--     (Equities, ETFs, Mutual Funds, Fixed Income, …).
--
--   * historical_cash_balances     — one row per (statement
--     period_end, account, currency). Parsed from the
--     "Transactions - Summary" header block which carries
--     BeginningCash / EndingCash plus the seven inflow/outflow
--     subtotals (Deposits, Withdrawals, Purchases,
--     Sales/Redemptions, Dividends/Interest, Expenses, plus the
--     "OtherActivity" non-cash row).
--
-- ------------------------------------------------------------
-- Shape decision (Option A: two separate tables)
-- ------------------------------------------------------------
-- The web feed will never carry "live" positions (the api owns
-- live coverage; the web channel only exposes positions at
-- statement period-end). So there is no benefit to one shared
-- `positions` table with a `source` column distinguishing live
-- vs. period-end; both rows in this silver are unambiguously
-- statement-derived. Separate tables also keep the cash-balance
-- columns (currency, debits, credits) from polluting the
-- security-position schema.
--
-- Mirrors ubs-web/migrations/0002 in structure, with
-- Schwab-specific column renames (instrument_key vs.
-- instrument_isin, account suffix vs. portfolio_external_id
-- pair).
--
-- ------------------------------------------------------------
-- sha256-churn handling
-- ------------------------------------------------------------
-- Schwab regenerates the PDF on every download (different
-- generation stamp embedded), so the same logical statement can
-- exist under multiple sha256s in the `documents` table — see
-- INTEROP.md §3. The natural PK below — (period_end, account,
-- instrument_key) or (period_end, account, currency) — collapses
-- those churned re-downloads into a single row. Re-parsing a
-- churned PDF just produces the same row again; the loader uses
-- INSERT OR REPLACE so the latest parse wins (it's
-- deterministic for the same input bytes).
--
-- The `source_sha256` column points at whichever PDF the most
-- recent parse came from, so gold can still trace a row back to
-- its bronze artefact. (Multiple churned copies of the same
-- statement parse identically; the column just doesn't keep all
-- of them.)
--
-- ------------------------------------------------------------
-- NULL preservation
-- ------------------------------------------------------------
-- Every numeric column except the PK members is nullable REAL.
-- Statements drop cost-basis for transferred-in lots, drop
-- accrued-interest for equities, drop unrealized-gain when cost
-- basis is missing — those absences are semantically different
-- from a real zero, and the loader is required to insert NULL
-- (not 0.0) when the parser doesn't see a value.
-- ============================================================

-- One row per (statement period_end, account, security).
-- instrument_key is NOT NULL — cash positions live in
-- historical_cash_balances instead. Schwab statement rows
-- almost always carry the ticker; CUSIP appears only for
-- bonds / options. The parser uses CUSIP when present and
-- falls back to the ticker.
CREATE TABLE historical_position_snapshots (
    as_of_date           INTEGER NOT NULL,           -- statement period_end, Unix sec UTC midnight
    account_external_id  TEXT    NOT NULL,           -- account suffix, e.g. "NNN"
    instrument_key       TEXT    NOT NULL,           -- CUSIP > ticker
    quantity             REAL,
    market_price         REAL,
    market_value         REAL,                       -- USD; account base currency
    cost_basis           REAL,
    unrealized_gain_loss REAL,
    accrued_interest     REAL,                       -- NULL for equities; populated for fixed income
    source_sha256        TEXT    NOT NULL,           -- bronze PDF this row was parsed from
    payload              TEXT    NOT NULL,           -- {section, description, est_yield, est_annual_income, pct_of_acct, raw_line, ...}
    PRIMARY KEY (as_of_date, account_external_id, instrument_key)
);
CREATE INDEX ix_hps_account_date
    ON historical_position_snapshots(account_external_id, as_of_date);
CREATE INDEX ix_hps_instrument
    ON historical_position_snapshots(instrument_key);

-- One row per (statement period_end, account, currency). Schwab
-- accounts are USD-only here, but the
-- currency column is mandatory so the schema is forward-
-- compatible with foreign-currency sweep cash if it ever
-- appears.
CREATE TABLE historical_cash_balances (
    period_end           INTEGER NOT NULL,           -- Unix sec UTC midnight
    period_start         INTEGER NOT NULL,
    account_external_id  TEXT    NOT NULL,
    currency_iso         TEXT    NOT NULL,           -- 'USD' for current footprint
    opening_balance      REAL,                       -- "BeginningCash" row
    closing_balance      REAL,                       -- "EndingCash" row
    total_debits         REAL,                       -- abs(Withdrawals + Purchases + Expenses)
    total_credits        REAL,                       -- Deposits + Sales/Redemptions + Dividends/Interest
    source_sha256        TEXT    NOT NULL,
    payload              TEXT    NOT NULL,           -- {deposits, withdrawals, purchases, sales_redemptions, dividends_interest, expenses, other_activity, raw_line}
    PRIMARY KEY (period_end, account_external_id, currency_iso)
);
CREATE INDEX ix_hcb_account_period
    ON historical_cash_balances(account_external_id, period_end);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s','now') AS INTEGER));
