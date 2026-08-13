-- Minimal chase silver schema for adapter tests — mirrors the columns the
-- adapter reads in collectors/chase/migrations/0001_initial.sql.

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
    fitid               TEXT    NOT NULL PRIMARY KEY,
    posted_at           INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    amount              REAL    NOT NULL,
    kind                TEXT,
    description         TEXT,
    check_number        TEXT,
    balance             REAL,
    source              TEXT    NOT NULL,
    payload             TEXT    NOT NULL
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
