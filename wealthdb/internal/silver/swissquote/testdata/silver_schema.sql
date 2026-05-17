-- Minimal subset of swissquote-dump's silver schema for adapter
-- tests. Mirrors the column types of the upstream migration but
-- omits PRAGMAs and the documents table the adapter doesn't read.

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    -- swissquote-dump v2 promoted column. Tolerated as optional by
    -- the adapter (hasColumn check at query-build time) so older
    -- silvers still load.
    account_type        TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (snapshot_at, account_external_id)
);

CREATE TABLE positions (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    symbol              TEXT    NOT NULL,
    currency            TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, symbol, currency)
);

CREATE TABLE currency_balances (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    currency            TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, currency)
);

CREATE TABLE transactions (
    account_external_id TEXT    NOT NULL,
    occurred_at         INTEGER NOT NULL,
    transaction_type    TEXT    NOT NULL,
    order_num           TEXT,
    isin                TEXT,
    symbol              TEXT,
    currency            TEXT    NOT NULL,
    net_amount          REAL    NOT NULL,
    payload             TEXT    NOT NULL
);
