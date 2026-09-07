-- ============================================================
-- amex silver schema, migration 0001 — initial schema.
--
-- Credit- and charge-card silver for the American Express relationship.
-- Like the chase card half it is a liability silver — accounts, a
-- transaction ledger, per-period statement balances, and a document
-- inventory; no positions, instruments, or tax-lot detail. See
-- ../DESIGN.md §C–§F for the source shapes these columns are parsed from.
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
-- ------------------------------------------------------------
-- SIGN CONVENTION — the one place this source differs from its siblings,
-- and the one that would be silently catastrophic to get wrong.
--
-- The fleet's silver card convention, set by chase, is the OFX one: SPEND IS
-- NEGATIVE, anything reducing the balance owed (a payment, a refund, a
-- statement credit) is POSITIVE, and the account's `balance` is the POSITIVE
-- amount owed. The gold adapter negates the balance into gold's canonical
-- negative cash, at one point.
--
-- Amex's own JSON and CSV state the opposite: a purchase is a POSITIVE
-- "amount charged" and a credit is negative. (Its QFX export uses the OFX
-- convention and agrees with the fleet — the two channels of one provider
-- disagree with each other, which is exactly how this is easy to miss.)
--
-- The loader therefore NEGATES every amount on the way in, so silver speaks
-- the fleet convention and the gold adapter needs no per-source special
-- case. `payload.provider_amount` keeps the provider's own signed figure for
-- traceability. Storing the JSON verbatim would turn every purchase into a
-- refund and every bill payment into spend.
--
-- ------------------------------------------------------------
-- Identifier conventions:
--   account_external_id
--     The `accountKey` — the 32-hex opaque key the servicing REST API takes,
--     as a string. NOT a card number; the human-facing mask lives in
--     accounts.mask, and the card number is never stored. The BFF's separate
--     `accountToken` is carried alongside in accounts.account_token, because
--     the two APIs take different keys for the same account.
--   txn_id (transactions)
--     The activity row's `identifier` — the provider's stable 18-digit
--     reference, which is ALSO the QFX `<FITID>` and (once its Excel quoting
--     is stripped) the CSV `Reference`. One id, four spellings: unlike chase,
--     no content-derived id is needed, because every channel agrees.
--
--     The exception is a PENDING row, whose identifier is provisional and
--     changes when the charge posts. Pending rows are marked `is_pending`
--     and are REPLACED wholesale per account on each load rather than
--     accumulated — see the loader — so a pending row that later posts does
--     not linger beside its posted self.
--
--     Statement-sourced rows carry a derived
--     `stmt:<account>:<period-end>:<index>` id instead: the document states
--     no reference of its own, so the loader keys the row on its period and
--     its position within it, which re-parsing reproduces.
--   sha256 (documents)
--     Content hash of the PDF, and the table's own key. Lets gold trace a
--     silver row back to the file it came from.
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
-- `balance` is the POSITIVE amount owed, as the provider states it (the sign
-- flip to a liability is gold's job, not silver's). `pending_charges` is the
-- unposted total the rolling activity view reports: it is the whole gap
-- between a balance reconstructed from the ledger and the balance the card
-- reports live, so keeping it turns that gap into a checkable identity.
CREATE TABLE accounts (
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,      -- accountKey (servicing API)
    account_token       TEXT,                  -- accountToken (functions BFF)
    display_name        TEXT,                  -- the product's own name
    mask                TEXT,                  -- the displayed last digits
    currency            TEXT,                  -- ISO code, e.g. 'USD'
    balance             REAL,                  -- POSITIVE amount owed
    pending_charges     REAL,                  -- unposted total, when reported
    payment_due_at      INTEGER,               -- Unix seconds UTC at midnight
    account_status      TEXT,                  -- 'Active', …
    line_of_business    TEXT,                  -- 'CONSUMER' / 'BUSINESS'
    user_type           TEXT,                  -- 'ACCOUNT_HOLDER', …
    payload             TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

-- ============================================================
-- EVENT TABLE — transactions
-- Idempotent INSERT OR IGNORE keyed by txn_id; re-loading the same activity
-- converges. Amounts follow the FLEET card convention (see the sign note
-- above): spend NEGATIVE, balance-reducing POSITIVE.
-- ============================================================
--
-- `posted_at` is the POST date — the date the balance moved, which is what a
-- balance series is keyed on. `txn_date` is the charge date, carried
-- alongside rather than substituted; the activity JSON states both, and a
-- statement-sourced row knows only the charge date and says so.
--
-- `category` is the provider's own spend category, resolved at load time
-- from the code → label map the same payload ships (../DESIGN.md §F), and
-- stored as free text rather than an enum: Amex publishes more values than
-- any one login produces, and a value this build has not seen must be able
-- to land without a migration. `category_code` keeps the raw code beside it.
-- Empty on a bill payment, which Amex leaves uncategorised — the same tell
-- Chase has, and what the gold adapter reads to tell a payment from a refund.
CREATE TABLE transactions (
    txn_id              TEXT    NOT NULL PRIMARY KEY,
    posted_at           INTEGER NOT NULL,      -- Unix seconds UTC at midnight
    account_external_id TEXT    NOT NULL,
    amount              REAL    NOT NULL,      -- signed; spend negative
    kind                TEXT,                  -- activity: 'DEBIT'/'CREDIT';
                                               -- statements: 'STMT_*'
    description         TEXT,                  -- the merchant line, verbatim
    merchant            TEXT,                  -- the merchant, as displayed
    category            TEXT,                  -- resolved provider category
    category_code       TEXT,                  -- the raw code (e.g. 'C1')
    txn_date            INTEGER,               -- charge date, when stated
    statement_end_at    INTEGER,               -- the cycle this row billed to
    currency            TEXT,                  -- NULL means the account's
    is_pending          INTEGER NOT NULL DEFAULT 0,
    source              TEXT    NOT NULL,      -- 'activity' | 'statement'
    payload             TEXT    NOT NULL
);
CREATE INDEX ix_transactions_account_posted
    ON transactions(account_external_id, posted_at);
CREATE INDEX ix_transactions_pending
    ON transactions(account_external_id, is_pending);

-- ============================================================
-- EVENT TABLE — statement_balances
-- One row per account per statement period: what the period opened and
-- closed at, and the addends between them.
--
-- The activity JSON states these for the ~24 months it reaches; the
-- statement PDFs state them for the provider's full retention. Either way
-- they are the only place a historic card balance is asserted — no channel
-- carries a running-balance column on a card — so this is what a
-- reconstructed balance series is anchored to.
--
-- Keyed on (account, period_end) so re-reading the same period converges;
-- `snapshot_at` names the bronze run the surviving copy came from.
-- `transactions_covered` records whether that period's ROWS reached silver,
-- which the balances themselves say nothing about.
-- ============================================================
CREATE TABLE statement_balances (
    account_external_id  TEXT    NOT NULL,
    period_start         INTEGER NOT NULL,     -- Unix seconds UTC at midnight
    period_end           INTEGER NOT NULL,     -- Unix seconds UTC at midnight
    opening              REAL,                 -- previous balance, as stated
    closing              REAL,                 -- new balance, as stated
    new_charges          REAL,
    payments_and_credits REAL,
    fees                 REAL,
    interest             REAL,
    transactions_covered INTEGER NOT NULL DEFAULT 0,
    source               TEXT    NOT NULL,     -- 'activity' | 'statement'
    snapshot_at          INTEGER NOT NULL,
    PRIMARY KEY (account_external_id, period_end)
);

-- ============================================================
-- SNAPSHOT-DEDUPED TABLE — documents
-- Deduped by sha256 in the PK: the same PDF content across multiple dumps
-- collapses to one row whose snapshot_at is the FIRST run that observed it.
-- ============================================================
CREATE TABLE documents (
    sha256              TEXT    NOT NULL PRIMARY KEY,
    snapshot_at         INTEGER NOT NULL,
    account_external_id TEXT    NOT NULL,
    doc_date            INTEGER,                -- Unix seconds UTC at midnight
    doc_kind            TEXT    NOT NULL,       -- 'statement' | 'year_end_summary'
    file_format         TEXT    NOT NULL,       -- 'pdf'
    filename            TEXT    NOT NULL,
    size_bytes          INTEGER NOT NULL,
    payload             TEXT    NOT NULL
);
CREATE INDEX ix_documents_account_date ON documents(account_external_id, doc_date);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s','now') AS INTEGER));
