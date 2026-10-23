-- Silver schema of the synthetic kind, version 5.
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
--   * Rows are append-only: nothing already written changes. A run adds
--     the days after the previous run's as-of and records itself in
--     dump_runs. It also adds each company figure filed since then. Such
--     a row's as_of, its period's last day, can lie before the previous
--     as-of; the run's own window then reaches back to it.

-- Bookkeeping about the silver itself: the schema version and whatever
-- the writer records about how the file was made (for the demo generator:
-- a hash of its code, the seed, input hashes and the as-of the file has
-- reached).
CREATE TABLE meta (
    key   TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
);

-- One row per run. change_number is strictly increasing across runs and
-- is what gold's watermark tracks; [window_start, window_end] (inclusive)
-- bounds every snapshot_at, occurred_at and as_of the run added, and the
-- UTC midnight of every disposal_date.
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

-- The open lots of a positions row: same snapshot_at, account_id and
-- position_key. A lot takes its instrument and currency from that row.
-- lot_key is unique within the position. book_value is the lot's cost,
-- in the convention of the position's book_value; a position with lots
-- has the sum of its lots' costs as its book_value. term is 'short' or
-- 'long' on the snapshot's day, NULL where the writer does not state it.
CREATE TABLE position_lots (
    snapshot_at      INTEGER NOT NULL,
    account_id       TEXT    NOT NULL,
    position_key     TEXT    NOT NULL,
    lot_key          TEXT    NOT NULL,
    quantity         TEXT,
    book_value       TEXT,
    acquisition_date TEXT,              -- YYYY-MM-DD
    term             TEXT,
    payload          TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (snapshot_at, account_id, position_key, lot_key)
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

-- One realized lot as one tax document states it. document_kind is a
-- canonical.RealizedDocKind value (form_1099b, statement, ...). The
-- quantity, proceeds and book_value are magnitudes in currency; the gain
-- is signed, and NULL where the document does not state it.
CREATE TABLE realized_lots (
    realized_lot_id    TEXT    NOT NULL PRIMARY KEY,
    account_id         TEXT    NOT NULL,
    instrument_id      TEXT,
    description        TEXT,
    document_kind      TEXT    NOT NULL,
    tax_year           INTEGER NOT NULL,
    acquisition_date   TEXT,            -- YYYY-MM-DD
    disposal_date      TEXT,            -- YYYY-MM-DD
    currency           TEXT    NOT NULL,
    quantity           TEXT,
    proceeds           TEXT,
    book_value         TEXT,
    realized_gain_loss TEXT,
    term               TEXT,
    payload            TEXT    NOT NULL DEFAULT '{}'
);

-- The reference records: what each fund holds and states of itself,
-- what each security is and reports, and what each pays
-- (canonical/refdata.go). The ten tables repeat the refdata collector's
-- silver shape column for column (collectors/refdata/migrations), so one
-- reader serves both kinds (silver.NewReferenceStream). as_of is the UTC
-- midnight of the day a row describes (for a period fact, its period's
-- last day; for a distribution, the day its source stated it) and must
-- lie inside the window of the run that adds it; run_at is the instant
-- the row was fetched, at or after as_of. Weights are in percent of the
-- fund's net assets.

CREATE TABLE compositions (
    fund_ref           TEXT    NOT NULL,
    as_of              INTEGER NOT NULL,
    basis              TEXT    NOT NULL,
    provider           TEXT    NOT NULL,
    source_kind        TEXT    NOT NULL,
    fund_name          TEXT,
    currency           TEXT,
    net_assets         TEXT,
    line_count         INTEGER NOT NULL,
    weight_sum         TEXT,
    units_outstanding  TEXT,
    run_at             INTEGER NOT NULL,
    payload            TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of, basis)
);

CREATE TABLE lines (
    fund_ref        TEXT    NOT NULL,
    as_of           INTEGER NOT NULL,
    basis           TEXT    NOT NULL,
    line_no         INTEGER NOT NULL,
    isin            TEXT,
    cusip           TEXT,
    figi            TEXT,
    ticker          TEXT,
    exchange        TEXT,
    name            TEXT,
    asset_type      TEXT,
    weight          TEXT,
    quantity        TEXT,
    market_value    TEXT,
    currency        TEXT,
    country         TEXT,
    risk_country    TEXT,
    sector          TEXT,
    industry        TEXT,
    maturity        TEXT,                        -- YYYY-MM-DD
    coupon          TEXT,
    long_currency   TEXT,
    long_amount     TEXT,
    short_currency  TEXT,
    short_amount    TEXT,
    payload         TEXT    NOT NULL DEFAULT '{}',
    coupon_kind       TEXT,
    is_default        INTEGER,
    in_arrears        INTEGER,
    paid_in_kind      INTEGER,
    exchange_rate     TEXT,
    duration          TEXT,
    modified_duration TEXT,
    yield_to_maturity TEXT,
    yield_to_worst    TEXT,
    yield_to_call     TEXT,
    real_duration     TEXT,
    real_yield        TEXT,
    accrual_date      TEXT,
    PRIMARY KEY (fund_ref, as_of, basis, line_no)
);

CREATE TABLE attributes (
    fund_ref         TEXT    NOT NULL,
    as_of            INTEGER NOT NULL,
    domicile         TEXT,
    legal_form       TEXT,
    ucits            INTEGER,                    -- 1, 0, or NULL where unstated
    replication      TEXT,
    distribution     TEXT,
    ter              TEXT,                       -- percent a year
    hedged_currency  TEXT,
    lending          INTEGER,
    index_name       TEXT,
    depositary       TEXT,
    run_at           INTEGER NOT NULL,
    payload          TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of)
);

CREATE TABLE counterparties (
    fund_ref  TEXT    NOT NULL,
    as_of     INTEGER NOT NULL,
    role      TEXT    NOT NULL,
    name      TEXT    NOT NULL,
    lei       TEXT,
    share     TEXT,
    run_at    INTEGER NOT NULL,
    payload   TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of, role, name)
);

CREATE TABLE securities (
    security_key      TEXT    NOT NULL,
    as_of             INTEGER NOT NULL,
    provider          TEXT    NOT NULL,
    isin              TEXT,
    cusip             TEXT,
    figi              TEXT,
    lei               TEXT,
    ticker            TEXT,
    exchange          TEXT,
    name              TEXT,
    security_type     TEXT,
    listing_currency  TEXT,
    run_at            INTEGER NOT NULL,
    payload           TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (security_key, as_of, provider)
);

CREATE TABLE security_facts (
    security_key  TEXT    NOT NULL,
    field         TEXT    NOT NULL,
    as_of         INTEGER NOT NULL,
    provider      TEXT    NOT NULL,
    value         TEXT    NOT NULL,
    scheme        TEXT,
    run_at        INTEGER NOT NULL,
    payload       TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (security_key, field, as_of, provider)
);

CREATE TABLE fund_risk (
    fund_ref        TEXT    NOT NULL,
    as_of           INTEGER NOT NULL,
    currency        TEXT    NOT NULL,
    tenor           TEXT    NOT NULL,
    dv01            TEXT,
    dv100           TEXT,
    credit_ig_dv01  TEXT,
    credit_hy_dv01  TEXT,
    run_at          INTEGER NOT NULL,
    payload         TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of, currency, tenor)
);

CREATE TABLE characteristics (
    fund_ref   TEXT    NOT NULL,
    as_of      INTEGER NOT NULL,
    provider   TEXT    NOT NULL,
    field      TEXT    NOT NULL,
    label      TEXT    NOT NULL DEFAULT '',
    value      TEXT    NOT NULL,
    run_at     INTEGER NOT NULL,
    payload    TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of, provider, field, label)
);

CREATE TABLE period_facts (
    security_key   TEXT    NOT NULL,
    field          TEXT    NOT NULL,
    as_of          INTEGER NOT NULL,
    period_start   TEXT    NOT NULL,
    provider       TEXT    NOT NULL,
    value          TEXT    NOT NULL,
    scheme         TEXT,
    unit           TEXT,
    fiscal_year    INTEGER,
    fiscal_period  TEXT,
    form           TEXT,
    filed          TEXT,
    run_at         INTEGER NOT NULL,
    payload        TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (security_key, field, as_of, period_start, provider)
);

CREATE TABLE dividend_events (
    ref            TEXT    NOT NULL,
    provider       TEXT    NOT NULL,
    event_id       TEXT    NOT NULL,
    as_of          INTEGER NOT NULL,
    kind           TEXT    NOT NULL,
    ex_date        TEXT,                         -- YYYY-MM-DD
    record_date    TEXT,                         -- YYYY-MM-DD
    pay_date       TEXT,                         -- YYYY-MM-DD
    declared_date  TEXT,                         -- YYYY-MM-DD
    amount         TEXT,
    currency       TEXT,
    status         TEXT,
    run_at         INTEGER NOT NULL,
    payload        TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (ref, provider, event_id)
);

-- The published rate series (canonical.RateObservationChange): the fred
-- collector's rates table column for column (collectors/fred/migrations),
-- so one reader serves both kinds (silver.NewRateStream). as_of is the
-- observation day's UTC midnight and must lie inside the window of the
-- run that adds it; value is in percent.
CREATE TABLE rates (
    as_of      INTEGER NOT NULL,
    series_id  TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    payload    TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (as_of, series_id)
);
