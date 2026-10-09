-- Minimal subset of schwab-web's silver schema, sufficient for the
-- merged-adapter tests. Mirrors the column types of the upstream
-- migrations (0001, 0002, 0004, 0006, 0007) and omits what the adapter
-- does not read.

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    nickname            TEXT,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

CREATE TABLE transactions (
    activity_id         TEXT    NOT NULL PRIMARY KEY,
    timestamp           INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    kind                TEXT    NOT NULL,
    instrument_key      TEXT,
    source              TEXT    NOT NULL,
    source_sha256       TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    logical_doc_key     TEXT
);

CREATE TABLE historical_position_snapshots (
    as_of_date           INTEGER NOT NULL,
    account_external_id  TEXT    NOT NULL,
    instrument_key       TEXT    NOT NULL,
    quantity             REAL,
    market_price         REAL,
    market_value         REAL,
    cost_basis           REAL,
    unrealized_gain_loss REAL,
    accrued_interest     REAL,
    source_sha256        TEXT    NOT NULL,
    payload              TEXT    NOT NULL,
    PRIMARY KEY (as_of_date, account_external_id, instrument_key)
);

CREATE TABLE historical_cash_balances (
    period_end           INTEGER NOT NULL,
    period_start         INTEGER NOT NULL,
    account_external_id  TEXT    NOT NULL,
    currency_iso         TEXT    NOT NULL,
    opening_balance      REAL,
    closing_balance      REAL,
    total_debits         REAL,
    total_credits        REAL,
    source_sha256        TEXT    NOT NULL,
    payload              TEXT    NOT NULL,
    PRIMARY KEY (period_end, account_external_id, currency_iso)
);

CREATE TABLE open_lots (
    as_of_date           INTEGER NOT NULL,
    account_external_id  TEXT    NOT NULL,
    instrument_key       TEXT    NOT NULL,
    lot_index            INTEGER NOT NULL,
    quantity             REAL,
    unit_cost            REAL,
    cost_basis           REAL,
    acquired_date        TEXT,
    unrealized_gain_loss REAL,
    term                 TEXT,
    covered              INTEGER,
    footnotes            TEXT,
    source_sha256        TEXT    NOT NULL,
    payload              TEXT    NOT NULL,
    PRIMARY KEY (as_of_date, account_external_id, instrument_key, lot_index)
);

CREATE TABLE closed_lots (
    logical_doc_key          TEXT    NOT NULL,
    document_kind            TEXT    NOT NULL,
    lot_index                INTEGER NOT NULL,
    account_external_id      TEXT    NOT NULL,
    tax_year                 INTEGER,
    security_name            TEXT,
    cusip                    TEXT,
    instrument_key           TEXT,
    quantity                 REAL,
    acquired_date            TEXT,
    disposed_date            TEXT,
    proceeds                 REAL,
    cost_basis               REAL,
    wash_sale_disallowed     REAL,
    accrued_market_discount  REAL,
    realized_gain_loss       REAL,
    term                     TEXT,
    covered                  INTEGER,
    form_8949_box            TEXT,
    footnotes                TEXT,
    source_sha256            TEXT    NOT NULL,
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (logical_doc_key, document_kind, lot_index)
);
