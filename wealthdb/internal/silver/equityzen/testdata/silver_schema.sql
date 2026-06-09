-- Minimal equityzen silver schema for adapter tests — the subset of
-- collectors/equityzen/migrations the adapter reads (dump_runs, offerings,
-- event-sourced positions, the cash_flows ledger with the 0002 shares/price
-- columns). Dates are source ISO TEXT, as in the real silver.

CREATE TABLE dump_runs (
    snapshot_at INTEGER NOT NULL PRIMARY KEY   -- unix seconds (download time)
);

CREATE TABLE offerings (
    deal_external_id TEXT NOT NULL PRIMARY KEY,
    kind             TEXT,                      -- 'spv' | 'private_fund'
    company_name     TEXT,
    currency         TEXT NOT NULL DEFAULT 'USD',
    payload          TEXT
);

CREATE TABLE positions (
    deal_external_id     TEXT    NOT NULL,
    event_seq            INTEGER NOT NULL,      -- 0 = investment; increases with as_of_date
    as_of_date           TEXT,                  -- event date (ISO)
    event_type           TEXT,
    is_open              INTEGER,               -- 1 = held after this event; 0 = exited
    shares_held          REAL,
    cost_basis_remaining REAL,
    market_value         REAL,
    PRIMARY KEY (deal_external_id, event_seq)
);

CREATE TABLE cash_flows (
    cash_flow_external_id TEXT NOT NULL PRIMARY KEY,
    deal_external_id      TEXT NOT NULL,
    kind                  TEXT NOT NULL,        -- 'purchase' | 'distribution'
    flow_date             TEXT,                 -- ISO
    amount                REAL,                 -- positive magnitude
    shares                REAL,
    price_per_share       REAL,
    currency              TEXT NOT NULL DEFAULT 'USD'
);
