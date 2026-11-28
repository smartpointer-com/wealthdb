-- Minimal erstebank_at silver schema for adapter tests: the tables and
-- columns of collectors/erstebank_at/migrations/0001_initial.sql.

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL
);

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE run_accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    account_kind        TEXT    NOT NULL,
    source_id           TEXT,
    product             TEXT,
    display_name        TEXT,
    mask                TEXT,
    currency            TEXT,
    balance             REAL,
    credit_limit        REAL,
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
    balance             REAL,
    balance_relevant    INTEGER NOT NULL DEFAULT 1,
    tx_type             TEXT,
    direction           TEXT,
    description         TEXT,
    counterparty        TEXT,
    reference           TEXT,
    snapshot_at         INTEGER NOT NULL,
    payload             TEXT    NOT NULL
);

CREATE TABLE daily_balances (
    account_external_id TEXT    NOT NULL,
    balance_date        INTEGER NOT NULL,
    balance             REAL    NOT NULL,
    PRIMARY KEY (account_external_id, balance_date)
);

CREATE TABLE statements (
    account_external_id TEXT    NOT NULL,
    statement_id        TEXT    NOT NULL,
    statement_kind      TEXT    NOT NULL,
    period_start        INTEGER,
    period_end          INTEGER,
    statement_date      INTEGER,
    snapshot_at         INTEGER NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (account_external_id, statement_id)
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
