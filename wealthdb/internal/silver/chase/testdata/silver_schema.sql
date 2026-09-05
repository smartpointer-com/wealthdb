-- Minimal chase silver schema for adapter tests — mirrors the columns the
-- adapter reads at silver schema 3 (collectors/chase/migrations/
-- 0001_initial.sql + 0002_cards.sql + 0003_statement_coverage.sql), with the
-- migrations' additions folded into the table definitions.

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL
);

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    account_type        TEXT,
    nickname            TEXT,
    mask                TEXT,
    currency            TEXT,
    balance             REAL,
    payload             TEXT    NOT NULL,
    product             TEXT    NOT NULL DEFAULT 'dda',   -- 'dda' | 'card'
    pending_charges     REAL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

CREATE TABLE transactions (
    fitid               TEXT    NOT NULL PRIMARY KEY,
    posted_at           INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    amount              REAL    NOT NULL,
    kind                TEXT,
    description         TEXT,
    check_number        TEXT,
    balance             REAL,
    source              TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    txn_date            INTEGER,
    merchant            TEXT,
    category            TEXT,
    currency            TEXT
);

CREATE TABLE documents (
    sha256              TEXT    NOT NULL PRIMARY KEY,
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    doc_date            INTEGER,
    doc_kind            TEXT    NOT NULL,
    file_format         TEXT    NOT NULL,
    filename            TEXT    NOT NULL,
    size_bytes          INTEGER NOT NULL,
    payload             TEXT    NOT NULL
);

CREATE TABLE statement_balances (
    account_external_id  TEXT    NOT NULL,
    period_start         INTEGER NOT NULL,
    period_end           INTEGER NOT NULL,
    opening              REAL,
    closing              REAL,
    snapshot_at          INTEGER NOT NULL,
    transactions_covered INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (account_external_id, period_end)
);
