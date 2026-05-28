-- ============================================================
-- ubs-web silver schema, migration 0001 — initial schema.
--
-- The web-scraped silver complements the PSN-fed silver
-- (`ubs-psn`, `~/wealthdb/ubs-psn/ubs-psn.db`). Gold-layer logic
-- merges the two by splicing transactions at the date PSN's feed
-- went live for each banking relationship and unioning snapshots
-- modulo content-dedup. The schema below is built to make that
-- merge straightforward.
--
-- Migration discipline: every change to the silver schema lands
-- as a new numbered file in this directory. The loader checks
-- the maximum applied silver_schema_version in schema_meta and
-- applies any newer migrations in order. Silver databases must
-- always conform to the latest schema; no backward-compatible
-- drift.
--
-- Storage convention: timestamps are INTEGER Unix seconds (UTC).
-- Stable filter columns are promoted; everything else lives in
-- the `payload` JSON column. Mirror of the PSN silver convention.
--
-- ------------------------------------------------------------
-- Identifier conventions (designed to align with ubs-psn)
-- ------------------------------------------------------------
--
--   account_external_id
--     For cash accounts: the IBAN with NO whitespace, uppercase
--       (e.g. 'CHKKBBBBRRRRAAAAAAAAC'). This matches the form
--       gold can derive from the PSN AcctId by reversing UBS's
--       internal padding. PSN's silver stores the same IBAN here.
--     For safekeeping accounts (when the web exposes them): the
--       UBS safekeeping code (e.g. 'BBBB-AAAAAAAA.S1'), matching
--       PSN.
--     Credit-card accounts are intentionally not modelled — this
--     is a wealth-management silver, not personal finance.
--
--   account_acct_id_psn_form
--     A PARALLEL column on `accounts` holding the AcctId form PSN
--     would emit for the same account: 21-char zero-padded string
--     like 'RRRR000000AAAAAAAA0000C'. Computed from the IBAN by the
--     loader. Lets gold join PSN.cash_accounts directly without
--     needing IBAN-checksum arithmetic.
--
--   portfolio_external_id
--     The trailing UBS portfolio code, e.g. 'RNNN', matching
--     PSN.portfolios.portfolio_external_id (`PrtflId` in SDPO).
--     The full positions.csv "Portfolio" string ('BBBB AAAAAAAA
--     RNNN') is stored separately in portfolio_full_id for
--     traceability.
--
--   banking_relationship_id
--     The UBS opaque `bankingRelationId` token harvested from the
--     SPA URLs. There is NO direct equivalent in PSN — PSN keys
--     by 'SFTPCH01' / 'SFTPCH02' which are SFTP server identifiers,
--     not customer-side banking-relationship handles. Gold has to
--     match via `banking_relationship_label` and / or
--     `account_number_prefix` (e.g. 'BBBB AAAAAAAA') which is a
--     constant slice of every account ID under that relationship.
--
--   transaction_external_id
--     UBS "Transaction no." column from the CSV export (e.g.
--     '8830131TO4290735'). PSN derives the same identifier from
--     the MT940 `:61:` `<bank_ref>` field; gold can use it for
--     cross-checks but the primary merge is value-date-based
--     splice (see "Gold merge strategy" below).
--
--   isin / valor
--     ISO 6166 ISIN-12 and Swiss "Valor" number, both as in PSN.
--
-- ------------------------------------------------------------
-- Gold merge strategy (informational; implemented in wealthdb)
-- ------------------------------------------------------------
--
-- 1. Banking-relationship matching (one-time config):
--    The gold layer needs an explicit map
--       (web banking_relationship_id) -> (PSN relationship_id)
--    because the two ID spaces have no shared key. Suggested
--    config: human-readable label on web silver +
--    SFTPCH01/SFTPCH02 in PSN.
--
-- 2. Account matching:
--    Web `account_external_id` (= IBAN no-spaces) joins PSN
--    `cash_accounts.account_external_id` directly when PSN also
--    stores the IBAN form (it does per `psn/migrations/0001`).
--    Card accounts have no PSN twin → gold uses web only.
--
-- 3. Transaction splicing:
--    For each (account_external_id, banking-relationship-pair):
--      web.transactions WHERE value_date  <  PSN_start_date
--      UNION ALL
--      PSN .events       WHERE value_date >= PSN_start_date
--    `value_date` is the splice key; gold does NOT attempt
--    per-row matching across the boundary. The promoted
--    value_date column on web.transactions is exactly the
--    timestamp PSN would put on `events.timestamp`.
--
-- 4. Position snapshots:
--    Both feeds emit complete snapshots. Gold prefers PSN
--    snapshots for dates PSN covers (daily, machine-format)
--    and falls back to web `positions` snapshots otherwise.
--
-- 5. Documents:
--    PDF documents are web-only. PSN has no document concept.
--    Gold surfaces them as a flat table referenced by
--    account_external_id and / or portfolio_external_id when
--    derivable from the label.
-- ============================================================

PRAGMA foreign_keys = ON;

-- One row per applied migration. Loader writes a row at the end
-- of each migration; current schema version = MAX(silver_schema_version).
CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                -- Unix seconds UTC
);

-- One row per dump run, regardless of whether any sub-artefact
-- had rows. Lets queries distinguish "no dump on date X" from
-- "dump on date X had no transactions / no documents / etc."
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,   -- Unix seconds UTC
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL,               -- absolute path of dump directory
    transactions_since    INTEGER,                        -- Unix seconds UTC, --since
    transactions_until    INTEGER,                        -- Unix seconds UTC, --until
    documents_since       INTEGER,                        -- Unix seconds UTC
    documents_until       INTEGER                         -- Unix seconds UTC
);


-- ============================================================
-- SNAPSHOT TABLES — slow-changing master data
--
-- These records change rarely (account opened, portfolio renamed).
-- Loader applies content-dedup against the most-recent row for
-- each entity: insert a new snapshot row only when the canonical-
-- JSON payload differs.
-- ============================================================

-- One row per banking relationship per snapshot. The web SPA only
-- exposes one banking_relationship_id per session (the one the
-- user has switched into); users with multiple relationships
-- must re-run download.py once per relationship after switching
-- in the UI. The `description`
-- column is empty by default — fill it via a manual UPDATE so
-- gold can map this row to the correct PSN relationship_id.
CREATE TABLE banking_relationships (
    snapshot_at              INTEGER NOT NULL,
    banking_relationship_id  TEXT    NOT NULL,            -- UBS opaque token
    account_number_prefix    TEXT,                        -- e.g. 'BBBB AAAAAAAA'
    description              TEXT,                        -- optional human label
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, banking_relationship_id)
);

-- One row per portfolio per snapshot. `portfolio_external_id`
-- matches PSN.portfolios.portfolio_external_id; `portfolio_uid`
-- is the UBS opaque token used in SPA URLs.
-- Portfolios are wealth-management wrappers (strategy / mandate /
-- product) that GROUP accounts but never hold cash or securities
-- themselves — that happens at the cash-account and safekeeping-
-- account layer. The link is `accounts.portfolio_external_id`
-- (nullable; standalone accounts are common). `base_currency` is
-- the portfolio's reporting currency, harvested from the
-- positions.csv "Valued in: <CCY>" footer line; aligns with PSN's
-- `PrtflKey.PrtflCcyIsoCd`.
CREATE TABLE portfolios (
    snapshot_at              INTEGER NOT NULL,
    portfolio_external_id    TEXT    NOT NULL,            -- e.g. 'RNNN'
    banking_relationship_id  TEXT,
    portfolio_full_id        TEXT,                        -- e.g. 'BBBB AAAAAAAA RNNN'
    portfolio_uid            TEXT,                        -- UBS opaque token
    base_currency            TEXT,                        -- e.g. 'CHF', from "Valued in:" footer
    description              TEXT,                        -- human label if discoverable
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, portfolio_external_id)
);

-- One row per (snapshot, account). Covers cash and (where the web
-- exposes them in the future) safekeeping accounts. Credit-card
-- accounts are intentionally not modelled.
-- `account_external_id` is the canonical join key (see header).
CREATE TABLE accounts (
    snapshot_at              INTEGER NOT NULL,
    account_external_id      TEXT    NOT NULL,            -- IBAN no-spaces upper
    kind                     TEXT    NOT NULL
        CHECK (kind IN ('cash', 'safekeeping')),
    iban                     TEXT,                        -- formatted IBAN with spaces
    account_acct_id_psn_form TEXT,                        -- 21-char PSN-style AcctId
    account_number_raw       TEXT,                        -- 'BBBB AAAAAAAA.40' form
    account_opaque_id        TEXT,                        -- UBS URL-resolvable token
    banking_relationship_id  TEXT,
    portfolio_external_id    TEXT,                        -- which portfolio it sits under
    currency_iso             TEXT,
    description              TEXT,                        -- 'CHF Checking, UBS Personal Account, …'
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);


-- ============================================================
-- SNAPSHOT TABLES — daily state
--
-- One row per (snapshot, portfolio, account, instrument).
-- Aligns with PSN's holdings + cash_balances tables:
--   instrument_isin NULL  -> cash position (matches PSN cash_balances)
--   instrument_isin set   -> securities position (matches PSN holdings)
-- ============================================================

-- `currency_iso` is the INSTRUMENT currency (USD for a US stock,
-- EUR for a EUR cash sub-account, etc.). `market_value` is in the
-- PORTFOLIO BASE currency named by `market_value_currency` —
-- positions.csv only exposes the base-currency value, not the
-- instrument-currency one, so there is no second column for it.
-- The base currency is harvested from the positions.csv footer
-- ("Valued in: CHF"); for UBS Switzerland portfolios it is CHF.
CREATE TABLE positions (
    snapshot_at              INTEGER NOT NULL,
    portfolio_external_id    TEXT    NOT NULL,
    account_external_id      TEXT    NOT NULL,            -- IBAN for cash; '' for securities-only rows
    instrument_isin          TEXT,                        -- NULL for cash positions
    valor                    TEXT,
    currency_iso             TEXT    NOT NULL,            -- INSTRUMENT currency
    units                    REAL,                        -- "Number/Amt." column
    market_value             REAL,                        -- in `market_value_currency`
    market_value_currency    TEXT,                        -- portfolio base ccy from positions.csv footer
    cost_price               REAL,                        -- web-only; PSN doesn't carry cost
    accrued_interest         REAL,
    lending_value            REAL,
    lending_value_ratio      REAL,
    description              TEXT,                        -- e.g. 'Shs Example Materials AG'
    payload                  TEXT    NOT NULL,            -- full positions.csv row
    PRIMARY KEY (snapshot_at, portfolio_external_id,
                 account_external_id, instrument_isin)
);


-- ============================================================
-- EVENT TABLE — transactions
--
-- One row per UBS transaction. Promotes `value_date` as the
-- splice key gold uses against PSN.events.timestamp. UBS's
-- "Transaction no." serves as the natural PK; re-running a
-- download window safely UPSERTs the same row.
--
-- Reload semantics: row-level INSERT OR REPLACE on the
-- transaction_external_id PK. Window deletion is unnecessary
-- because UBS never RETRACTS transactions in this UI (each is
-- final once booked); corrections produce a new debit/credit
-- pair, not an edit.
-- ============================================================

-- Compound PK (transaction_external_id, account_external_id):
-- UBS uses the SAME Transaction no. for both sides of an inter-
-- account transfer (the debit row in the source account and the
-- credit row in the destination account share the ID). A PK on
-- transaction_external_id alone would UPSERT one side away — we
-- need both sides in silver to splice cleanly per account.
CREATE TABLE transactions (
    transaction_external_id  TEXT    NOT NULL,             -- UBS Transaction no.
    account_external_id      TEXT    NOT NULL,             -- joins accounts.account_external_id
    snapshot_at              INTEGER NOT NULL,             -- which dump first captured this row
    trade_date               INTEGER,                      -- Unix seconds UTC; CSV "Trade date"
    booking_date             INTEGER,                      -- CSV "Booking date"
    value_date               INTEGER NOT NULL,             -- CSV "Value date" — gold splice key
    currency_iso             TEXT    NOT NULL,
    amount_debit             REAL,
    amount_credit            REAL,
    counterparty             TEXT,                         -- extracted from description1
    description_kind         TEXT,                         -- "Dividend", "e-banking payment order", etc.
    payload                  TEXT    NOT NULL,             -- all four description columns + footnotes
    PRIMARY KEY (transaction_external_id, account_external_id)
);

-- Time-range queries on a single account are the dominant access
-- pattern for the splice step.
CREATE INDEX ix_transactions_account_value_date
    ON transactions(account_external_id, value_date);


-- ============================================================
-- DOCUMENTS CATALOG
--
-- PDF binary stays on disk under <bronze-root>/<dump-ts>/documents/
-- — this table indexes them. `doc_token` is UBS's stable URL token;
-- `content_sha256` lets us dedup the same PDF across re-fetches.
-- ============================================================

CREATE TABLE documents (
    doc_token                TEXT    NOT NULL PRIMARY KEY, -- UBS API token
    content_sha256           TEXT    NOT NULL UNIQUE,
    file_path                TEXT    NOT NULL,             -- relative to bronze-root
    size_bytes               INTEGER NOT NULL,
    snapshot_at              INTEGER NOT NULL,             -- which dump first captured the file
    -- Best-effort parse from the listing-row label:
    doc_type                 TEXT,                         -- 'Account Statement', 'Tax Report', ...
    doc_date                 INTEGER,                      -- Unix seconds UTC
    account_external_id      TEXT,                         -- when discoverable
    portfolio_external_id    TEXT,                         -- when discoverable
    label                    TEXT    NOT NULL              -- raw label text
);

CREATE INDEX ix_documents_doc_date ON documents(doc_date);
CREATE INDEX ix_documents_doc_type ON documents(doc_type);
CREATE INDEX ix_documents_account  ON documents(account_external_id);


-- ============================================================
-- Migration-complete marker. Must be the last statement in the
-- file; the loader uses it to record the version.
-- ============================================================
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));
