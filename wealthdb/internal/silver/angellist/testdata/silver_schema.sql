-- angellist silver schema for adapter tests — the tables the gold adapter
-- reads, mirroring the collector migrations through 0005 (event-sourced
-- offerings + position_snapshots). vehicles / k1_capital_accounts /
-- commitments / portfolio_timeseries are collector-internal and omitted.

CREATE TABLE schema_meta (
    silver_schema_version INTEGER PRIMARY KEY,
    applied_at            TEXT
);

CREATE TABLE dump_runs (
    snapshot_at         INTEGER PRIMARY KEY,
    invest_account_slug TEXT,
    loaded_at           TEXT,
    payload             TEXT
);

CREATE TABLE portfolio_summary (
    snapshot_at         INTEGER PRIMARY KEY,
    invest_account_slug TEXT,
    currency            TEXT,
    payload             TEXT
);

CREATE TABLE offerings (
    position_external_id TEXT    PRIMARY KEY,
    vehicle_external_id  TEXT,
    kind                 TEXT,            -- 'spv' | 'fund'
    company_name         TEXT,
    fund_name            TEXT,
    fund_tax_id          TEXT,
    investment_date      INTEGER,
    currency             TEXT NOT NULL DEFAULT 'USD',
    first_seen_at        INTEGER,
    last_seen_at         INTEGER,
    payload              TEXT
);

CREATE TABLE position_snapshots (
    position_external_id TEXT    NOT NULL,
    as_of_date           INTEGER NOT NULL,
    event_type           TEXT    NOT NULL,
    status               TEXT,
    is_open              INTEGER NOT NULL DEFAULT 1,
    currency             TEXT,
    market_value_minor   INTEGER,
    valuation_basis      TEXT,
    contributed_minor    INTEGER,
    distributions_minor  INTEGER,
    snapshot_at          INTEGER NOT NULL,
    payload              TEXT,
    PRIMARY KEY (position_external_id, as_of_date)
);

CREATE TABLE funding_accounts (
    funding_account_external_id TEXT PRIMARY KEY,
    slug_name      TEXT,
    legal_name     TEXT,
    currency       TEXT,
    balance_minor  INTEGER,
    snapshot_at    INTEGER,
    payload        TEXT
);

CREATE TABLE funding_transactions (
    transaction_external_id     TEXT PRIMARY KEY,
    funding_account_external_id TEXT,
    occurred_at        INTEGER NOT NULL,
    type               TEXT,
    amount_minor       INTEGER,
    currency           TEXT,
    running_balance_minor INTEGER,
    syndicate_name     TEXT,
    description        TEXT,
    position_external_id TEXT,
    snapshot_at        INTEGER,
    payload            TEXT
);

INSERT INTO schema_meta (silver_schema_version) VALUES (6);
