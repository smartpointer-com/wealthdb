-- Minimal subset of schwab-api's silver schema, sufficient for
-- adapter tests. Mirrors the column types of the upstream
-- migrations/0001_initial.sql but omits PRAGMAs and FK declarations
-- that don't affect what the adapter reads.

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    -- v3 promoted columns. Tolerated optionally by the adapter
    -- (hasColumn checks at query-build time) so older silvers that
    -- predate the schwab-api migration still load.
    account_type        TEXT,
    preference_type     TEXT,
    nickname            TEXT,
    PRIMARY KEY (snapshot_at, account_external_id)
);

-- Populated only when schwab-api runs with --with-instruments.
-- The adapter reads (snapshot_at, symbol) grouped to symbol →
-- latest description for instrument-name enrichment.
CREATE TABLE instruments (
    snapshot_at INTEGER NOT NULL,
    symbol      TEXT    NOT NULL,
    payload     TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, symbol)
);

CREATE TABLE account_balances (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    balance_kind        TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, balance_kind)
);

CREATE TABLE positions (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    instrument_key      TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, instrument_key)
);

CREATE TABLE transactions (
    activity_id         TEXT    NOT NULL PRIMARY KEY,
    timestamp           INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    kind                TEXT    NOT NULL,
    payload             TEXT    NOT NULL
);
