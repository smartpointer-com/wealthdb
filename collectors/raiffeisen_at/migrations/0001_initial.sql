-- ============================================================
-- raiffeisen_at silver schema, migration 0001 — initial schema.
--
-- Deposit-account (checking / savings) silver for the Austrian Raiffeisen
-- (Mein ELBA) retail relationship. Like the chase / firstcitizens siblings it
-- is a cash silver — accounts, a transaction ledger, a daily-balance series,
-- and a statement/document inventory; no positions, instruments, or tax-lot
-- detail. See ../DESIGN.md §3-Observed for the REST source shapes these
-- columns are parsed from.
--
-- Migration discipline: every schema change lands as a new numbered file
-- here. The loader applies any migration newer than MAX(silver_schema_version)
-- in schema_meta, in order. Silver always conforms to the latest schema.
--
-- Storage conventions (mirror the other collectors):
--   * Unix-seconds-UTC integers for all timestamps.
--   * Stable filter columns promoted; everything else in `payload` TEXT JSON.
--   * Snapshot tables monotemporal on snapshot_at (PK starts with it).
--   * Event tables keyed by a stable external id.
--
-- Two shape differences from the US siblings, both from the source (DESIGN.md
-- §C), not the projection:
--   * The transaction history carries NO per-row running balance, so
--     transactions have no `balance` column. The closing-balance series comes
--     from a dedicated `daily_balances` table (the `kontostaende` endpoint) —
--     the axis the gold adapter reads instead of a per-row balance.
--   * Each row carries both a booking date (`buchungstag` → posted_at) and a
--     value date (`valuta` → value_at); both are kept.
--
-- Identifier conventions:
--   account_external_id
--     The IBAN (no whitespace, uppercase — the form the API returns), the
--     account key every Mein ELBA endpoint takes. This matches the ubs-web /
--     ubs-psn convention, so an Austrian cash account joins across sources on
--     the IBAN. The silver DB lives in the private data tree (like bronze) and
--     is never committed; a last-4 mask ("…1234") is also stored for display.
--   txn_id (transactions)
--     The `kontoumsaetze` row's stable `id`, present on every history row. A
--     row missing one — never observed, but tolerated — gets a synthetic
--     hex SHA-256 id prefixed "syn_" over
--     "<iban>|<buchungstag>|<amount>|<description>".
--   sha256 (documents)
--     Content hash of the statement PDF. Lets gold trace a silver row back to
--     its original Kontoauszug file.
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
-- account_type is the Kontoart from the account-information detail
-- ('Gehaltekonto', a savings-account label, …) when captured, else the roster
-- product type ('KONTO'); not constrained at the schema layer so a new deposit
-- product needs no migration. `mask` is the human-facing last-4 ("…1234"); the
-- full IBAN is the external id, not a separate stored account number. The
-- `payload` folds the roster row together with the curated deposit-relevant
-- detail attributes (currency, institution, BIC, the Zinssatz Soll/Haben
-- interest rates) — the card block and the account-holder name are dropped.
CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,                 -- the IBAN
    account_type        TEXT,                             -- Kontoart, e.g. 'Gehaltekonto'
    nickname            TEXT,                             -- user account label, if any
    mask                TEXT,                             -- last-4, e.g. "…1234"
    currency            TEXT,                             -- ISO code, e.g. 'EUR'
    balance             REAL,                             -- current balance if captured
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

-- ============================================================
-- EVENT TABLE — transactions
-- Idempotent INSERT OR IGNORE keyed by txn_id; re-loading the same history
-- converges. `amount` is signed as the source gives it (positive = credit /
-- balance increase, negative = debit). `kind` is a coarse 'CREDIT' / 'DEBIT'
-- label derived from that sign; the source's own `kategorieCode` is promoted
-- to `category` and the full row is preserved in `payload`. There is no
-- per-row balance (see daily_balances).
-- ============================================================
CREATE TABLE transactions (
    txn_id              TEXT    NOT NULL PRIMARY KEY,
    account_external_id TEXT    NOT NULL,                 -- the IBAN
    posted_at           INTEGER NOT NULL,                 -- buchungstag, Unix seconds UTC at midnight
    value_at            INTEGER,                          -- valuta (value date), if present
    amount              REAL    NOT NULL,                 -- signed; debits negative
    currency            TEXT,                             -- ISO code, e.g. 'EUR'
    kind                TEXT,                             -- coarse 'CREDIT' / 'DEBIT'
    category            TEXT,                             -- source kategorieCode, e.g. 'income_other'
    description         TEXT,                             -- Verwendungszweck (purpose)
    counterparty        TEXT,                             -- Transaktionsteilnehmer (other party)
    source              TEXT    NOT NULL,                 -- 'history' (the kontoumsaetze ledger)
    payload             TEXT    NOT NULL
);
CREATE INDEX ix_transactions_account_posted
    ON transactions(account_external_id, posted_at);

-- ============================================================
-- EVENT TABLE — daily_balances
-- The `kontostaende` daily closing-balance series (one row per day the account
-- had a closing balance). Keyed (account, day); INSERT OR REPLACE so a later
-- run refreshes a still-settling recent day, while settled past days are
-- stable. This is the cash time series the gold adapter reads (the history has
-- no per-row balance).
-- ============================================================
CREATE TABLE daily_balances (
    account_external_id TEXT    NOT NULL,                 -- the IBAN
    balance_date        INTEGER NOT NULL,                 -- Unix seconds UTC at midnight
    balance             REAL    NOT NULL,                 -- closing saldo that day
    snapshot_at         INTEGER NOT NULL,                 -- run that last wrote this row
    PRIMARY KEY (account_external_id, balance_date)
);

-- ============================================================
-- SNAPSHOT-DEDUPED TABLE — documents (statements)
-- Deduped by sha256 in the PK: the same PDF content across multiple dumps
-- collapses to one row whose snapshot_at is the FIRST run that observed it.
-- ============================================================
CREATE TABLE documents (
    sha256              TEXT    NOT NULL PRIMARY KEY,
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,                 -- the IBAN
    doc_date            INTEGER,                          -- Unix seconds UTC at midnight, if known
    doc_kind            TEXT    NOT NULL,                 -- 'statement'
    file_format         TEXT    NOT NULL,                 -- 'pdf'
    filename            TEXT    NOT NULL,
    size_bytes          INTEGER NOT NULL,
    payload             TEXT    NOT NULL
);
CREATE INDEX ix_documents_account_date ON documents(account_external_id, doc_date);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s','now') AS INTEGER));
