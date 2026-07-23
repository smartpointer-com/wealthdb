-- ============================================================
-- relevate silver schema, migration 0001 — initial schema.
--
-- Migration discipline: every change lands as a new numbered file
-- in this directory. The loader checks the maximum applied
-- silver_schema_version in schema_meta and applies any newer
-- migrations in order. Silver databases must always conform to
-- the latest schema; no backward-compatible drift.
--
-- Storage conventions:
--   * Unix-seconds-UTC integers for all timestamps.
--   * Stable filter columns promoted; rest in `payload` TEXT JSON.
--   * Snapshot tables monotemporal on snapshot_at (PK starts with
--     it, so the implicit B-tree serves as the as-of index).
--   * Event tables keyed by a stable external id; INSERT OR REPLACE
--     so re-loading a window converges.
--   * Documents content-deduped via PRIMARY KEY content_sha256.
--
-- ------------------------------------------------------------
-- Identifier conventions
-- ------------------------------------------------------------
--
--   account_external_id
--     `portfolios[].externalId` from
--     /middlelayer/v2/portfolio/investment-overview. Shape:
--     `NNNN.NNNNNN.N`, foundation-issued, stable across sessions.
--     One row per Relevate "portfolio" — what surfaces as one
--     Vested Benefits depot. wealthdb gold joins on this.
--
--   portfolio_internal_id
--     `portfolios[].id` — the foundation's internal DB key (small
--     integer). Kept on `accounts` as a join column
--     because Relevate's per-portfolio API endpoints
--     (/Portfolio/{id}/deposits, /portfolio/{id}/performance, ...)
--     are keyed by THIS, not by externalId.
--
--   instrument_external_id
--     `modelportfolio.positions[].security.id` (numeric, Relevate-
--     internal). Used as the stable join key for `instruments`.
--     ISIN is also promoted as a separate column when present, for
--     cross-bank joins in wealthdb gold.
--
--   transaction_external_id
--     'credit_note:<relevate_doc_id>' for contribution events the
--     loader parses out of credit-note PDFs — deterministic, so
--     re-loads collapse to the same row. The /deposits endpoint
--     can return an empty `{transactions:[]}` envelope for FZ
--     accounts; the credit-note PDFs carry the contribution data.
--
--   content_sha256 (documents)
--     SHA-256 hex of the PDF on disk. Same row regardless of how
--     many bronze dumps fetched the same file; snapshot_at records
--     the first dump that observed it.
--
-- ------------------------------------------------------------
-- Gold merge strategy (informational; implemented in wealthdb)
-- ------------------------------------------------------------
--
-- The relevate silver is currently the only source for Relevate
-- accounts. wealthdb's `relevate` adapter projects:
--   accounts.account_external_id        -> gold.accounts.account_external_id
--   accounts.product_key (FZ*)          -> gold.accounts.tax_wrapper='vested_benefits'
--   accounts.product_key                -> management_style='automated' for FZPF and FZI
--                                          Both products are robo-advisor-shaped: the
--                                          holder picks one of a small menu of pre-built
--                                          strategies at setup, and the strategy then
--                                          runs by rules with no further human input
--                                          (no foundation manager, no advisor, no
--                                          per-security holder action — Relevate doesn't
--                                          let the holder pick individual securities or
--                                          funds). Any other FZ* product code falls
--                                          through to management_style='other' until
--                                          classified.
--   positions                           -> gold.positions (TARGET allocation, not actual units;
--                                          Relevate doesn't surface unit holdings on the
--                                          endpoints we've mapped)
--   cash_balances                       -> gold.cash_balances
--   performance_points                  -> not in gold today; payload-preserved for the adapter
--                                          to derive cash/securities timelines if needed
--   documents                           -> gold.documents (PDF binaries stay on bronze disk;
--                                          this is the index)
-- ============================================================

PRAGMA foreign_keys = ON;

-- All-or-nothing migration: BEGIN/COMMIT wrap the whole file so a
-- mid-script failure rolls everything back. Python's sqlite3
-- executescript() does an implicit COMMIT *before* running, so
-- transaction control must live inside the migration itself —
-- a Python-side BEGIN gets swallowed.
BEGIN;

-- One row per applied migration. The loader writes a row at the
-- end of each migration; current schema version =
-- MAX(silver_schema_version).
CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                -- Unix seconds UTC
);

-- One row per bronze dump run that has been ingested. snapshot_at
-- is parsed from the bronze dir name (YYYYMMDDTHHMMSSZ -> Unix
-- seconds UTC). dump_runs is the idempotency anchor: the loader
-- skips dumps whose snapshot_at already exists here.
--
-- mode, dry_run, errors_count are pulled from the bronze run.json
-- manifest so silver carries the bronze provenance.
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL,                -- run-ts dirname relative to bronze root (e.g. 'YYYYMMDDTHHMMSSZ'); same convention as documents.bronze_path
    mode                  TEXT,                            -- 'all' | 'accounts' | 'portfolios' | 'documents'
    dry_run               INTEGER NOT NULL DEFAULT 0,      -- 0/1; dry-run dumps are still recorded
    state_minted_at       INTEGER,                         -- Unix seconds; from the login session
    bronze_files_total    INTEGER NOT NULL DEFAULT 0,
    bronze_errors_total   INTEGER NOT NULL DEFAULT 0,
    payload               TEXT    NOT NULL                 -- the full bronze run.json verbatim
);


-- ============================================================
-- SNAPSHOT TABLES — slow-changing master data
-- ============================================================

-- One row per (snapshot, account). Relevate calls these
-- "portfolios" on the wire, but each one is presented
-- as a single account / depot — wealthdb gold's
-- `accounts` table is the right target.
--
-- Promoted columns: enough for filter / join / wealthdb-gold
-- projection without parsing JSON. Everything else (risk metrics,
-- targetInvestment, strategy, services, etc.) lives in payload.
--
-- Loader semantics: append-only per snapshot. The same
-- (snapshot_at, account_external_id) is unique because there's
-- one row per portfolio per bronze run.
CREATE TABLE accounts (
    snapshot_at           INTEGER NOT NULL,
    account_external_id   TEXT    NOT NULL,                -- portfolios[].externalId, e.g. 'NNNN.NNNNNN.N'
    portfolio_internal_id INTEGER NOT NULL,                -- portfolios[].id; per-endpoint key
    contact_id            INTEGER,                          -- portfolios[].contactId
    contact_group_id      INTEGER,                          -- portfolios[].contactGroupId
    -- product_key + product_name distinguish two LEGALLY SEPARATE
    -- foundations (Stiftungen) that offer the IDENTICAL investment
    -- menu, fees, and strategy UI — the two-foundation structure is
    -- a Swiss vested-benefits regulatory workaround, NOT a product-
    -- behaviour distinction. DO NOT branch taxonomy / classification
    -- (asset_class, tax_wrapper, management_style, account_kind, fee
    -- handling) on product_key. The column is surfaced for forensic
    -- attribution (which Stiftung holds this account) only. See
    -- DESIGN.md §7.1.1.
    product_key           TEXT,                             -- 'FZPF' | 'FZI' | ... (Relevate product code)
    product_name          TEXT,                             -- 'PensFree' | 'Independent' | ...
    product_offer_id      INTEGER,
    currency_code         TEXT    NOT NULL,                 -- ISO 4217, 'CHF' for all observed
    name                  TEXT,                             -- foundation-assigned display name
    portfolio_type_id     INTEGER,
    portfolio_status_id   INTEGER,
    portfolio_proposal_id INTEGER,                          -- proposalId for /modelportfolio endpoint
    is_active             INTEGER,                          -- 0/1
    is_read_only          INTEGER,                          -- 0/1
    first_investment_date TEXT,                             -- ISO 8601 from API; '0001-01-01T00:00:00' = sentinel
    create_date           TEXT,                             -- ISO 8601
    payload               TEXT    NOT NULL,                 -- the full portfolios[i] object
    PRIMARY KEY (snapshot_at, account_external_id)
);
CREATE INDEX ix_accounts_external_id
    ON accounts(account_external_id);
CREATE INDEX ix_accounts_internal_id
    ON accounts(portfolio_internal_id);


-- ============================================================
-- SNAPSHOT TABLES — per-snapshot state
-- ============================================================

-- Per-portfolio per-currency cash/value rollups, derived from
-- portfolios[i] fields in investment-overview. balance_kind values:
--   'cash'        — portfolios[i].cashBalance (free cash inside portfolio)
--   'invested'    — portfolios[i].investedAmount (amount put in by the holder)
--   'current'     — portfolios[i].currentValue (current valuation)
--   'securities'  — portfolios[i].securitiesBalance (non-cash holdings value)
--   'saving'      — portfolios[i].savingValuation
--   'investment'  — portfolios[i].investmentValuation
--   'virtual'     — portfolios[i].valuationVirtual
--   'target_inv'  — portfolios[i].targetInvestment
--   'target_sav'  — portfolios[i].targetSaving
-- Not every kind is present in every snapshot; the loader inserts
-- rows only for fields that are non-null in the source.
CREATE TABLE cash_balances (
    snapshot_at           INTEGER NOT NULL,
    account_external_id   TEXT    NOT NULL,
    currency              TEXT    NOT NULL,                 -- ISO 4217
    balance_kind          TEXT    NOT NULL,
    amount                REAL    NOT NULL,
    payload               TEXT,                             -- optional: source-field-name + raw
    PRIMARY KEY (snapshot_at, account_external_id, currency, balance_kind)
);

-- One row per (snapshot, account, instrument) from
-- modelportfolio.positions[]. THIS IS TARGET ALLOCATION — Relevate
-- doesn't surface actual unit holdings on the endpoints we've
-- mapped, only the model portfolio's recommended composition. If a
-- future endpoint exposes actual holdings a second table (or a
-- `source` column) can disambiguate.
--
-- allocation is a 0..1 fraction in observed payloads (verify in
-- loader; if 0..100 emerges, normalise consistently).
CREATE TABLE positions (
    snapshot_at            INTEGER NOT NULL,
    account_external_id    TEXT    NOT NULL,
    instrument_external_id TEXT    NOT NULL,                -- modelportfolio.positions[].security.id (numeric str)
    isin                   TEXT,                             -- modelportfolio.positions[].security.isin
    instrument_name        TEXT,
    asset_class            TEXT,                             -- security.assetClass.name
    asset_class_external   TEXT,                             -- security.assetClass.externalId
    country_code           TEXT,                             -- security.country.countryCode
    allocation             REAL,                             -- positions[].allocation (target)
    trading_price          REAL,                             -- security.tradingPrice
    trading_unit           TEXT,                             -- security.tradingUnit
    face_value             REAL,                             -- security.faceValue
    payload                TEXT    NOT NULL,                 -- the full positions[i] object
    PRIMARY KEY (snapshot_at, account_external_id, instrument_external_id)
);
CREATE INDEX ix_positions_isin
    ON positions(isin);
CREATE INDEX ix_positions_account
    ON positions(account_external_id);

-- Catalog of instruments seen across all modelportfolios. Slow-
-- changing master data — same security.id appearing in multiple
-- snapshots collapses to one row whose first_seen_at is the
-- earliest snapshot_at and whose last_seen_at advances on each
-- load.
CREATE TABLE instruments (
    instrument_external_id TEXT    NOT NULL PRIMARY KEY,    -- security.id
    isin                   TEXT,
    name                   TEXT,
    asset_class            TEXT,
    asset_class_external   TEXT,
    country_code           TEXT,
    first_seen_at          INTEGER NOT NULL,
    last_seen_at           INTEGER NOT NULL,
    payload                TEXT    NOT NULL                  -- the most-recent security{} object
);
CREATE INDEX ix_instruments_isin
    ON instruments(isin);

-- Performance time series. One row per (snapshot, account,
-- value_date) from /portfolio/{id}/performance values[]. Each
-- snapshot's values[] is the full history Relevate exposes for
-- that portfolio; re-loading a fresh dump REPLACES the prior
-- snapshot's points via the (snapshot_at, account, value_date)
-- PK collision being avoided by snapshot_at — distinct snapshots
-- get distinct rows, and the gold layer chooses the latest
-- snapshot per value_date.
CREATE TABLE performance_points (
    snapshot_at           INTEGER NOT NULL,
    account_external_id   TEXT    NOT NULL,
    value_date            INTEGER NOT NULL,                  -- Unix seconds UTC at midnight; values[].date
    currency              TEXT,                              -- performance.currency.currencyCode
    value                 REAL,                              -- values[].value
    amount                REAL,                              -- values[].amount
    cash_balance          REAL,                              -- values[].cashBalance
    securities_balance    REAL,                              -- values[].securitiesBalance
    cash_flow             REAL,                              -- values[].cashFlow
    deposits              REAL,                              -- values[].deposits
    payouts               REAL,                              -- values[].payouts
    profit                REAL,                              -- values[].profit
    profit_all            REAL,                              -- values[].profitAll
    profit_virtual        REAL,                              -- values[].profitVirtual
    amount_virtual        REAL,                              -- values[].amountVirtual
    additional_value      REAL,                              -- values[].additionalValue
    payload               TEXT    NOT NULL,                  -- the full values[i] row
    PRIMARY KEY (snapshot_at, account_external_id, value_date)
);
CREATE INDEX ix_performance_account_date
    ON performance_points(account_external_id, value_date);


-- ============================================================
-- EVENT TABLE — transactions
--
-- Populated by the loader from credit-note PDFs
-- (source='credit_note_pdf'), one row per parsed
-- Gutschriftsanzeige. The /deposits endpoint can return an
-- empty `{transactions:[]}` envelope for FZ accounts; if
-- Relevate exposes contributions / withdrawals via an
-- endpoint feed, those rows land here too.
-- ============================================================

CREATE TABLE transactions (
    transaction_external_id TEXT    NOT NULL PRIMARY KEY,
    snapshot_at             INTEGER NOT NULL,                -- dump that first observed this event
    occurred_at             INTEGER NOT NULL,                -- Unix seconds UTC
    account_external_id     TEXT    NOT NULL,
    instrument_external_id  TEXT,                             -- NULL for cash-only events
    kind                    TEXT    NOT NULL,                 -- 'contribution' | 'withdrawal' | 'fee' | 'rebalance' | 'other'
    currency                TEXT    NOT NULL,
    gross_amount            REAL,
    net_amount              REAL,
    quantity                REAL,
    price                   REAL,
    source                  TEXT    NOT NULL                  -- 'deposits_endpoint' | 'credit_note_pdf' | 'manual'
        CHECK (source IN ('deposits_endpoint', 'credit_note_pdf', 'manual')),
    payload                 TEXT    NOT NULL
);
CREATE INDEX ix_transactions_account_occurred
    ON transactions(account_external_id, occurred_at);


-- ============================================================
-- DOCUMENTS CATALOG
--
-- PDF binaries stay under <bronze-root>/<dump-ts>/documents/ — this
-- table indexes them. Content-deduped on content_sha256: the same
-- PDF fetched in multiple dumps collapses to one row whose
-- first_seen_at is the earliest dump that observed it and whose
-- last_seen_at advances on each subsequent dump.
--
-- doc_kind is the loader's best-effort human label, derived by
-- matching German + English needles against the
-- /middlelayer/v2/documents response's `fileName` field
-- (DOC_KIND_PATTERNS in load.py); fileNames that match no
-- needle fall through to 'other'. The numeric
-- `document_type_code` + `category_code` columns ARE
-- structured, but the enum-to-name mapping isn't published —
-- if it surfaces, a later migration can swap it in for the
-- fileName heuristic.
-- ============================================================

-- bronze_path is RELATIVE TO THE BRONZE ROOT (i.e. starts with
-- the run-ts dirname: '20260101T120000Z/documents/NNNN.pdf').
-- Consumers join with their own bronze root — works the same
-- inside the container (root = /data) and on the host (root =
-- $XDG_DATA_HOME/wealthdb/relevate). No absolute path stored, no container-
-- vs-host translation needed.
CREATE TABLE documents (
    content_sha256          TEXT    NOT NULL PRIMARY KEY,
    relevate_doc_id         INTEGER UNIQUE,                  -- documents[].id; NULL for manually-added PDFs
    relevate_external_id    TEXT,                             -- documents[].externalId
    file_name               TEXT    NOT NULL,
    file_size               INTEGER NOT NULL,
    doc_kind                TEXT    NOT NULL,                 -- heuristic from fileName
    document_type_code      INTEGER,                          -- documents[].documentType (numeric enum)
    category_code           INTEGER,                          -- documents[].category (numeric enum)
    document_year           INTEGER,                          -- documents[].documentYear; NULL when absent
    create_date             TEXT,                             -- ISO 8601 from API
    valid_till              TEXT,                             -- ISO 8601 from API; '9999-...' = never
    foundation_id           INTEGER,                          -- documents[].foundationId
    contract_id             INTEGER,                          -- documents[].contractId
    owner_id                INTEGER,                          -- documents[].ownerId
    bronze_path             TEXT    NOT NULL,                 -- '<run-ts>/documents/<docId>.pdf' relative to bronze root
    first_seen_at           INTEGER NOT NULL,                 -- earliest snapshot_at that captured this content
    last_seen_at            INTEGER NOT NULL,                 -- latest snapshot_at that captured this content
    payload                 TEXT    NOT NULL                  -- the documents[i] index entry verbatim
);
CREATE INDEX ix_documents_doc_id          ON documents(relevate_doc_id);
CREATE INDEX ix_documents_kind            ON documents(doc_kind);
CREATE INDEX ix_documents_year            ON documents(document_year);
CREATE INDEX ix_documents_first_seen      ON documents(first_seen_at);


-- ============================================================
-- Migration-complete marker. Must be the second-to-last statement
-- in the file (COMMIT closes the wrapping transaction); the loader
-- uses it to record the version.
-- ============================================================
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
