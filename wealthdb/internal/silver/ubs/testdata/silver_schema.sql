-- Minimal subset of ubs-psn-dump's silver schema for adapter
-- tests. Mirrors the column types of the upstream migration but
-- skips PRAGMAs and FK declarations that don't affect the
-- adapter's read path.

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE account_holders (
    snapshot_at          INTEGER NOT NULL,
    relationship_id      TEXT    NOT NULL,
    client_external_id   TEXT    NOT NULL,
    payload              TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, client_external_id)
);

CREATE TABLE cash_accounts (
    snapshot_at            INTEGER NOT NULL,
    relationship_id        TEXT    NOT NULL,
    account_external_id    TEXT    NOT NULL,
    portfolio_external_id  TEXT,
    payload                TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, account_external_id)
);

CREATE TABLE safekeeping_accounts (
    snapshot_at            INTEGER NOT NULL,
    relationship_id        TEXT    NOT NULL,
    account_external_id    TEXT    NOT NULL,
    portfolio_external_id  TEXT,
    payload                TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, account_external_id)
);

CREATE TABLE portfolios (
    snapshot_at            INTEGER NOT NULL,
    relationship_id        TEXT    NOT NULL,
    portfolio_external_id  TEXT    NOT NULL,
    base_currency          TEXT,
    payload                TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, portfolio_external_id)
);

CREATE TABLE instruments (
    snapshot_at          INTEGER NOT NULL,
    relationship_id      TEXT    NOT NULL,
    isin                 TEXT    NOT NULL,
    payload              TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, isin)
);

CREATE TABLE holdings (
    snapshot_at               INTEGER NOT NULL,
    relationship_id           TEXT    NOT NULL,
    safekeeping_external_id   TEXT    NOT NULL,
    isin                      TEXT    NOT NULL,
    payload                   TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, safekeeping_external_id, isin)
);

CREATE TABLE cash_balances (
    snapshot_at          INTEGER NOT NULL,
    relationship_id      TEXT    NOT NULL,
    account_external_id  TEXT    NOT NULL,
    balance_kind         TEXT    NOT NULL,
    currency_iso         TEXT    NOT NULL,
    payload              TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, account_external_id, balance_kind)
);

CREATE TABLE pending_securities (
    snapshot_at               INTEGER NOT NULL,
    relationship_id           TEXT    NOT NULL,
    safekeeping_external_id   TEXT    NOT NULL,
    payload                   TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, safekeeping_external_id)
);

CREATE TABLE fx_rates (
    snapshot_at          INTEGER NOT NULL,
    base_currency_iso    TEXT    NOT NULL,
    quote_currency_iso   TEXT    NOT NULL,
    payload              TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, base_currency_iso, quote_currency_iso)
);

CREATE TABLE forward_contracts (
    snapshot_at           INTEGER NOT NULL,
    relationship_id       TEXT    NOT NULL,
    contract_external_id  TEXT    NOT NULL,
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, contract_external_id)
);

CREATE TABLE option_contracts (
    snapshot_at           INTEGER NOT NULL,
    relationship_id       TEXT    NOT NULL,
    contract_external_id  TEXT    NOT NULL,
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, contract_external_id)
);

CREATE TABLE money_market_contracts (
    snapshot_at           INTEGER NOT NULL,
    relationship_id       TEXT    NOT NULL,
    contract_external_id  TEXT    NOT NULL,
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, contract_external_id)
);

CREATE TABLE otc_contracts (
    snapshot_at           INTEGER NOT NULL,
    relationship_id       TEXT    NOT NULL,
    contract_external_id  TEXT    NOT NULL,
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, contract_external_id)
);

CREATE TABLE events (
    event_external_id    TEXT    NOT NULL PRIMARY KEY,
    timestamp            INTEGER NOT NULL,
    relationship_id      TEXT    NOT NULL,
    account_external_id  TEXT    NOT NULL,
    kind                 TEXT    NOT NULL,
    currency_iso         TEXT,
    payload              TEXT    NOT NULL
);
