-- ============================================================
-- ubs-psn silver schema, migration 0001 — initial schema.
--
-- Migration discipline: every change to the silver schema lands as a
-- new numbered file in this directory. The loader checks the maximum
-- applied silver_schema_version in schema_meta and applies any
-- migrations whose number is greater, in order. Silver databases must
-- always conform to the latest schema; no backward-compatible drift.
--
-- Storage convention: timestamps are INTEGER Unix seconds (UTC).
-- Stable filter columns are promoted; everything else lives in the
-- `payload` JSON column. See the "Silver semi-relational" memory note
-- for the broader contract.
--
-- Identifier conventions (see README / DESIGN once written):
--   relationship_id          — UBS Server ID from the FTP-access PDF
--                              ('SFTPCH01', 'SFTPCH02', ...). One per
--                              banking relationship under one SFTP login.
--   account_external_id      — IBAN for cash accounts, or the UBS
--                              safekeeping code for safekeeping accounts
--                              (banking-relationship prefix + suffix
--                              like 'S1', 'S2', 'T1', ...).
--   portfolio_external_id    — UBS PrtflId.
--   client_external_id       — UBS ClntId (numeric, derived from the
--                              banking-relationship number).
--   contract_external_id     — Per-MT/XML contract reference (TDFWD/TDOPT/etc.).
--   isin                     — ISO 6166 ISIN-12.
--   event_external_id        — Per-MT extracted source reference (`:20C::SEME//`
--                              on most securities messages, statement / bank
--                              ref on MT940 :61: lines, etc.).
--
-- Plaintext IDs always stay in the payload as well, for traceability.
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
-- date X had no holdings / no events / etc."
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,   -- Unix seconds UTC
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL                -- absolute path of dump directory
);

-- ============================================================
-- SNAPSHOT TABLES — slow-changing master data
--
-- Source: UBS XML feed (`ZMD.zip` for SD*, `ZME.zip` for TD*).
-- These records change rarely (account opened, instrument added).
-- Loader applies content-based dedup against the most-recent row
-- for the same PK-prefix, after stripping per-batch noise:
-- <DWHMsgId>, <MsqSeqNo>, <CrtnDtTm> in the UBS <Hdr:Header>.
-- ============================================================

-- One row per snapshot per client. SDCL is typically a single client
-- record per banking relationship.
CREATE TABLE account_holders (
    snapshot_at          INTEGER NOT NULL,
    relationship_id      TEXT    NOT NULL,
    client_external_id   TEXT    NOT NULL,
    payload              TEXT    NOT NULL,                -- full SDCL payload
    PRIMARY KEY (snapshot_at, relationship_id, client_external_id)
);

-- One row per cash account per snapshot. account_external_id is the IBAN.
CREATE TABLE cash_accounts (
    snapshot_at          INTEGER NOT NULL,
    relationship_id      TEXT    NOT NULL,
    account_external_id  TEXT    NOT NULL,                -- IBAN
    payload              TEXT    NOT NULL,                -- full SDCA per-account block
    PRIMARY KEY (snapshot_at, relationship_id, account_external_id)
);

-- One row per safekeeping account per snapshot. account_external_id is
-- the full 28-char UBS safekeeping code.
CREATE TABLE safekeeping_accounts (
    snapshot_at          INTEGER NOT NULL,
    relationship_id      TEXT    NOT NULL,
    account_external_id  TEXT    NOT NULL,                -- UBS safekeeping code
    payload              TEXT    NOT NULL,                -- full SDSA per-account block
    PRIMARY KEY (snapshot_at, relationship_id, account_external_id)
);

-- One row per portfolio per snapshot. SDPO carries each portfolio's
-- composition (sub-positions / portfolio elements) inline; we keep
-- that nested structure in payload rather than splitting into a
-- separate per-element table.
CREATE TABLE portfolios (
    snapshot_at            INTEGER NOT NULL,
    relationship_id        TEXT    NOT NULL,
    portfolio_external_id  TEXT    NOT NULL,              -- UBS PrtflId
    payload                TEXT    NOT NULL,              -- portfolio + nested elements
    PRIMARY KEY (snapshot_at, relationship_id, portfolio_external_id)
);

-- One row per instrument per snapshot. SDFI is the securities master
-- record (name, asset class, exchange, etc.). Kept relationship-scoped
-- on the off chance UBS ever varies the per-relationship view (e.g.
-- pricing source); collapse in a later migration if always identical.
CREATE TABLE instruments (
    snapshot_at          INTEGER NOT NULL,
    relationship_id      TEXT    NOT NULL,
    isin                 TEXT    NOT NULL,
    payload              TEXT    NOT NULL,                -- full SDFI per-instrument block
    PRIMARY KEY (snapshot_at, relationship_id, isin)
);

-- ============================================================
-- SNAPSHOT TABLES — daily state
--
-- Genuinely change every batch (prices, quantities, balances).
-- No content-dedup — each dump produces a new row per entity.
-- ============================================================

-- One row per (snapshot, safekeeping, instrument) from MT535 (ZAH).
-- Carries qty, market value, book value, currency in payload.
CREATE TABLE holdings (
    snapshot_at               INTEGER NOT NULL,
    relationship_id           TEXT    NOT NULL,
    safekeeping_external_id   TEXT    NOT NULL,
    isin                      TEXT    NOT NULL,
    payload                   TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, safekeeping_external_id, isin)
);

-- One row per (snapshot, account, balance_kind) from MT940 `:60F:`
-- (opening), `:62F:` (closing), `:64:` (available/forward). currency_iso
-- is promoted to allow fast per-currency aggregation; it's also in payload.
CREATE TABLE cash_balances (
    snapshot_at          INTEGER NOT NULL,
    relationship_id      TEXT    NOT NULL,
    account_external_id  TEXT    NOT NULL,                -- IBAN
    balance_kind         TEXT    NOT NULL
        CHECK (balance_kind IN ('opening', 'closing', 'available')),
    currency_iso         TEXT    NOT NULL,
    payload              TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, account_external_id, balance_kind)
);

-- One row per (snapshot, safekeeping) from MT537 (ZM5). Header-only
-- "no pending activity" messages are stored as such — payload records
-- ACTI//N — so absence here means "no dump that day", not "no info".
CREATE TABLE pending_securities (
    snapshot_at               INTEGER NOT NULL,
    relationship_id           TEXT    NOT NULL,
    safekeeping_external_id   TEXT    NOT NULL,
    payload                   TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, safekeeping_external_id)
);

-- One row per (snapshot, currency pair) from TDFXR. base is always
-- CHF in the current PSN setup; modelled as a column to leave room
-- for future cross-base support without a migration.
CREATE TABLE fx_rates (
    snapshot_at          INTEGER NOT NULL,
    base_currency_iso    TEXT    NOT NULL,
    quote_currency_iso   TEXT    NOT NULL,
    payload              TEXT    NOT NULL,                -- mid/buy/sell, period code
    PRIMARY KEY (snapshot_at, base_currency_iso, quote_currency_iso)
);

-- One row per (snapshot, portfolio) from TDPOPF. Monthly; UBS generates
-- this only on the 7th of each month, so most snapshots will not have
-- new TDPOPF rows.
CREATE TABLE portfolio_performance (
    snapshot_at            INTEGER NOT NULL,
    relationship_id        TEXT    NOT NULL,
    portfolio_external_id  TEXT    NOT NULL,
    payload                TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, portfolio_external_id)
);

-- One row per (snapshot, cash account) from TDCAPI. Carries the
-- pricing / interest configuration for each cash account.
CREATE TABLE cash_account_pricing (
    snapshot_at          INTEGER NOT NULL,
    relationship_id      TEXT    NOT NULL,
    account_external_id  TEXT    NOT NULL,                -- IBAN
    payload              TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, account_external_id)
);

-- ============================================================
-- SNAPSHOT TABLES — "open" contract state
--
-- Forward/option/MM/OTC contracts have a lifecycle (open → mature/
-- settle). Modelled as snapshots so we get a clean as-of view of
-- "what was open when". Each contract appears in every snapshot
-- where it's still open; the contract_external_id is in the PK so
-- multiple contracts coexist per snapshot.
-- ============================================================

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

-- ============================================================
-- EVENT TABLE
--
-- One unified table for everything that *happened* (cash movements,
-- trade confirmations, corporate actions, FX confirmations, ...).
-- The `kind` column discriminates; full per-source detail lives in
-- payload. Not constrained at the schema layer so new MT types can be
-- added by the loader without a migration.
--
-- Load semantics: window-DELETE-then-INSERT, in one transaction,
-- per (account_external_id, kind, time-range). Never row-level upsert,
-- so upstream removals/corrections propagate.
--
-- MT950 (ZAY) is intentionally NOT loaded: for retail PSN it's a
-- bank-to-bank duplicate of MT940. Bronze keeps the raw ZAY zip; if a
-- future user has MT950-only accounts, add a loader path then.
-- ============================================================

-- Expected `kind` values, added as the corresponding loader paths land:
--   'cash_movement'                — MT940 `:61:` line  (Z40)
--   'securities_movement'          — MT536              (ZMH)
--   'trade_confirmation'           — MT515              (ZAG)
--   'fx_confirmation'              — MT300              (ZAA)
--   'fx_option_confirmation'       — MT305              (ZAB)
--   'loan_deposit_confirmation'    — MT320/MT330/MT350  (ZAC/ZAD/ZAE)
--   'corporate_action_notification'— MT564              (ZM6)
--   'corporate_action_confirmation'— MT566              (ZAN)
--   'corporate_action_narrative'   — MT568              (ZM7)
--   'securities_settlement_advice' — MT544–MT548        (ZAI/ZAJ/ZAK/ZAL/ZAM)
--   'precious_metal_trade'         — MT600/MT601        (ZAX/ZM9)
--   'charges_advice'               — MT590/MT990        (ZM8/ZMC)
--   'debit_credit_confirmation'    — MT900/MT910        (ZAO/ZAP)
CREATE TABLE events (
    event_external_id    TEXT    NOT NULL PRIMARY KEY,
    timestamp            INTEGER NOT NULL,                -- value date / event date, Unix seconds UTC
    relationship_id      TEXT    NOT NULL,
    account_external_id  TEXT    NOT NULL,                -- IBAN for cash, safekeeping ID for securities
    kind                 TEXT    NOT NULL,
    currency_iso         TEXT,                            -- NULL for kinds that don't carry one
    payload              TEXT    NOT NULL
);
-- One secondary index. The PK on event_external_id does not help time-
-- range queries; (account_external_id, timestamp) composite covers both
-- "all activity at time T" and "all activity for account A".
CREATE INDEX ix_events_account_timestamp
    ON events(account_external_id, timestamp);

-- Record that this migration was applied. Must be the last statement
-- in the file; the loader uses it as the migration-complete marker.
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));
