-- The cost-basis additions to the minimal fidelity silver schema, as
-- fidelity-web migrations 0009, 0011 and 0012 leave them. Applied on
-- top of silver_schema.sql; a test without it stands for a silver
-- from before them. SQLite syntax.

ALTER TABLE historical_position_snapshots ADD COLUMN cost_basis REAL;

CREATE TABLE open_lots (
    snapshot_at          INTEGER NOT NULL,
    account_external_id  TEXT    NOT NULL,
    instrument_key       TEXT    NOT NULL,
    lot_index            INTEGER NOT NULL,
    cusip                TEXT,
    quantity             REAL,
    unit_cost            REAL,
    cost_basis           REAL,
    acquired_date        TEXT,
    unrealized_gain_loss REAL,
    current_value        REAL,
    term                 TEXT,
    covered              INTEGER,
    currency             TEXT    NOT NULL DEFAULT 'USD',
    source_sha256        TEXT    NOT NULL DEFAULT 'sha-lots',
    payload              TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (snapshot_at, account_external_id, instrument_key, lot_index)
);

CREATE TABLE closed_lots (
    lot_id                   TEXT    NOT NULL PRIMARY KEY,
    document_kind            TEXT    NOT NULL,
    account_external_id      TEXT    NOT NULL,
    tax_year                 INTEGER,
    form_prepared            TEXT,
    security_name            TEXT,
    instrument_key           TEXT,
    cusip                    TEXT,
    action                   TEXT,
    quantity                 REAL,
    acquired_date            TEXT,
    disposed_date            TEXT,
    settlement_date          TEXT,
    proceeds                 REAL,
    cost_basis               REAL,
    accrued_market_discount  REAL,
    wash_sale_disallowed     REAL,
    realized_gain_loss       REAL,
    fees                     REAL,
    federal_tax_withheld     REAL,
    term                     TEXT,
    covered                  INTEGER,
    form_8949_box            TEXT,
    specific_share_id        INTEGER,
    corrected                INTEGER,
    currency                 TEXT    NOT NULL DEFAULT 'USD',
    source_sha256            TEXT    NOT NULL DEFAULT 'sha-doc',
    payload                  TEXT    NOT NULL DEFAULT '{}'
);
