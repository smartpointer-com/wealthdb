-- ============================================================
-- swissquote-dump silver schema, migration 0001 — initial schema.
--
-- Migration discipline: every change lands as a new numbered file in
-- this directory. load.py reads MAX(silver_schema_version) from
-- schema_meta and applies any migrations whose number is greater, in
-- order. Silver databases must always conform to the latest schema;
-- no backward-compatible drift.
--
-- Storage convention: timestamps are INTEGER Unix seconds (UTC).
-- Swissquote emits transaction times in Europe/Zurich local time
-- (CSV column "Date", format DD-MM-YYYY HH:MM:SS); load.py converts
-- to UTC epoch on the way in and preserves the original string inside
-- payload for traceability.
--
-- Stable filter columns are promoted; everything else lives in the
-- `payload` JSON column. See sibling repos' DESIGN.md (schwab-dump)
-- for the semi-relational contract.
-- ============================================================

PRAGMA foreign_keys = ON;

-- One row per applied migration. Loader writes a row at the end of
-- each migration; current schema version = MAX(silver_schema_version).
CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                 -- Unix seconds UTC
);

-- One row per dump run, regardless of whether any sub-artefact had
-- data. Lets queries distinguish "no dump on date X" from "dump on
-- date X had no new documents".
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,    -- Unix seconds UTC; run-start time
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL                 -- absolute path of dump directory
);

-- ============================================================
-- SNAPSHOT TABLES
-- Monotemporal on source time. PK is composite and starts with
-- snapshot_at, so the implicit B-tree serves as the as-of index.
-- ============================================================

-- Per-customer metadata captured at dump time (display name, default
-- currency, IBAN if visible, anything else download.py scrapes from
-- the Portfolio/Account UI). Today the customer-ID is the only field
-- we reliably extract; payload will grow as download.py learns the UI.
--
-- Loader dedup: insert only when the new canonical-JSON payload
-- differs from the most-recent payload for the same account_external_id.
-- Direct text comparison; no stored hash column.
CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,                  -- Swissquote customer ID, e.g. "1234567"
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

-- One row per held instrument per snapshot.
--
-- The Swissquote Positions XLS export carries `Symbol` and `CCY` but
-- not ISIN — symbol is therefore the primary identifier in the PK.
-- ISIN, where available, is recorded inside payload (the loader may
-- backfill it from the transactions stream where symbols overlap).
-- Currency is in the PK because Swissquote groups positions per
-- currency in the UI, and a single instrument held in two currency
-- books would surface as two rows; only CHF is in the book today but
-- the schema mirrors the source's multi-currency model from day one.
--
-- Symbol is stored as '' (empty string), never NULL, when absent —
-- SQLite allows multiple NULLs in PK columns, which would silently
-- permit duplicate rows. NOT NULL + '' sentinel keeps uniqueness
-- enforced.
--
-- Cash balances are NOT in this table — see `currency_balances`
-- below. Swissquote splits the two artefacts (Positions export vs.
-- List of Assets export) at the source; silver mirrors that split.
-- Conflating them into a synthetic 'CASH' row would muddy
-- provenance.
--
-- Promoted columns are the minimum set that adapters in wealth-suite
-- need as filter/join keys without parsing JSON. Quantity, valuations,
-- average cost, P&L, asset_class (the section-header dimension from
-- the XLS — 'ETFs', 'Bonds', etc.), and the raw row all live in payload.
CREATE TABLE positions (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    symbol              TEXT    NOT NULL,                  -- '' if absent
    currency            TEXT    NOT NULL,                  -- ISO 4217 (CHF/EUR/USD/XAU/...)
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, symbol, currency)
);

-- Per-currency rollup of cash, securities valuation, and the FX
-- rate against the account's reference currency (CHF) at snapshot
-- time. Source: Swissquote's "List of Assets" XLS export, which has
-- one row per currency the account is exposed to (including
-- zero-balance currencies that the account is provisioned for).
--
-- PK = (snapshot_at, account, currency). The XLS includes a
-- "Total CHF" footer row that aggregates all currencies; the
-- loader drops it (gold can re-compute the sum trivially).
--
-- payload: {
--   rate_to_chf:     float,   -- 1.0 for CHF; reciprocal-style rate that maps 1 unit currency -> CHF
--   cash_balance:    float,   -- in the row's currency
--   positions_value: float,   -- in the row's currency
--   total_value:     float,   -- cash + positions, in the row's currency
--   valuation_chf:   float,   -- total_value * rate_to_chf
--   account_pct:     float    -- this currency's share of the account's CHF valuation
-- }
CREATE TABLE currency_balances (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    currency            TEXT    NOT NULL,                  -- ISO 4217
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id, currency)
);

-- ============================================================
-- EVENT TABLE
-- Monotemporal on source time. No primary key by design — Swissquote
-- assigns "Order #" only to trade executions (and shares one Order #
-- across partial fills); cash/dividend/fee rows carry the literal
-- "00000000". Any synthetic content-hash PK would either pile up
-- phantoms or silently keep stale rows when Swissquote amends a
-- past event.
--
-- Load semantics: window-DELETE then INSERT, in one transaction, per
-- (account_external_id, time-range). The dump emits one CSV per
-- non-overlapping window and records the bounds in run.json; the
-- loader replaces exactly that range. Reloading a window with an
-- amended past event therefore converges to Swissquote's current
-- truth, even if dates or amounts changed.
-- ============================================================

-- transaction_type values seen in real data:
--   'Buy', 'Sell'                — securities trade (Order # populated)
--   'Dividend'                   — equity/ETF cash distribution
--   'Coupon'                     — bond coupon
--   'Capital Gain'               — fund capital-gain distribution
--   'Custody Fees'               — quarterly custody fee
--   'Fees Tax Statement'         — annual e-tax statement fee
--   'Debit'                      — outgoing wire/transfer
--   'Payment'                    — incoming wire/transfer
-- Not constrained at the schema layer so new Swissquote types do not
-- require a migration.
--
-- order_num: the CSV's "Order #" column. The placeholder "00000000"
-- is normalised to NULL at load time so SQL queries can use
-- IS NULL / IS NOT NULL to separate trade rows from non-trade rows
-- without string-equality dances.
--
-- net_amount: signed (positive = inflow, negative = outflow). Direct
-- from the CSV "Net Amount" column.
--
-- payload preserves the full row including: quantity, unit_price
-- (with its trailing "%" flag intact for bond-as-pct-of-face rows),
-- costs, accrued_interest, balance (running), name, and the raw CSV
-- line for forensic round-trip.
CREATE TABLE transactions (
    account_external_id TEXT    NOT NULL,
    occurred_at         INTEGER NOT NULL,                  -- Unix seconds UTC (CSV Date, Europe/Zurich → UTC)
    transaction_type    TEXT    NOT NULL,
    order_num           TEXT,                              -- NULL when CSV has '00000000'
    isin                TEXT,                              -- NULL for cash-only rows
    symbol              TEXT,                              -- NULL for cash-only rows
    currency            TEXT    NOT NULL,
    net_amount          REAL    NOT NULL,
    payload             TEXT    NOT NULL
);

-- One secondary index. Dominant query is "all transactions for
-- account A in time range [t0, t1]"; the composite supports both
-- that and the loader's DELETE bounds.
CREATE INDEX ix_transactions_account_occurred
    ON transactions(account_external_id, occurred_at);

-- ============================================================
-- DOCUMENT INDEX
-- One row per unique file. The PDF binaries themselves stay in
-- bronze; this table just records what's been seen, where it lives,
-- and minimal metadata for query/dedup.
--
-- Two sources feed this table:
--   'auto'   — fetched by download.py from the eBanking Documents
--              endpoint; swissquote_doc_id is populated.
--   'manual' — dropped into <bronze-dir>/manual/ by the user (e-tax
--              statements brought forward from prior years, anything
--              else out-of-band); swissquote_doc_id is NULL.
--
-- content_sha256 is the natural identity: same file => same row.
-- This dedups across re-runs of download.py (Swissquote serves the
-- same bytes for the same doc_id) and across accidental re-drops in
-- manual/.
-- ============================================================

CREATE TABLE documents (
    content_sha256      TEXT NOT NULL PRIMARY KEY,         -- hex sha256 of the file bytes
    source              TEXT NOT NULL
        CHECK (source IN ('auto', 'manual')),
    swissquote_doc_id   TEXT UNIQUE,                       -- Swissquote GUID for 'auto'; NULL for 'manual'
    account_external_id TEXT,                              -- NULL when not attributable
    document_type       TEXT,                              -- 'Corporate','Stock','Transfer','Account','TaxStatement',...
    first_seen_at       INTEGER NOT NULL,                  -- Unix seconds UTC; the dump_run that first recorded it
    bronze_path         TEXT NOT NULL,                     -- path relative to bronze-dir
    payload             TEXT NOT NULL                      -- canonical JSON: original filename, listing-row metadata
);

-- Record that this migration was applied. Must be the last statement
-- in the file; load.py uses it as the migration-complete marker.
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));
