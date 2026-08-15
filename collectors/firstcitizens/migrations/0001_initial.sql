-- ============================================================
-- firstcitizens silver schema, migration 0001 — initial schema.
--
-- Deposit-account (checking / savings) silver for the First Citizens Bank
-- retail relationship. Like the chase sibling it is a cash silver — accounts,
-- a transaction ledger, and a statement/document inventory; no positions,
-- instruments, or tax-lot detail. See ../DESIGN.md §3 for the Q2 `mobilews`
-- source shapes these columns are parsed from.
--
-- Migration discipline: every schema change lands as a new numbered file
-- here. The loader applies any migration newer than MAX(silver_schema_version)
-- in schema_meta, in order. Silver always conforms to the latest schema.
--
-- Storage conventions (mirror the other collectors):
--   * Unix-seconds-UTC integers for all timestamps.
--   * Stable filter columns promoted; everything else in `payload` TEXT JSON.
--   * Snapshot tables monotemporal on snapshot_at (PK starts with it, so the
--     implicit B-tree serves as the as-of index).
--   * Event tables keyed by a stable external id.
--
-- The schema is column-compatible with chase's (a deposit relationship
-- projects the same way), which lets the gold adapters run in parallel — the
-- differences are in what the loader fills, not the shape:
--   * `balance` is ALWAYS populated. The `accountHistory` JSON carries a
--     per-row `runningBalance`, so — unlike chase, where it came only from
--     the CSV export and was NULL for QFX-only rows — every row here has its
--     end-of-day balance, giving an exact cash time series with no join.
--   * There is no statement-PDF transaction backfill: the export/history
--     reach the account's full post-SVB-migration lifetime (deeper than the
--     ~2y of statements), so statements are documents only (DESIGN.md §4.3).
--
-- Identifier conventions:
--   account_external_id
--     The Q2 `mobilews` account key (the opaque `id` the /accountHistory,
--     /accountExport and /accountStatement endpoints take), as a string. NOT
--     the real account number — that is PII and never stored; the
--     human-facing mask (last 4) lives in accounts.mask.
--   fitid (transactions)
--     The `accountHistory` row's stable `transactionId` — First Citizens'
--     per-transaction id, present on every history row (no CSV↔QFX join is
--     needed; the JSON history is the authoritative ledger). A row missing an
--     id — never observed, but tolerated — gets a synthetic hex SHA-256
--     prefix of "<account_external_id>|<date>|<amount>|<description>|<check_number>".
--   doc_sha256 (documents)
--     Content hash of the statement PDF. Lets gold trace a silver row back to
--     its original First Citizens file.
-- ============================================================

PRAGMA foreign_keys = ON;

-- One row per applied migration. current schema version =
-- MAX(silver_schema_version).
CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                -- Unix seconds UTC
);

-- One row per bronze dump ingested. snapshot_at is parsed from the bronze
-- dir name (YYYYMMDDTHHMMSSZ → Unix seconds UTC).
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL                -- absolute path at load time
);

-- ============================================================
-- SNAPSHOT TABLE — accounts
-- Monotemporal on snapshot_at. Content-deduped by the loader: a new row
-- lands only when the canonical-JSON payload differs from the most-recent
-- row for the same account_external_id.
-- ============================================================
--
-- account_type is the Q2 product-type name as observed ('Checking',
-- 'Savings', …), not constrained at the schema layer so a new deposit
-- product doesn't need a migration. `mask` is the human-facing last-4
-- ("…1234"); the full number is never stored.
CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    account_type        TEXT,                              -- 'Checking', 'Savings', …
    nickname            TEXT,                              -- e.g. "Example Checking"
    mask                TEXT,                              -- last-4, e.g. "…1234"
    currency            TEXT,                              -- ISO code, e.g. 'USD'
    balance             REAL,                              -- current balance if captured
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

-- ============================================================
-- EVENT TABLE — transactions
-- Idempotent INSERT OR IGNORE keyed by fitid; re-loading the same history
-- converges. `amount` is signed the canonical way (positive = balance
-- increase / credit, debits negative). `kind` is a coarse 'CREDIT' / 'DEBIT'
-- label derived from the row's `isDebit`; the richer categorisation the
-- source may carry is preserved verbatim in `payload`. `balance` is the row's
-- `runningBalance` (always present).
-- ============================================================
CREATE TABLE transactions (
    fitid               TEXT    NOT NULL PRIMARY KEY,
    posted_at           INTEGER NOT NULL,                 -- Unix seconds UTC at midnight
    account_external_id TEXT    NOT NULL,
    amount              REAL    NOT NULL,                 -- signed; debits negative
    kind                TEXT,
    description         TEXT,
    check_number        TEXT,
    balance             REAL,                             -- per-row running balance
    source              TEXT    NOT NULL,                 -- 'history' (the authoritative JSON ledger)
    payload             TEXT    NOT NULL
);
CREATE INDEX ix_transactions_account_posted
    ON transactions(account_external_id, posted_at);

-- ============================================================
-- SNAPSHOT-DEDUPED TABLE — documents (statements)
-- Deduped by sha256 in the PK: the same PDF content across multiple dumps
-- collapses to one row whose snapshot_at is the FIRST run that observed it.
-- ============================================================
CREATE TABLE documents (
    sha256              TEXT    NOT NULL PRIMARY KEY,
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    doc_date            INTEGER,                           -- Unix seconds UTC at midnight, if known
    doc_kind            TEXT    NOT NULL,                  -- 'statement'
    file_format         TEXT    NOT NULL,                  -- 'pdf'
    filename            TEXT    NOT NULL,
    size_bytes          INTEGER NOT NULL,
    payload             TEXT    NOT NULL
);
CREATE INDEX ix_documents_account_date ON documents(account_external_id, doc_date);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s','now') AS INTEGER));
