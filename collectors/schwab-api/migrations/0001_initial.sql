-- ============================================================
-- schwab-api silver schema, migration 0001 — initial schema.
--
-- Migration discipline: every change to the silver schema lands as a
-- new numbered file in this directory. The parser checks the maximum
-- applied silver_schema_version in schema_meta and applies any
-- migrations whose number is greater, in order. Silver databases must
-- always conform to the latest schema; no backward-compatible drift.
--
-- Storage convention: timestamps are INTEGER Unix seconds (UTC).
-- Stable filter columns are promoted; everything else lives in the
-- `payload` JSON column. See the "Silver semi-relational" memory note
-- for the broader contract.
-- ============================================================

PRAGMA foreign_keys = ON;

-- One row per applied migration. Loader writes a row at the end of
-- each migration; current schema version = MAX(silver_schema_version).
CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                -- Unix seconds UTC
);

-- One row per dump run, regardless of whether any sub-artefact had
-- rows. Lets queries distinguish "no dump on date X" from "dump on
-- date X had no open orders".
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,   -- Unix seconds UTC
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL                -- absolute path of dump directory
);

-- ============================================================
-- SNAPSHOT TABLES
-- Monotemporal on source time. PK is composite and starts with
-- snapshot_at, so the implicit B-tree serves as the as-of index.
-- No separate snapshot_at index needed.
-- ============================================================

-- accountNumber ↔ hashValue mapping captured at dump time.
-- Loader dedup: insert only when the new canonical-JSON payload differs
-- from the most-recent payload for the same account_external_id.
-- Direct text comparison, no stored hash column needed.
CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,                 -- Schwab hashValue
    payload             TEXT    NOT NULL,                 -- JSON: {accountNumber, hashValue}
    PRIMARY KEY (snapshot_at, account_external_id)
);

-- /userPreference top-level response.
-- Loader dedup: insert only when the new canonical-JSON payload differs
-- from the most-recent row. Direct text comparison.
CREATE TABLE user_preference (
    snapshot_at INTEGER NOT NULL PRIMARY KEY,
    payload     TEXT    NOT NULL                          -- full /userPreference response
);

-- balance_kind values:
--   'initial'    — securitiesAccount.initialBalances
--   'current'    — securitiesAccount.currentBalances
--   'projected'  — securitiesAccount.projectedBalances
--   'aggregated' — top-level aggregatedBalance object
CREATE TABLE account_balances (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    balance_kind        TEXT    NOT NULL
        CHECK (balance_kind IN ('initial', 'current', 'projected', 'aggregated')),
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, balance_kind)
);

-- One row per held instrument per snapshot. instrument_key is the
-- best stable identifier the source provides: CUSIP when present,
-- else symbol. The chosen key is also recorded inside payload for
-- traceability.
CREATE TABLE positions (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    instrument_key      TEXT    NOT NULL,
    payload             TEXT    NOT NULL,                 -- full position object
    PRIMARY KEY (snapshot_at, account_external_id, instrument_key)
);

-- One row per open order per snapshot. Snapshots with zero open
-- orders are recorded only in dump_runs; absence here means "zero
-- orders observed" provided dump_runs has a matching row.
-- `status` is promoted because per-status queries ("which orders
-- are AWAITING_PARENT_ORDER right now") are likely.
CREATE TABLE open_orders (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    order_external_id   TEXT    NOT NULL,                 -- Schwab orderId
    status              TEXT    NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, order_external_id)
);

-- ============================================================
-- EVENT TABLE
-- Monotemporal on event-time (Schwab `time` field, renamed to
-- `timestamp` to avoid confusion with time-of-day).
-- Load semantics: window-DELETE then INSERT, in one transaction,
-- per (account_external_id, time-range). Never row-level upsert.
-- ============================================================

-- One row per Schwab activityId. `kind` is the source `type` field
-- (TRADE, JOURNAL, DIVIDEND_OR_INTEREST, WIRE_OUT, WIRE_IN,
-- CASH_DISBURSEMENT, CASH_RECEIPT, RECEIVE_AND_DELIVER, MEMORANDUM,
-- MARGIN_CALL, MONEY_MARKET, SMA_ADJUSTMENT, ELECTRONIC_FUND, ...);
-- not constrained at the schema layer so new Schwab types do not
-- require a migration.
CREATE TABLE transactions (
    activity_id         TEXT    NOT NULL PRIMARY KEY,
    timestamp           INTEGER NOT NULL,                 -- Unix seconds UTC; Schwab `time`
    account_external_id TEXT    NOT NULL,
    kind                TEXT    NOT NULL,
    payload             TEXT    NOT NULL
);
-- One secondary index. The PK on activity_id does not help time-
-- range queries; (account_external_id, timestamp) composite covers
-- both "all activity at time T" and "all activity for account A".
CREATE INDEX ix_transactions_account_timestamp
    ON transactions(account_external_id, timestamp);

-- Record that this migration was applied. Must be the last statement
-- in the file; the parser uses it as the migration-complete marker.
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));
