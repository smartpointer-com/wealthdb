-- Minimal raiffeisen_at silver schema for adapter tests — mirrors the columns
-- the adapter reads in collectors/raiffeisen_at/migrations/0001_initial.sql.

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
    PRIMARY KEY (snapshot_at, account_external_id)
);

CREATE TABLE transactions (
    txn_id              TEXT    NOT NULL PRIMARY KEY,
    account_external_id TEXT    NOT NULL,
    posted_at           INTEGER NOT NULL,
    value_at            INTEGER,
    amount              REAL    NOT NULL,
    currency            TEXT,
    kind                TEXT,
    category            TEXT,
    description         TEXT,
    counterparty        TEXT,
    source              TEXT    NOT NULL,
    payload             TEXT    NOT NULL
);

CREATE TABLE daily_balances (
    account_external_id TEXT    NOT NULL,
    balance_date        INTEGER NOT NULL,
    balance             REAL    NOT NULL,
    snapshot_at         INTEGER NOT NULL,
    PRIMARY KEY (account_external_id, balance_date)
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
