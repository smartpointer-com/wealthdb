-- ============================================================
-- schwab-web silver schema, migration 0001 — initial schema.
--
-- The web-scraped silver complements `schwab-api`'s
-- Trader-API silver. Gold-layer logic merges the two by splicing
-- transactions at the date Schwab's Trader API access started
-- (mid-2024) and falling back to the web feed for
-- everything earlier — see "Gold merge strategy" below.
--
-- Migration discipline: every change to the silver schema lands
-- as a new numbered file in this directory. The loader checks
-- the maximum applied silver_schema_version in schema_meta and
-- applies any newer migrations in order. Silver databases must
-- always conform to the latest schema; no backward-compatible
-- drift.
--
-- Storage conventions (mirror schwab-api + ubs-*):
--   * Unix-seconds-UTC integers for all timestamps.
--   * Stable filter columns promoted; rest in `payload` TEXT JSON.
--   * Snapshot tables monotemporal on snapshot_at (PK starts with
--     it, so the implicit B-tree serves as the as-of index).
--   * Event tables keyed by a stable external id.
--   * Content-dedup: insert only when canonical-JSON payload
--     differs from the most-recent row for the same key.
--
-- ------------------------------------------------------------
-- Identifier conventions (and irreconcilable differences with
-- schwab-api — flagged so the gold layer can join correctly)
-- ------------------------------------------------------------
--
--   account_external_id
--     The trailing 3-to-5-digit account suffix Schwab renders in
--     the UI ("…NNN"). NOT the same value space as
--     schwab-api's `account_external_id`, which is the
--     opaque `hashValue` returned by /accounts/accountNumbers.
--     Gold cannot join web↔api on this column directly. The
--     bridge is the FULL account number, which web statement
--     PDF filenames suffix verbatim
--     ("Brokerage-Statement_2026-04-30_NNN.PDF"): map suffix →
--     full number via a hand-maintained mapping or by parsing
--     the PDF header.
--
--   activity_id (transactions)
--     Schwab web has no stable per-transaction identifier.
--     Synthesised as the hex SHA-256 prefix of
--     "<account_external_id>|<date>|<amount>|<description>|
--      <symbol>|<index_within_statement>|<source_document_sha256>".
--     This is stable across re-loads but NOT join-able with
--     schwab-api's `activity_id`, which is the Schwab
--     activityId/orderId. Gold must match web↔api transactions
--     by (resolved account, timestamp, amount, description)
--     with tolerance, NOT by activity_id.
--
--   instrument_key
--     CUSIP > ticker, mirroring api's convention. Tax-form XML
--     (1099 Composite) always carries CUSIP; statement PDFs
--     often show ticker only in the activity rows but list CUSIP
--     in the positions / holdings sections. NULL for cash-only
--     transactions.
--
--   doc_sha256 (documents)
--     Content hash of the PDF/XML/CSV. No api equivalent — api
--     emits structured JSON, web emits opaque-blob documents.
--     The documents table is web-only and lets the gold layer
--     trace any silver row back to its original Schwab file.
--
-- ------------------------------------------------------------
-- Gold merge strategy (informational; implemented in `wealthdb`)
-- ------------------------------------------------------------
--
-- 1. Account matching:
--    Maintain a manual map (account_external_id_web → account_external_id_api)
--    derived from full account numbers. Web silver carries only
--    the suffix; gold widens it to the api hashValue via the map.
--    Without this map, web rows must remain unjoined to api.
--
-- 2. Transaction splicing:
--    Per resolved account:
--      web.transactions WHERE timestamp <  api_coverage_start
--      UNION ALL
--      api.transactions WHERE timestamp >= api_coverage_start
--    The splice is value-date-based; gold does NOT attempt
--    per-row matching across the boundary. Inside the api
--    window, the api feed wins (structured + activity_id).
--
-- 3. Position snapshots:
--    The api feed emits live positions; the web feed exposes
--    them only via 1099-B detail (annual). Gold prefers api
--    positions for any date api covers; web 1099 detail is a
--    fallback for pre-api years.
-- ============================================================

PRAGMA foreign_keys = ON;

-- One row per applied migration. Loader writes a row at the end of
-- each migration; current schema version = MAX(silver_schema_version).
CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                -- Unix seconds UTC
);

-- One row per bronze dump ingested. snapshot_at is parsed from
-- the bronze dir name (YYYYMMDDTHHMMSSZ → Unix seconds UTC).
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL                -- absolute path on disk at load time
);

-- ============================================================
-- SNAPSHOT TABLES
-- Monotemporal on snapshot_at. PK starts with snapshot_at, so
-- the implicit B-tree serves as the as-of index.
-- ============================================================

-- One row per (snapshot, account). Loader dedup: insert only when
-- the new canonical-JSON payload differs from the most-recent
-- payload for the same account_external_id.
--
-- `nickname` is promoted because it's the only user-visible name
-- attached to an account (the web UI doesn't expose Schwab's
-- internal account-type taxonomy directly — IRA vs ESA vs UTMA
-- etc. is conveyed only via the nickname the user set when the
-- account was opened).
CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,                 -- account suffix, e.g. "NNN"
    nickname            TEXT,                              -- e.g. "Brokerage Account …NNN"
    payload             TEXT    NOT NULL,                  -- {label, suffix, entry_id, ...}
    PRIMARY KEY (snapshot_at, account_external_id)
);

-- One row per (snapshot, downloaded artefact). Deduped naturally
-- by sha256 in PRIMARY KEY: the same PDF/XML/CSV content seen in
-- multiple bronze dumps collapses to one row whose snapshot_at
-- is the FIRST run that observed it.
--
-- doc_kind values mirror Schwab's filter-chip labels:
--   'statement'      Statements chip
--   'tax_form'       Tax Forms chip
--   'letter'         Letters chip
--   'report_or_plan' Reports & Plans chip
-- file_format values: 'pdf', 'xml', 'csv'.
CREATE TABLE documents (
    sha256              TEXT    NOT NULL PRIMARY KEY,
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    doc_date            INTEGER NOT NULL,                 -- Unix seconds UTC at midnight
    doc_kind            TEXT    NOT NULL,
    file_format         TEXT    NOT NULL,
    filename            TEXT    NOT NULL,                  -- Schwab-supplied download name
    size_bytes          INTEGER NOT NULL,
    payload             TEXT    NOT NULL                   -- {raw_type, raw_doc_name, ...}
);
CREATE INDEX ix_documents_account_date ON documents(account_external_id, doc_date);
CREATE INDEX ix_documents_kind         ON documents(doc_kind);

-- ============================================================
-- EVENT TABLE
-- Monotemporal on event-time. Load semantics: idempotent INSERT
-- OR IGNORE keyed by the synthetic activity_id. Re-loading the
-- same statement converges; re-parsing with an improved parser
-- requires an explicit DELETE WHERE payload_source = sha256 step
-- (see load.py).
-- ============================================================

-- One row per transaction extracted from a statement PDF or
-- transaction-history HTML capture. activity_id is the hex
-- SHA-256 prefix described in the header.
--
-- `kind` is the row's category as the parser observed it
-- (Sale, Purchase, Withdrawal, Deposit, CashDividend,
-- CreditInterest, NRATax, …). Not constrained at the schema
-- layer so new categories don't need a migration.
--
-- `source` distinguishes the bronze artefact this row came from.
-- Not constrained at the schema layer (mirroring schwab-api's
-- `kind`) so new sources can land without a migration. Known
-- values today:
--   'statement_pdf'     parsed by pdf_parsers from monthly /
--                       quarterly statement PDFs
--   'tx_history_json'   parsed by load.py from Schwab's
--                       Transaction-History "Export as JSON"
--                       download (covers the same logical
--                       events as 'statement_pdf' but
--                       Schwab-rendered, not parser-derived)
CREATE TABLE transactions (
    activity_id         TEXT    NOT NULL PRIMARY KEY,
    timestamp           INTEGER NOT NULL,                 -- Unix seconds UTC at midnight
    account_external_id TEXT    NOT NULL,
    kind                TEXT    NOT NULL,
    instrument_key      TEXT,
    source              TEXT    NOT NULL,
    source_sha256       TEXT    NOT NULL,                 -- sha256 of the bronze artefact
    payload             TEXT    NOT NULL
);
CREATE INDEX ix_transactions_account_timestamp
    ON transactions(account_external_id, timestamp);
CREATE INDEX ix_transactions_instrument_key
    ON transactions(instrument_key);
CREATE INDEX ix_transactions_source_sha256
    ON transactions(source_sha256);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s','now') AS INTEGER));
