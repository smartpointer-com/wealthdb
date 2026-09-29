-- Silver schema of the synthetic kind, version 1.
--
-- The one generic silver: one table per canonical record type, so a row
-- here is the canonical change record it becomes, column for column. No
-- collector writes it; a generator does (demo/generate.py writes it and
-- reads this file to create it, and the adapter tests create their
-- fixtures from it), so this file is the schema's single definition.
--
-- Conventions:
--   * Money, quantities, prices and rates are decimal STRINGS, never REAL.
--   * Timestamps are INTEGER unix seconds UTC. A snapshot is stamped at the
--     UTC midnight of its day and holds that day's closing state.
--   * payload is a JSON object, '{}' when there is nothing to add.
--   * Rows are append-only. A run adds the days after the previous run's
--     as-of and records itself in dump_runs; nothing older is rewritten.

-- Bookkeeping about the silver itself: the schema version and whatever
-- the writer records about how the file was made (for the demo generator:
-- its version, seed, input hashes and the as-of the file has reached).
CREATE TABLE meta (
    key   TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
);

-- One row per run. change_number is strictly increasing across runs and
-- is what gold's watermark tracks; [window_start, window_end] (inclusive)
-- bounds every snapshot_at and occurred_at the run added.
CREATE TABLE dump_runs (
    change_number INTEGER NOT NULL PRIMARY KEY,
    window_start  INTEGER NOT NULL,
    window_end    INTEGER NOT NULL,
    as_of         TEXT    NOT NULL
);

CREATE TABLE portfolios (
    portfolio_id  TEXT NOT NULL PRIMARY KEY,
    display_name  TEXT,
    base_currency TEXT,
    nickname      TEXT,
    payload       TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE accounts (
    account_id       TEXT NOT NULL PRIMARY KEY,
    account_kind     TEXT NOT NULL,
    display_name     TEXT,
    base_currency    TEXT,
    nickname         TEXT,
    account_category TEXT,
    tax_wrapper      TEXT,
    management_style TEXT,
    portfolio_id     TEXT,
    payload          TEXT NOT NULL DEFAULT '{}'
);

-- An instrument may change its descriptive fields over time (a fund
-- renamed, a ticker changed) while keeping its id. Each version applies
-- from valid_from (unix seconds) until the next version's valid_from.
CREATE TABLE instruments (
    instrument_id TEXT    NOT NULL,
    valid_from    INTEGER NOT NULL,
    asset_class   TEXT    NOT NULL,
    vehicle       TEXT    NOT NULL,
    isin          TEXT,
    cusip         TEXT,
    symbol        TEXT,
    name          TEXT,
    currency      TEXT,
    payload       TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (instrument_id, valid_from)
);

CREATE TABLE positions (
    snapshot_at      INTEGER NOT NULL,
    account_id       TEXT    NOT NULL,
    position_key     TEXT    NOT NULL,
    instrument_id    TEXT,
    asset_class      TEXT    NOT NULL,
    vehicle          TEXT    NOT NULL,
    currency         TEXT    NOT NULL,
    quantity         TEXT,
    market_value     TEXT,
    book_value       TEXT,
    accrued_interest TEXT,
    acquisition_date TEXT,              -- YYYY-MM-DD
    payload          TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (snapshot_at, account_id, position_key)
);

CREATE TABLE cash_balances (
    snapshot_at  INTEGER NOT NULL,
    account_id   TEXT    NOT NULL,
    currency     TEXT    NOT NULL,
    balance_kind TEXT    NOT NULL,
    amount       TEXT    NOT NULL,
    payload      TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (snapshot_at, account_id, currency, balance_kind)
);

-- 1 quote_currency = mid_rate base_currency, gold's fx_rates direction.
CREATE TABLE fx_rates (
    snapshot_at    INTEGER NOT NULL,
    base_currency  TEXT    NOT NULL,
    quote_currency TEXT    NOT NULL,
    mid_rate       TEXT    NOT NULL,
    bid_rate       TEXT,
    ask_rate       TEXT,
    payload        TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (snapshot_at, base_currency, quote_currency)
);

-- Amounts carry gold's canonical sign, from the account's own side
-- (canonical/sign.go); the adapter re-applies it as a guard.
CREATE TABLE transactions (
    transaction_id    TEXT    NOT NULL PRIMARY KEY,
    occurred_at       INTEGER NOT NULL,
    account_id        TEXT    NOT NULL,
    instrument_id     TEXT,
    asset_class       TEXT,
    vehicle           TEXT,
    instrument_hint   TEXT,
    kind              TEXT    NOT NULL,
    currency          TEXT    NOT NULL,
    gross_amount      TEXT,
    net_amount        TEXT,
    quantity          TEXT,
    price             TEXT,
    description       TEXT,
    memo              TEXT,
    counterparty      TEXT,
    provider_category TEXT,
    check_number      TEXT,
    payload           TEXT    NOT NULL DEFAULT '{}'
);

CREATE INDEX transactions_by_time ON transactions (occurred_at);
