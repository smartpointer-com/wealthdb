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
    -- v6 promoted columns, filled from payload by the trigger below.
    asset_type          TEXT,
    instrument_type     TEXT,
    cusip               TEXT,
    symbol              TEXT,
    description         TEXT,
    maturity_date       INTEGER,
    variable_rate       TEXT,
    long_quantity       TEXT,
    short_quantity      TEXT,
    market_value        TEXT,
    PRIMARY KEY (snapshot_at, account_external_id, instrument_key)
);

-- A fixture states a holding once, in its payload. The trigger fills
-- the v6 columns from it: a code's text, a number's JSON literal (no
-- fixture states one with an exponent, which the collector writes out),
-- the day a maturity stamp names at 00:00 UTC.
CREATE TRIGGER positions_promote AFTER INSERT ON positions BEGIN
    UPDATE positions SET
        asset_type = CASE WHEN json_type(payload, '$.instrument.assetType') = 'text'
            THEN json_extract(payload, '$.instrument.assetType') END,
        instrument_type = CASE WHEN json_type(payload, '$.instrument.type') = 'text'
            THEN json_extract(payload, '$.instrument.type') END,
        cusip = CASE WHEN json_type(payload, '$.instrument.cusip') = 'text'
            THEN json_extract(payload, '$.instrument.cusip') END,
        symbol = CASE WHEN json_type(payload, '$.instrument.symbol') = 'text'
            THEN json_extract(payload, '$.instrument.symbol') END,
        description = CASE WHEN json_type(payload, '$.instrument.description') = 'text'
            THEN json_extract(payload, '$.instrument.description') END,
        maturity_date = (SELECT CASE
            WHEN d GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'
             AND substr(d, 1, 4) <> '0000' AND date(d) = d
            THEN CAST(strftime('%s', d) AS INTEGER) END
            FROM (SELECT substr(json_extract(payload, '$.instrument.maturityDate'), 1, 10) AS d)),
        variable_rate = CASE WHEN json_type(payload, '$.instrument.variableRate') IN ('integer', 'real')
            THEN payload -> '$.instrument.variableRate' END,
        long_quantity = CASE WHEN json_type(payload, '$.longQuantity') IN ('integer', 'real')
            THEN payload -> '$.longQuantity' END,
        short_quantity = CASE WHEN json_type(payload, '$.shortQuantity') IN ('integer', 'real')
            THEN payload -> '$.shortQuantity' END,
        market_value = CASE WHEN json_type(payload, '$.marketValue') IN ('integer', 'real')
            THEN payload -> '$.marketValue' END
     WHERE rowid = NEW.rowid;
END;

CREATE TABLE transactions (
    activity_id         TEXT    NOT NULL PRIMARY KEY,
    timestamp           INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    kind                TEXT    NOT NULL,
    payload             TEXT    NOT NULL
);

-- v6. The adapter reads a transaction's legs from its payload, not from
-- this table.
CREATE TABLE transaction_legs (
    activity_id TEXT    NOT NULL,
    leg         INTEGER NOT NULL,
    asset_type  TEXT,
    cusip       TEXT,
    symbol      TEXT,
    amount      TEXT,
    cost        TEXT,
    price       TEXT,
    PRIMARY KEY (activity_id, leg)
);
