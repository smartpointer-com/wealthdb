-- ============================================================
-- chase silver schema, migration 0001 — initial schema.
--
-- Deposit-account (checking / savings) silver for the Chase retail
-- relationship. Far simpler than a brokerage silver: cash accounts, a
-- transaction ledger, and a statement/document inventory — no positions,
-- instruments, or tax-lot detail. See ../DESIGN.md "Observed" for the
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
-- Identifier conventions:
--   account_external_id
--     Chase's `digital-account-identifier` / `accountId` for the account
--     (the opaque numeric id the /svc/ API and export forms key on), as a
--     string. NOT the real account number — that is PII and never stored;
--     the human-facing mask (last 4) lives in accounts.mask.
--   fitid (transactions)
--     The OFX `<FITID>` from the QFX export — Chase's stable per-transaction
--     id, which the CSV export lacks. This is why download captures QFX
--     alongside CSV: QFX supplies the stable key + a clean type/name/memo
--     split, CSV supplies the per-row running balance (DESIGN.md §E). Rows
--     that appear only in CSV (e.g. pending items QFX omits) get a synthetic
--     id — the hex SHA-256 prefix of
--     "<account_external_id>|<date>|<amount>|<description>|<check_number>".
--   doc_sha256 (documents)
--     Content hash of the statement PDF. Lets gold trace a silver row back
--     to its original Chase file.
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
-- account_type is Chase's own code as observed ('CHK' checking, 'SAV'
-- savings, …), not constrained at the schema layer so a new deposit
-- product doesn't need a migration. `mask` is the human-facing last-4
-- ("…1234"); the full number is never stored.
CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    account_type        TEXT,                              -- 'CHK', 'SAV', …
    nickname            TEXT,                              -- e.g. "TOTAL CHECKING"
    mask                TEXT,                              -- last-4, e.g. "…1234"
    currency            TEXT,                              -- ISO code, e.g. 'USD'
    balance             REAL,                              -- current balance if captured
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

-- ============================================================
-- EVENT TABLE — transactions
-- Idempotent INSERT OR IGNORE keyed by fitid; re-loading the same export
-- converges. `kind` is the OFX TRNTYPE / CSV Type as observed (DEBIT,
-- CREDIT, CHECK, DSLIP, …), unconstrained so new categories need no
-- migration. `balance` is the CSV per-row running balance joined onto the
-- QFX row (NULL when only QFX saw the row, e.g. a pending item).
-- ============================================================
CREATE TABLE transactions (
    fitid               TEXT    NOT NULL PRIMARY KEY,
    posted_at           INTEGER NOT NULL,                 -- Unix seconds UTC at midnight
    account_external_id TEXT    NOT NULL,
    amount              REAL    NOT NULL,                 -- signed; debits negative
    kind                TEXT,
    description         TEXT,
    check_number        TEXT,
    balance             REAL,                             -- CSV running balance, if joined
    source              TEXT    NOT NULL,                 -- 'qfx' | 'csv' (which export keyed the row)
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
