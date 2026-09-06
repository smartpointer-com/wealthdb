-- ============================================================
-- ubs-web silver schema, migration 0007 — credit cards.
--
-- Migration 0001 stated that credit-card accounts were "intentionally
-- not modelled — this is a wealth-management silver, not personal
-- finance". That premise is retired: a card's ledger is where a
-- household's consumption actually is, and gold's spending engine reads
-- it. An applied migration is never edited, so this file supersedes that
-- note rather than correcting it in place.
--
-- Cards get their own three tables rather than joining `accounts` and
-- `transactions`, on the same reasoning migration 0002 used for the
-- historical tables: the identity model differs. A cash account is keyed
-- by IBAN and carries the columns that join it to the PSN feed
-- (`account_acct_id_psn_form`, `portfolio_external_id`); a card is keyed
-- by an opaque UBS token, has no PSN twin at all, and shares none of
-- those columns. Folding them together would mean relaxing the
-- `accounts.kind` CHECK, carrying a row of NULLs per card, and teaching
-- every existing query to filter a kind it never had to before.
--
-- Source: the netbanking card API, captured by cards.py (DESIGN.md §5),
-- not the CSV export. Every column below is a promotion of a field in
-- the stored JSON; `payload` keeps the row whole.
--
-- ------------------------------------------------------------
-- Sign convention
-- ------------------------------------------------------------
--
-- Amounts are stored exactly as UBS reports them, and UBS reports a card
-- as NEGATIVE-when-owed: a purchase is negative, a payment or refund
-- positive, and a statement's `balanceForward` is negative while the
-- card carries debt. That already matches gold's convention for a
-- liability held as negative cash, so nothing is flipped here and
-- nothing needs flipping downstream — the opposite of the chase silver,
-- which stores a positive amount owed and leaves the negation to its
-- adapter. Recorded because the two collectors disagreeing is exactly
-- the kind of thing a reader assumes away.
-- ============================================================

-- ============================================================
-- SNAPSHOT TABLE — card accounts
--
-- One row per (snapshot, card account). Content-dedup is NOT applied:
-- unlike the slow-changing master data in 0001, a card account's balance
-- moves daily, and the snapshot series is what a balance history is
-- reconstructed from.
-- ============================================================
CREATE TABLE card_accounts (
    snapshot_at              INTEGER NOT NULL,   -- Unix seconds UTC
    account_external_id      TEXT    NOT NULL,   -- UBS opaque card-account id
    account_number           TEXT,               -- the printed account number
    currency_iso             TEXT,
    -- Balance as UBS reports it: negative while the card owes. NULL when
    -- the roster did not carry one for this account.
    balance                  REAL,
    available                REAL,               -- remaining spending power
    credit_limit             REAL,
    -- Authorised-but-unposted activity, summed from the ledger's
    -- RESERVED rows. Those rows carry no id and cannot be stored
    -- individually (see card_transactions); this is the whole of what
    -- silver keeps about them, and it is the gap between a balance
    -- reconstructed from booked rows and the balance the card reports.
    reserved_amount          REAL,
    reserved_count           INTEGER,
    product_name             TEXT,               -- e.g. the card product line
    card_type                TEXT,               -- UBS internal type code
    account_status           TEXT,
    structure_type           TEXT,               -- e.g. a top-level account
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);


-- ============================================================
-- EVENT TABLE — card transactions
--
-- One row per BOOKED card transaction, keyed on the provider's own
-- opaque row id.
--
-- Why `_id` and not `transactionNr`: the latter is a one- to
-- three-digit sequence that repeats across hundreds of rows — a
-- position within a statement, not a key. `_id` is unique per row and
-- stable across fetches; where overlapping windows returned a row
-- twice, every repeat agreed field for field. So a re-download UPSERTs
-- rather than duplicating, and no occurrence index is needed (the
-- chase collector synthesises one only because its source offers no
-- usable id at all).
--
-- RESERVED rows are deliberately absent. A pending authorisation
-- carries no `_id`, no value date and no posting amount — nothing that
-- could key it and nothing that would let a re-fetch recognise it — and
-- it changes shape when it posts. Its magnitude is kept on
-- `card_accounts.reserved_amount` instead, which is the same place the
-- chase adapter keeps a card's pending charges.
-- ============================================================
CREATE TABLE card_transactions (
    transaction_external_id  TEXT    NOT NULL PRIMARY KEY,  -- minted `card:<hash>`
    account_external_id      TEXT    NOT NULL,   -- joins card_accounts
    snapshot_at              INTEGER NOT NULL,   -- dump that first captured it
    -- Two dates, both promoted: the purchase and the booking. A window
    -- fetch filters on the booking date (cards.py), so that is the one
    -- comparable with an invoice period.
    transaction_date         INTEGER,            -- purchase, Unix seconds UTC
    value_date               INTEGER NOT NULL,   -- booking, Unix seconds UTC
    -- Posted amount in the account's currency, signed UBS's way.
    amount                   REAL    NOT NULL,
    currency_iso             TEXT    NOT NULL,
    -- The amount as charged, before conversion. Equal to `amount` on a
    -- domestic row; the FX columns are populated only on a converted one.
    original_amount          REAL,
    original_currency_iso    TEXT,
    exchange_rate            REAL,
    -- The merchant descriptor as the terminal supplied it. This is the
    -- payee, and gold builds its merchant signature from it.
    merchant                 TEXT,
    -- The merchant CATEGORY in words — an ISO 18245 MCC description.
    -- Despite the API spelling it `merchantName`, it names a line of
    -- business, never a merchant. Free text, not an enum.
    merchant_category        TEXT,
    -- UBS's own coarser grouping over those descriptions.
    merchant_group_code      TEXT,
    card_number              TEXT,               -- which card booked it
    settled_in_invoice       INTEGER,            -- 1 once billed
    -- No foreign key to card_accounts: that table is a snapshot series,
    -- so `account_external_id` repeats there and cannot be a parent key.
    -- `transactions` relates to `accounts` the same way, for the same
    -- reason.
    payload                  TEXT    NOT NULL
);

-- The dominant read is one account's ledger over a date range: the gold
-- projection, and the per-period reconciliation against an invoice.
CREATE INDEX ix_card_transactions_account_value_date
    ON card_transactions(account_external_id, value_date);


-- ============================================================
-- EVENT TABLE — card invoices (billing periods)
--
-- One row per (account, period end). A UBS card statement exists
-- nowhere but here — the eDocuments archive carries no card category —
-- so this table is the only record of what a period opened and closed
-- at, and the only place the settlement date is stated.
--
-- The four figures reconcile exactly, and the loader checks it:
--
--     balance_forward + total_debit + total_credit == due_amount
--
-- which is the same gate the chase collector applies to a parsed
-- statement, arithmetic on structured fields rather than on a PDF
-- parse. `reconciles` records the outcome per period rather than
-- refusing the row: a period whose figures disagree is still the only
-- evidence that period exists, and a consumer that needs an anchor can
-- ask for the ones that add up.
-- ============================================================
CREATE TABLE card_invoices (
    account_external_id      TEXT    NOT NULL,
    period_end               INTEGER NOT NULL,   -- Unix seconds UTC
    period_start             INTEGER NOT NULL,
    invoice_external_id      TEXT    NOT NULL,
    snapshot_at              INTEGER NOT NULL,
    invoicing_date           INTEGER,
    -- The day the cash account is debited for this bill. NULL on the
    -- open period, which has not been billed yet. This is the date gold's
    -- internal-transfer matcher pairs the cash-side debit against.
    debiting_date            INTEGER,
    due_on                   INTEGER,
    -- Closing figure for the period, signed UBS's way (negative = owed).
    due_amount               REAL,
    minimal_due_amount       REAL,
    currency_iso             TEXT,
    -- Opening balance and the period's turnover. NULL until the
    -- per-invoice detail has been captured — the period list alone does
    -- not carry them.
    balance_forward          REAL,
    total_debit              REAL,
    total_credit             REAL,
    -- 1 when the four figures above satisfy the identity, 0 when they do
    -- not, NULL when the detail is missing and there was nothing to check.
    reconciles               INTEGER,
    statement_type           TEXT,               -- a closed period vs the open one
    payment_method           TEXT,               -- the settlement rail
    invoice_status           TEXT,
    -- Whether this period's transactions are in `card_transactions`.
    -- A function of which ledger windows have been loaded, so the loader
    -- recomputes it on every load rather than keeping the first answer;
    -- 0 is the safe default for a period no load has covered yet.
    transactions_covered     INTEGER NOT NULL DEFAULT 0,
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (account_external_id, period_end)
);

CREATE INDEX ix_card_invoices_debiting_date
    ON card_invoices(debiting_date);


-- ============================================================
-- DOCUMENT CATALOG — card statements
--
-- The statement PDFs live on disk under
-- <bronze-root>/<dump-ts>/cards/statements/<sha256>.pdf; this indexes
-- them. Separate from `documents` for the same reason the tables above
-- are separate: that table is keyed by the eDocuments API's own token,
-- which a card statement has none of.
-- ============================================================
CREATE TABLE card_statements (
    content_sha256           TEXT    NOT NULL PRIMARY KEY,
    account_external_id      TEXT    NOT NULL,
    invoice_external_id      TEXT    NOT NULL,
    period_end               INTEGER NOT NULL,
    file_path                TEXT    NOT NULL,   -- relative to bronze root
    size_bytes               INTEGER NOT NULL,
    snapshot_at              INTEGER NOT NULL
);

CREATE INDEX ix_card_statements_account ON card_statements(account_external_id);


INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (7, CAST(strftime('%s','now') AS INTEGER));
