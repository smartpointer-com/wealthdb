-- ============================================================
-- viac silver schema, migration 0001 — initial schema.
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
--     VIAC's dotted portfolio number — `<product>.<customer-id>.
--     <portfolio-index>`. Examples (placeholder shape):
--       Pillar-3a:        3.NNN.NNN.NNN.NN
--       Pillar-2 (PVB):   2.NNN.NNN.NNN.O  (mandatory)
--                         2.NNN.NNN.NNN.U  (extra-mandatory)
--       Investment (INV): 1.NNN.NNN.NNN.NN  (not observed; product 1
--                                            is plausible)
--     The first dotted segment encodes the product line; we promote
--     it to `product_code`. Stable across sessions.
--
--   instrument_external_id
--     ISIN. VIAC's assetsOverview always carries the ISIN on each
--     position (none observed with NULL); we use it directly as
--     the per-instrument key. Mirrors wealthdb gold's
--     instruments.isin so cross-bank joins are free.
--
--   transaction_external_id
--     Synthetic SHA-256 hex prefix over
--     `(account|type|value_date|amount_chf|document_number)`. VIAC
--     doesn't surface a stable per-event id on the wire — the
--     `documentNumber` is shared across some legs of corporate-
--     action pairs, so we hash a tuple instead. Deterministic →
--     re-loading converges.
--
--   content_sha256 (documents)
--     SHA-256 hex of the PDF on disk. The same logical document
--     gets one row regardless of which dump first fetched it;
--     last_seen_at advances on subsequent dumps.
--
-- ------------------------------------------------------------
-- Gold merge strategy (informational; implemented in wealthdb)
-- ------------------------------------------------------------
--
-- The viac silver is currently the only source for VIAC accounts.
-- A future wealthdb `viac` adapter projects:
--
--   accounts.account_external_id  →  gold.accounts.account_external_id
--   accounts.product_code='3'     →  gold.accounts.tax_wrapper='pillar_3a'
--   accounts.product_code='2'     →  gold.accounts.tax_wrapper='vested_benefits'
--   accounts.product_code='1'     →  gold.accounts.tax_wrapper='taxable_personal'
--                                    (INV is VIAC's non-retirement product
--                                    line; not yet observed in the source
--                                    data, mapping speculative)
--   ALL VIAC accounts             →  gold.accounts.management_style='automated'
--                                    VIAC is robo-advisor-shaped — the holder
--                                    picks a strategy from a menu (or builds
--                                    one within VIAC's concentration limits),
--                                    after which rebalancing runs by rules
--                                    with no human in the loop. Not pure
--                                    self_directed like an IRA brokerage; the
--                                    holder cannot trade outside VIAC's
--                                    listed fund universe. Same management
--                                    style as Relevate's FZ products.
--   account_kind                  →  'brokerage' for ACTIVE p3a; 'cash' for
--                                    PASSIVE pvb until we know more
--   instruments.isin              →  gold.instruments.isin (indexed)
--   positions                     →  gold.positions (ACTUAL holdings;
--                                    assetsOverview carries amount, ratio,
--                                    acquisitionPrice, assetPrice per fund)
--   transactions.kind             →  gold.transactions.kind (mapped from
--                                    VIAC's type by load.py — see §kind
--                                    mapping below)
--   wealth_history                →  not in gold today; customer-level
--                                    daily NAV that the adapter can project
--                                    into per-account allocations if needed
--   documents                     →  gold.documents (PDFs stay on disk;
--                                    this is the index)
-- ============================================================

PRAGMA foreign_keys = ON;

-- All-or-nothing migration: BEGIN/COMMIT wrap the whole file so a
-- mid-script failure rolls everything back. Python's sqlite3
-- executescript() does an implicit COMMIT before running, so
-- transaction control must live inside the migration itself.
BEGIN;

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                     -- Unix seconds UTC
);

-- One row per bronze dump run that has been ingested. snapshot_at
-- is parsed from the bronze dir name (YYYYMMDDTHHMMSSZ → Unix
-- seconds UTC). dump_runs is the idempotency anchor: the loader
-- skips dumps whose snapshot_at already exists here.
--
-- Manifest counters are promoted for quick `wealthdb status`-style
-- queries; the full run.json lives in payload for forensics.
CREATE TABLE dump_runs (
    snapshot_at                  INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version        INTEGER NOT NULL,
    run_dir                      TEXT    NOT NULL,
    dry_run                      INTEGER NOT NULL DEFAULT 0,   -- 0/1
    with_transaction_documents   INTEGER NOT NULL DEFAULT 0,   -- 0/1; VIAC-specific gate
    bronze_docs_total            INTEGER NOT NULL DEFAULT 0,
    bronze_docs_fetched          INTEGER NOT NULL DEFAULT 0,
    bronze_docs_linked           INTEGER NOT NULL DEFAULT 0,
    bronze_docs_skipped          INTEGER NOT NULL DEFAULT 0,
    bronze_docs_errors           INTEGER NOT NULL DEFAULT 0,
    payload                      TEXT    NOT NULL              -- the full bronze run.json verbatim
);


-- ============================================================
-- SNAPSHOT TABLES — slow-changing master data
-- ============================================================

-- One row per (snapshot, account). VIAC calls these "portfolios"
-- on the wire and the UI; each one is what the holder perceives
-- as a single account / depot — wealthdb gold's `accounts` table
-- is the right target.
--
-- Both Pillar-3a (`p3a`) and Pillar-2 vested-benefits (`pvb`)
-- portfolios from the inventory land here, distinguished by
-- product_code (the first dotted segment of the number).
-- download.py currently only fetches strategy/assets/fees for
-- p3a; pvb portfolios appear with NULL strategy_* fields until
-- the pvb endpoint surface is mapped.
--
-- Promoted columns: enough for filter / join / wealthdb-gold
-- projection without parsing JSON. Anything portfolio-strategy-
-- shaped that the SPA might add later goes in payload.
CREATE TABLE accounts (
    snapshot_at            INTEGER NOT NULL,
    account_external_id    TEXT    NOT NULL,                   -- e.g. 'X.NNN.NNN.NNN.NN'
    product_code           TEXT    NOT NULL,                   -- '3' p3a, '2' pvb, '1' inv
    portfolio_index        TEXT    NOT NULL,                   -- last segment, e.g. '01', 'O', 'U'
    name                   TEXT,                                -- e.g. 'Portfolio 2021'
    state                  TEXT,                                -- 'ACTIVE' | 'PASSIVE'
    inventory_index        INTEGER,                             -- inventory `index` field
    inventory_sort_index   INTEGER,                             -- inventory `sortIndex` field
    -- p3a-specific fields from the inventory
    investment_focus       TEXT,                                -- e.g. 'GLOBAL'
    risk_level             INTEGER,                             -- 0..100
    -- p3a-specific fields from strategy.json (currentStrategy)
    strategy_id            INTEGER,
    strategy_is_custom     INTEGER,                             -- 0/1
    custody_bank           TEXT,                                -- 'UBS' for observed p3a
    remainder_allocation   TEXT,                                -- 'CASH' for observed p3a
    investment_type        TEXT,                                -- e.g. 'DELTA_GLIDER'
    interest_rate          REAL,                                -- portfolio-level p3a interest rate
    -- pvb-specific fields from the inventory
    foundation             TEXT,                                -- e.g. 'WIR' for observed pvb
    portfolio_type         TEXT,                                -- e.g. 'MANDATORY_INSURANCE'
    -- currency is uniform 'CHF' across observed VIAC accounts but
    -- promoted so consumers don't have to assume.
    currency_code          TEXT    NOT NULL DEFAULT 'CHF',
    payload                TEXT    NOT NULL,                    -- the full portfolios[i] + strategy union
    PRIMARY KEY (snapshot_at, account_external_id)
);
CREATE INDEX ix_accounts_external_id
    ON accounts(account_external_id);
CREATE INDEX ix_accounts_product
    ON accounts(product_code);


-- ============================================================
-- SNAPSHOT TABLES — per-snapshot state
-- ============================================================

-- Per-portfolio per-currency cash balances. Currently the only
-- balance_kind we surface is 'cash' (assetsOverview.cashAmount —
-- the free liquidity inside the portfolio). Schema is keyed
-- compositely so future kinds (e.g. 'available', 'pending')
-- don't need a migration.
CREATE TABLE cash_balances (
    snapshot_at            INTEGER NOT NULL,
    account_external_id    TEXT    NOT NULL,
    currency               TEXT    NOT NULL,                    -- ISO 4217
    balance_kind           TEXT    NOT NULL,                    -- 'cash' today; extensible
    amount                 REAL    NOT NULL,
    payload                TEXT,                                 -- source field name + raw extras
    PRIMARY KEY (snapshot_at, account_external_id, currency, balance_kind)
);

-- One row per (snapshot, account, instrument) from
-- assetsOverview.assetsByClasses.*. These are ACTUAL holdings, not
-- target allocation — VIAC carries unit-value (`amount` in CHF) and
-- per-fund `ratio`. The native fund currency is on `currency_code`;
-- the CHF value is on `amount` (assetPrice × units, FX-converted).
--
-- asset_class is the loader's normalisation to wealthdb's canonical
-- taxonomy (equity / bond / fund / money_market / metal / other);
-- viac_asset_class + sub_asset_class preserve VIAC's own labels for
-- forensics and for tighter wealthdb-side categorisation later.
CREATE TABLE positions (
    snapshot_at            INTEGER NOT NULL,
    account_external_id    TEXT    NOT NULL,
    instrument_external_id TEXT    NOT NULL,                    -- ISIN
    asset_class            TEXT    NOT NULL,                    -- canonical (equity/bond/fund/...)
    viac_asset_class       TEXT,                                 -- raw (EQUITIES/BONDS/REAL_ESTATE/...)
    sub_asset_class        TEXT,                                 -- raw (SHARES_SWITZERLAND/...)
    currency_code          TEXT,                                 -- native fund currency
    name                   TEXT,
    amount                 REAL,                                 -- fund UNITS held (VIAC's JSON key, despite the name; renamed to `quantity` in 0002)
    ratio                  REAL,                                 -- fraction of portfolio (0..1)
    ratio_chf              REAL,                                 -- CHF market value (= amount * asset_price; renamed to `market_value_chf` in 0002)
    acquisition_price      REAL,                                 -- cost basis per unit (CHF)
    asset_price            REAL,                                 -- current price per unit (CHF)
    rate_of_return         REAL,                                 -- VIAC's per-position return %
    payload                TEXT    NOT NULL,                    -- the full position object
    PRIMARY KEY (snapshot_at, account_external_id, instrument_external_id)
);
CREATE INDEX ix_positions_isin
    ON positions(instrument_external_id);
CREATE INDEX ix_positions_account
    ON positions(account_external_id);

-- Catalog of instruments observed across all positions. Slow-
-- changing master data — same ISIN appearing in multiple snapshots
-- collapses to one row whose first_seen_at is the earliest
-- snapshot_at and whose last_seen_at advances on each load.
CREATE TABLE instruments (
    instrument_external_id TEXT    NOT NULL PRIMARY KEY,        -- ISIN
    isin                   TEXT    NOT NULL,                    -- same value, kept named for clarity
    name                   TEXT,
    currency_code          TEXT,
    asset_class            TEXT,                                 -- canonical
    viac_asset_class       TEXT,                                 -- raw
    sub_asset_class        TEXT,                                 -- raw
    etf_link_en            TEXT,                                 -- factsheet URL if VIAC supplies one
    first_seen_at          INTEGER NOT NULL,
    last_seen_at           INTEGER NOT NULL,
    payload                TEXT    NOT NULL                     -- most-recent position object verbatim
);


-- ============================================================
-- SNAPSHOT TABLE — customer-level wealth time series
-- ============================================================

-- VIAC's `/wealth/summary` returns three aligned daily time series
-- across the customer's WHOLE portfolio set (not per-portfolio):
-- daily wealth value, daily performance %, daily invested amount.
-- We zip them by date into one row per (snapshot, value_date).
-- The series can extend back to the customer's first investment
-- (~1600 rows for the observed user) and grows by one day per
-- working day. Each snapshot persists the full series so the gold
-- layer can pick the latest snapshot per value_date — same
-- pattern as relevate's `performance_points` table.
CREATE TABLE wealth_history (
    snapshot_at            INTEGER NOT NULL,
    value_date             INTEGER NOT NULL,                    -- Unix s at midnight UTC
    wealth_value           REAL,                                 -- dailyWealth[i].value
    performance_value      REAL,                                 -- dailyPerformance[i].value (return %)
    invested_amount        REAL,                                 -- dailyInvestedAmounts[i].value
    payload                TEXT,                                 -- the three source rows merged
    PRIMARY KEY (snapshot_at, value_date)
);
CREATE INDEX ix_wealth_history_date
    ON wealth_history(value_date);


-- ============================================================
-- EVENT TABLE — transactions
--
-- Every transaction VIAC's /p3a/portfolio/transactions surfaces.
-- The JSON record is tiny (type / amountInChf / valueDate /
-- balanceAfterBooking / documentNumber); the rich detail (ISIN,
-- units, native price, FX rate for trades; old→new ISIN map for
-- fusions) lives in the PDF behind documentNumber.
--
-- kind mapping (set by load.py from VIAC's `type`):
--   INTEREST              → 'interest'
--   FEE_CHARGE            → 'fee'
--   CONTRIBUTION          → 'deposit'
--   TRADE_BUY             → 'buy'
--   TRADE_SELL            → 'sell'
--   DIVIDEND              → 'dividend'
--   DIVIDEND_CANCELLATION → 'corporate_action'  (reversal of an earlier dividend)
--   FUSION_NEW_FONDS      → 'corporate_action'  (fund-merger ADD leg)
--   FUSION_FONDS_RESET    → 'corporate_action'  (fund-merger REMOVE leg)
--   (anything else)       → 'other'             (raw VIAC type preserved in payload)
-- ============================================================

CREATE TABLE transactions (
    transaction_external_id TEXT    NOT NULL PRIMARY KEY,       -- sha256 prefix over (acct|type|date|amt|doc)
    snapshot_at             INTEGER NOT NULL,                   -- dump that first observed this event
    occurred_at             INTEGER NOT NULL,                   -- Unix s at midnight UTC of valueDate
    account_external_id     TEXT    NOT NULL,
    type                    TEXT    NOT NULL,                   -- VIAC's raw type
    kind                    TEXT    NOT NULL,                   -- canonical (see comment above)
    amount_chf              REAL,                                -- signed; debits negative
    balance_after_chf       REAL,                                -- portfolio cash balance after booking
    document_number         TEXT,                                -- VIAC doc id (21V-XXX-XXX) when present
    currency                TEXT    NOT NULL DEFAULT 'CHF',
    payload                 TEXT    NOT NULL
);
CREATE INDEX ix_transactions_account_occurred
    ON transactions(account_external_id, occurred_at);
CREATE INDEX ix_transactions_document_number
    ON transactions(document_number);
CREATE INDEX ix_transactions_kind
    ON transactions(kind);


-- ============================================================
-- DOCUMENTS CATALOG
--
-- PDF binaries stay under <bronze-root>/<dump-ts>/documents/ —
-- this table indexes them. Content-deduped on content_sha256:
-- the same PDF fetched in multiple dumps collapses to one row
-- whose first_seen_at is the earliest dump that observed it and
-- whose last_seen_at advances on each subsequent dump.
--
-- VIAC's document index is rich and well-structured: (type,
-- subType) is the canonical taxonomy. doc_type maps to:
--   TRANSACTION       per-event PDFs (TRADE_REPORT, FEE_CHARGE,
--                     INTEREST, DIVIDEND, SECURITY_FUSION,
--                     DIVIDEND_CANCELLATION)
--   CONTRACT          PROVISION_CONTRACT, INVESTMENT_PROFILE
--   REPORT            INVESTMENT_REPORTING (annual statements),
--                     MANUAL_INVESTMENT_REPORTING
--   TAX               TAX_REPORT (Pillar-3a Bescheinigungen)
--   ACCOUNT_MOVEMENT  CONTRIBUTION_CREDIT_NOTE
--   COMMUNICATION     GENERIC_COMMUNICATION
-- ============================================================

-- bronze_path is RELATIVE TO THE BRONZE ROOT (i.e. starts with
-- the run-ts dirname: '<YYYYMMDDTHHMMSSZ>/documents/<docid>.pdf').
-- Consumers join with their own bronze root — same row resolves
-- correctly inside the container (root = /data) and on the host
-- (root = $XDG_DATA_HOME/wealthdb/viac). No absolute path stored, no
-- container-vs-host translation needed.
CREATE TABLE documents (
    content_sha256          TEXT    NOT NULL PRIMARY KEY,
    viac_doc_id             TEXT    NOT NULL,                   -- documentNumber, e.g. '21V-XXX-XXX'
    doc_type                TEXT    NOT NULL,                   -- TRANSACTION/CONTRACT/REPORT/TAX/...
    doc_subtype             TEXT,                                -- TRADE_REPORT/INTEREST/...
    mime_type               TEXT,                                -- application/pdf observed
    language                TEXT,                                -- 'en' / 'de' / 'fr' / 'it'
    product                 TEXT,                                -- 'P3A' for observed; presumably 'PVB' for vested benefits
    timestamp               INTEGER,                             -- Unix s from documents[].timestamp
    file_size               INTEGER NOT NULL,
    bronze_path             TEXT    NOT NULL,                   -- '<run-ts>/documents/<docid>.pdf' relative
    first_seen_at           INTEGER NOT NULL,
    last_seen_at            INTEGER NOT NULL,
    payload                 TEXT    NOT NULL                    -- the documents[i] index entry
);
CREATE INDEX ix_documents_viac_doc_id      ON documents(viac_doc_id);
CREATE INDEX ix_documents_type             ON documents(doc_type, doc_subtype);
CREATE INDEX ix_documents_timestamp        ON documents(timestamp);
CREATE INDEX ix_documents_first_seen       ON documents(first_seen_at);


-- ============================================================
-- Migration-complete marker. Must be the second-to-last statement
-- (COMMIT closes the wrapping transaction); the loader uses it
-- to record the version.
-- ============================================================
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
