-- ============================================================
-- erstebank_at silver schema, migration 0001 — initial schema.
--
-- Checking-account and credit-card silver for the George (Erste Bank und
-- Sparkassen, Austria) retail relationship: accounts, a transaction ledger
-- with the source's running balance, a daily closing-balance series derived
-- from it, the statement listings, and the statement PDF inventory. No
-- positions or instruments. ../DESIGN.md §C, §E and §G describe the source
-- shapes these columns are parsed from.
--
-- Migration discipline: every schema change lands as a new numbered file
-- here. The loader applies any migration newer than MAX(silver_schema_version)
-- in schema_meta, in order. Silver always conforms to the latest schema.
--
-- Storage conventions (mirror the other collectors):
--   * Unix-seconds-UTC integers for all timestamps. A date is the UTC
--     midnight of the calendar day George names; George sends Vienna
--     midnights, which the loader turns into that day first.
--   * Money as REAL in major units: George's {value, precision} integer
--     divided by 10^precision.
--   * Stable filter columns promoted; everything else in `payload` TEXT JSON.
--   * Snapshot tables monotemporal on snapshot_at (PK starts with it).
--   * Event tables keyed by a stable external id.
--
-- ------------------------------------------------------------
-- SIGN CONVENTION
--
-- Amounts follow the fleet convention, as George already states them:
-- money out (a payment, a card purchase) is NEGATIVE, money in (a deposit,
-- a refund, a card repayment) is POSITIVE.
--
-- Balances differ by account kind:
--   * checking — the balance as George states it: positive in credit,
--     negative in overdraft.
--   * credit_card — the POSITIVE amount owed, the fleet's card convention
--     (amex, chase). George states a card's balance NEGATIVE while money is
--     owed, so the loader negates it, in accounts.balance and in each card
--     row's running balance alike. The gold adapter turns the amount owed
--     into a liability.
--
-- ------------------------------------------------------------
-- Identifier conventions:
--   account_external_id
--     checking — the IBAN (no whitespace, uppercase), so an Austrian cash
--       account joins across sources on it, as in raiffeisen_at. George's
--       own opaque account id is kept in accounts.source_id.
--     credit_card — the card's shadow account id
--       (`creditCardShadowAccountId`). It keys the card's history and
--       statements, and outlives a replaced card. The card id is kept in
--       accounts.source_id.
--   txn_id (transactions)
--     The history row's stable `id`, present on every row.
--   sha256 (documents)
--     Content hash of the statement PDF.
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

-- The accounts each loaded run's rosters listed. `accounts` is
-- content-deduped, so it keeps no row for an unchanged account; this table
-- shows that a run still listed it.
CREATE TABLE run_accounts (
    snapshot_at         INTEGER NOT NULL,                 -- the run (dump_runs)
    account_external_id TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

-- ============================================================
-- SNAPSHOT TABLE — accounts
-- Monotemporal on snapshot_at. Content-deduped by the loader: a new row
-- lands only when the canonical-JSON payload differs from the most-recent
-- row for the same account_external_id.
-- ============================================================
--
-- The payload is the roster row (checking) or the card row (credit_card),
-- minus the holder's name and address blocks and the card image.
CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,                 -- IBAN / shadow account id
    account_kind        TEXT    NOT NULL,                 -- 'checking' | 'credit_card'
    source_id           TEXT,                             -- George account id / card id
    product             TEXT,                             -- George's product code
    display_name        TEXT,                             -- the product's own name
    mask                TEXT,                             -- last 4, e.g. "…1234"
    currency            TEXT,                             -- ISO code, e.g. 'EUR'
    balance             REAL,                             -- see SIGN CONVENTION
    credit_limit        REAL,                             -- card limit / approved overdraft
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

-- ============================================================
-- EVENT TABLE — transactions
-- INSERT OR REPLACE keyed by txn_id, runs applied oldest first, so a row
-- reads as the newest run that returned it. `balance` is the running
-- balance after the row, as the SIGN CONVENTION states it.
--
-- `balance_relevant` is 0 for an information row George shows in the list
-- but books for no amount (txType 'BANKINFO'). Such a row moves no balance.
-- ============================================================
CREATE TABLE transactions (
    txn_id              TEXT    NOT NULL PRIMARY KEY,
    account_external_id TEXT    NOT NULL,
    posted_at           INTEGER NOT NULL,                 -- booking date
    value_at            INTEGER,                          -- value date
    amount              REAL    NOT NULL,                 -- signed; money out negative
    currency            TEXT,                             -- ISO code, e.g. 'EUR'
    balance             REAL,                             -- running balance after the row
    balance_relevant    INTEGER NOT NULL DEFAULT 1,
    tx_type             TEXT,                             -- e.g. 'POSPAYMENT', 'CREDITCARD'
    direction           TEXT,                             -- 'INCOMING' | 'OUTGOING'
    description         TEXT,                             -- the row's title
    counterparty        TEXT,                             -- partner, else merchant name
    reference           TEXT,                             -- the free-text lines, joined
    snapshot_at         INTEGER NOT NULL,                 -- run that last wrote this row
    payload             TEXT    NOT NULL
);
CREATE INDEX ix_transactions_account_posted
    ON transactions(account_external_id, posted_at);

-- ============================================================
-- DERIVED TABLE — daily_balances
-- The closing balance of each day an account booked a balance-relevant
-- row, derived from the running balances in `transactions`. Rebuilt from
-- the whole ledger on every load that ingests a run, so it never needs
-- patching. Same sign as transactions.balance.
-- ============================================================
CREATE TABLE daily_balances (
    account_external_id TEXT    NOT NULL,
    balance_date        INTEGER NOT NULL,                 -- Unix seconds UTC at midnight
    balance             REAL    NOT NULL,                 -- closing balance that day
    PRIMARY KEY (account_external_id, balance_date)
);

-- ============================================================
-- SNAPSHOT TABLE — statements
-- The statement listings: one row per statement George lists, whether or
-- not its PDF was downloaded. Keyed (account, statement id); the newest
-- run's row wins. Figures stay in the payload, as George states them.
-- ============================================================
CREATE TABLE statements (
    account_external_id TEXT    NOT NULL,
    statement_id        TEXT    NOT NULL,                 -- statement id / card billId
    statement_kind      TEXT    NOT NULL,                 -- 'statement' | 'card_statement'
    period_start        INTEGER,                          -- first day covered
    period_end          INTEGER,                          -- last day covered
    statement_date      INTEGER,                          -- the day it was issued
    snapshot_at         INTEGER NOT NULL,
    payload             TEXT    NOT NULL,
    PRIMARY KEY (account_external_id, statement_id)
);

-- ============================================================
-- SNAPSHOT-DEDUPED TABLE — documents (statement PDFs)
-- Deduped by sha256 in the PK: the same PDF content across several dumps
-- collapses to one row whose snapshot_at is the FIRST run that held it.
-- ============================================================
CREATE TABLE documents (
    sha256              TEXT    NOT NULL PRIMARY KEY,
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    doc_date            INTEGER,                          -- Unix seconds UTC at midnight
    doc_kind            TEXT    NOT NULL,                 -- 'statement' | 'card_statement'
    file_format         TEXT    NOT NULL,                 -- 'pdf'
    filename            TEXT    NOT NULL,
    size_bytes          INTEGER NOT NULL,
    payload             TEXT    NOT NULL
);
CREATE INDEX ix_documents_account_date ON documents(account_external_id, doc_date);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s','now') AS INTEGER));
