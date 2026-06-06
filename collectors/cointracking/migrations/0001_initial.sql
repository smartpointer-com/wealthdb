-- cointracking silver schema v1 (DuckDB).
--
-- This collector uses DuckDB rather than SQLite — the exception in
-- the otherwise-SQLite silver layer. Rationale lives in DESIGN.md;
-- the short version is that the holdings replay is a window-function
-- computation and amounts are arbitrary-precision decimals, both of
-- which DuckDB handles natively. Other collectors keep using SQLite
-- (their silvers are shape transformations, not computations).
--
-- The file uses CREATE … IF NOT EXISTS + INSERT … ON CONFLICT DO
-- NOTHING throughout, so load.py can re-apply it on every run
-- without a separate version tracker.

CREATE TABLE IF NOT EXISTS schema_meta (
    silver_schema_version INTEGER PRIMARY KEY,
    applied_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- One row per loaded bronze run-dir. Idempotency gate: load.py
-- skips a bronze dump whose snapshot_at is already present.
CREATE TABLE IF NOT EXISTS dump_runs (
    snapshot_at           BIGINT PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               VARCHAR NOT NULL,
    payload               JSON
);

-- Portfolios = linked CoinTracking user accounts (one per linked
-- user — switched via ?change_user=<ID>). The cointracking_user_id
-- is user-specific PII; never log a real value.
CREATE TABLE IF NOT EXISTS portfolios (
    portfolio_external_id VARCHAR PRIMARY KEY,
    cointracking_user_id  INTEGER NOT NULL,
    display_name          VARCHAR NOT NULL,
    snapshot_at           BIGINT NOT NULL,
    payload               JSON
);

-- Wallets = the per-portfolio holding location (the CSV's
-- "Exchange" column). Custodial exchanges and self-custody
-- hardware wallets land here under one neutral kind. The
-- wallet_external_id is namespaced with the portfolio so identical
-- wallet display names across portfolios stay disjoint.
CREATE TABLE IF NOT EXISTS wallets (
    portfolio_external_id VARCHAR NOT NULL,
    wallet_external_id    VARCHAR NOT NULL,
    display_name          VARCHAR NOT NULL,
    snapshot_at           BIGINT NOT NULL,
    payload               JSON,
    PRIMARY KEY (portfolio_external_id, wallet_external_id)
);

-- Raw transactions from /export/trades_csv.php. One row per CSV row.
-- Amounts are DECIMAL(38, 18): exact arithmetic, no precision loss,
-- and big enough for any conceivable crypto amount in Wei-scale
-- units. (CoinTracking truncates to 8 decimals in the export, so
-- the extra precision is unused today but future-proofs the table.)
CREATE TABLE IF NOT EXISTS transactions (
    transaction_external_id VARCHAR PRIMARY KEY,
    portfolio_external_id   VARCHAR NOT NULL,
    wallet_external_id      VARCHAR NOT NULL,
    snapshot_at             BIGINT NOT NULL,
    occurred_at             TIMESTAMP NOT NULL,
    type                    VARCHAR NOT NULL,
    buy_amount              DECIMAL(38, 18),
    buy_currency            VARCHAR,
    sell_amount             DECIMAL(38, 18),
    sell_currency           VARCHAR,
    fee_amount              DECIMAL(38, 18),
    fee_currency            VARCHAR,
    comment                 VARCHAR,
    lpn                     VARCHAR,
    payload                 JSON
);

-- Daily holdings per (portfolio, wallet, instrument), COMPUTED by
-- load.py from `transactions` via the aggregate-then-window query.
-- Recomputed from scratch on every load — CoinTracking itself
-- recalculates from genesis after any transaction edit, so the
-- silver replay matches that semantics. Only days with state
-- change are written; gold queries forward-fill.
CREATE TABLE IF NOT EXISTS positions_daily (
    as_of_date             DATE NOT NULL,
    portfolio_external_id  VARCHAR NOT NULL,
    wallet_external_id     VARCHAR NOT NULL,
    instrument_external_id VARCHAR NOT NULL,
    amount                 DECIMAL(38, 18) NOT NULL,
    snapshot_at            BIGINT NOT NULL,
    PRIMARY KEY (as_of_date, portfolio_external_id, wallet_external_id, instrument_external_id)
);

INSERT INTO schema_meta (silver_schema_version) VALUES (1)
ON CONFLICT DO NOTHING;
