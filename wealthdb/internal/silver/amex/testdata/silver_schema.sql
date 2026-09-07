-- amex silver schema for adapter tests — the tables of
-- collectors/amex/migrations/0001_initial.sql, hand-written here rather than
-- generated from it, column-for-column and in the migration's order, so the
-- tests' positional inserts write the row shape real silver has. Only what no
-- test reads is left out: schema_meta and the indexes.
--
-- `documents` is reproduced although the projection never reads it, and the
-- fixture seeds a row into it: an inventory table that started yielding
-- balance marks or transactions would break the counts the adapter tests
-- assert.
--
-- Silver holds every card figure the PROVIDER's way: the balance is the
-- positive amount owed, and an amount is spend-negative (the collector having
-- negated Amex's own spend-positive figures at load). The adapter negates the
-- balance into gold's canonical negative cash.

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    account_token       TEXT,
    display_name        TEXT,
    mask                TEXT,
    currency            TEXT,
    balance             REAL,
    pending_charges     REAL,
    payment_due_at      INTEGER,
    account_status      TEXT,
    line_of_business    TEXT,
    user_type           TEXT,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

CREATE TABLE transactions (
    txn_id              TEXT    NOT NULL PRIMARY KEY,
    posted_at           INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    amount              REAL    NOT NULL,
    kind                TEXT,
    description         TEXT,
    merchant            TEXT,
    category            TEXT,
    category_code       TEXT,
    txn_date            INTEGER,
    statement_end_at    INTEGER,
    currency            TEXT,
    is_pending          INTEGER NOT NULL DEFAULT 0,
    source              TEXT    NOT NULL,
    payload             TEXT    NOT NULL
);

CREATE TABLE statement_balances (
    account_external_id  TEXT    NOT NULL,
    period_start         INTEGER NOT NULL,
    period_end           INTEGER NOT NULL,
    opening              REAL,
    closing              REAL,
    new_charges          REAL,
    payments_and_credits REAL,
    fees                 REAL,
    interest             REAL,
    transactions_covered INTEGER NOT NULL DEFAULT 0,
    source               TEXT    NOT NULL,
    snapshot_at          INTEGER NOT NULL,
    PRIMARY KEY (account_external_id, period_end)
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
