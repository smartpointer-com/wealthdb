-- The plaid silver schema, as collectors/plaid/migrations builds it. The
-- adapter's tests run in a container that sees only the gold module, so the
-- schema is kept here too; collectors/plaid/tests/test_gold_schema.py fails
-- when the two differ in a column or an index.

PRAGMA foreign_keys = ON;

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL
);

-- One row per bronze run loaded. snapshot_at is the run's start, parsed
-- from its directory name.
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL,
    loaded_at             INTEGER NOT NULL
);

-- What each run read, product by product: run.json's entries. A product a
-- run did not read in full leaves earlier rows in place, so this table is
-- how a reader tells "read, and empty" from "not read".
CREATE TABLE run_products (
    run_at       INTEGER NOT NULL,      -- dump_runs.snapshot_at
    product      TEXT    NOT NULL,      -- accounts, holdings, liabilities, transactions, investment_transactions
    status       TEXT    NOT NULL,      -- fetched, partial, not_linked, absent, not_ready, failed
    rows         INTEGER,
    window_start INTEGER,               -- a ledger's window, both ends inclusive
    window_end   INTEGER,
    history      TEXT,                  -- the bank and card ledger: Plaid's transactions_update_status
    error_code   TEXT,
    PRIMARY KEY (run_at, product)
);

-- The Item, as each run saw it.
CREATE TABLE item_states (
    run_at                  INTEGER NOT NULL PRIMARY KEY,
    item_id                 TEXT    NOT NULL,
    environment             TEXT    NOT NULL,   -- production or sandbox
    institution_id          TEXT,
    institution_name        TEXT,
    products                TEXT    NOT NULL,   -- JSON array
    consent_expires_at      INTEGER,
    transactions_updated_at INTEGER,            -- Plaid's last successful update
    investments_updated_at  INTEGER,
    payload                 TEXT    NOT NULL
);

-- ============================================================
-- SNAPSHOT TABLE: accounts
-- Every account of the Item, restated by every run at its start. An
-- account a later run does not restate has left the Item.
-- ============================================================
CREATE TABLE accounts (
    snapshot_at        INTEGER NOT NULL,
    account_id         TEXT    NOT NULL,   -- Plaid's account_id
    name               TEXT,
    official_name      TEXT,
    mask               TEXT,               -- the last digits Plaid shows
    type               TEXT,               -- depository, credit, loan, investment, other
    subtype            TEXT,               -- checking, credit card, mortgage, ira, ...
    currency           TEXT,
    balance_current    TEXT,               -- as Plaid states it; see below
    balance_available  TEXT,
    balance_limit      TEXT,
    balance_updated_at INTEGER,            -- where Plaid states the balance's own time
    payload            TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_id)
);
-- balance_current keeps Plaid's sign: for a card or a loan it is the
-- amount owed, positive; for an investment account it is the total value
-- of the account. Reading it as cash or as a liability is the gold
-- adapter's job.

-- ============================================================
-- DIMENSION TABLE: securities
-- One row per Plaid security_id. A later run's description replaces an
-- earlier one; the seen range only widens.
-- ============================================================
CREATE TABLE securities (
    security_id             TEXT    NOT NULL PRIMARY KEY,
    name                    TEXT,
    ticker_symbol           TEXT,
    type                    TEXT,     -- equity, etf, mutual fund, fixed income, cash, derivative, cryptocurrency, loan, other
    subtype                 TEXT,
    currency                TEXT,
    cusip                   TEXT,
    isin                    TEXT,
    figi                    TEXT,
    cfi_code                TEXT,
    market_identifier_code  TEXT,
    is_cash_equivalent      INTEGER,  -- 0 or 1; NULL when Plaid states nothing
    institution_security_id TEXT,
    proxy_security_id       TEXT,
    first_seen_at           INTEGER NOT NULL,
    last_seen_at            INTEGER NOT NULL,
    payload                 TEXT    NOT NULL
);

-- ============================================================
-- SNAPSHOT TABLE: holdings
-- The holdings of every investment account, at the start of each run that
-- read them. An account that holds nothing has no row at that time. seq
-- tells apart two holdings of one security in one account.
-- ============================================================
CREATE TABLE holdings (
    snapshot_at             INTEGER NOT NULL,
    account_id              TEXT    NOT NULL,
    security_id             TEXT    NOT NULL,
    seq                     INTEGER NOT NULL DEFAULT 0,
    quantity                TEXT,
    institution_price       TEXT,
    institution_price_as_of INTEGER,
    institution_value       TEXT,
    cost_basis              TEXT,     -- the whole holding's cost, as Plaid states it
    currency                TEXT,
    vested_quantity         TEXT,
    vested_value            TEXT,
    tax_lots                TEXT    NOT NULL,   -- JSON array, as Plaid states it
    payload                 TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_id, security_id, seq)
);

-- ============================================================
-- EVENT TABLE: investment_transactions
-- Keyed by Plaid's id. A run that read the ledger in full replaces, per
-- account it covers, every row dated within its window.
-- ============================================================
CREATE TABLE investment_transactions (
    investment_transaction_id TEXT    NOT NULL PRIMARY KEY,
    account_id                TEXT    NOT NULL,
    security_id               TEXT,
    posted_at                 INTEGER NOT NULL,   -- posting date
    transaction_at            INTEGER,            -- when the order was placed, where stated
    name                      TEXT,
    type                      TEXT,               -- buy, sell, cash, fee, transfer, cancel
    subtype                   TEXT,
    amount                    TEXT    NOT NULL,   -- fleet sign: cash in positive
    quantity                  TEXT,               -- as Plaid states it
    price                     TEXT,
    fees                      TEXT,
    currency                  TEXT,
    cancel_transaction_id     TEXT,
    run_at                    INTEGER NOT NULL,   -- the run that stored the row
    payload                   TEXT    NOT NULL
);
CREATE INDEX ix_investment_transactions_account_posted
    ON investment_transactions (account_id, posted_at);

-- ============================================================
-- EVENT TABLE: transactions
-- The bank, card and loan ledger, keyed by Plaid's id. A run that read the
-- ledger in full replaces, per account it covers, every row dated within
-- its window. Every read also replaces those accounts' pending rows,
-- wherever they are dated. A charge that posts comes back under a new id.
-- A run that read the ledger while Plaid held only part of its history
-- adds and updates rows, and removes no posted row.
-- ============================================================
CREATE TABLE transactions (
    transaction_id         TEXT    NOT NULL PRIMARY KEY,
    account_id             TEXT    NOT NULL,
    posted_at              INTEGER NOT NULL,   -- posting date
    authorized_date        INTEGER,
    amount                 TEXT    NOT NULL,   -- fleet sign: money in positive
    currency               TEXT,
    name                   TEXT,
    merchant_name          TEXT,
    original_description   TEXT,               -- the institution's own text
    pending                INTEGER NOT NULL,   -- 0 or 1
    pending_transaction_id TEXT,
    category_primary       TEXT,               -- personal_finance_category, taxonomy v2
    category_detailed      TEXT,
    category_confidence    TEXT,
    category_version       TEXT,
    payment_channel        TEXT,
    transaction_code       TEXT,
    check_number           TEXT,
    merchant_category_code TEXT,
    run_at                 INTEGER NOT NULL,   -- the run that stored the row
    payload                TEXT    NOT NULL
);
CREATE INDEX ix_transactions_account_posted
    ON transactions (account_id, posted_at);

-- ============================================================
-- SNAPSHOT TABLE: liabilities
-- Card, mortgage and student-loan terms, restated by every run that read
-- them, at its start.
-- ============================================================
CREATE TABLE liabilities (
    snapshot_at                  INTEGER NOT NULL,
    account_id                   TEXT    NOT NULL,
    kind                         TEXT    NOT NULL,   -- credit, mortgage, student
    interest_rate                TEXT,   -- percent: a card's purchase APR, a loan's rate
    last_statement_balance       TEXT,   -- as Plaid states it: owed is positive
    last_statement_issue_date    INTEGER,
    next_payment_amount          TEXT,   -- the minimum, or a mortgage's monthly payment
    next_payment_due_date        INTEGER,
    origination_principal_amount TEXT,
    origination_date             INTEGER,
    payload                      TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_id, kind)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
VALUES (1, CAST(strftime('%s','now') AS INTEGER));
