-- ============================================================
-- fidelity-web silver schema, migration 0001 — initial schema.
--
-- Built to mirror schwab-web and ubs-web's silver
-- conventions so the (planned) `wealthdb` Fidelity adapter can
-- compose with the sibling silvers using the same shapes.
--
-- Migration discipline: every change to the silver schema lands
-- as a new numbered file in this directory. The loader checks
-- the maximum applied silver_schema_version in schema_meta and
-- applies any newer migrations in order. Silver databases must
-- always conform to the latest schema; no backward-compatible
-- drift.
--
-- Storage conventions:
--   * Unix-seconds-UTC integers for all timestamps.
--   * Stable filter columns promoted; rest in `payload` TEXT JSON.
--   * Snapshot tables monotemporal on snapshot_at (PK starts with
--     it, so the implicit B-tree serves as the as-of index).
--   * Event tables keyed by a stable external id; idempotent
--     INSERT OR REPLACE so re-loading a window converges.
--   * Content-dedup on documents via PRIMARY KEY content_sha256.
--
-- ------------------------------------------------------------
-- Identifier conventions
-- ------------------------------------------------------------
--
--   account_external_id
--     Fidelity's 9-digit account number, no separator. The DAF's
--     7-digit id is the auto-exclusion criterion at bronze; it
--     never enters silver. Promoted into every snapshot + event
--     table as the canonical account join key.
--
--   portfolio_external_id
--     The label Fidelity renders above each account group in the
--     account selector ('Education' for the 529 sleeves,
--     'Authorized' for trust accounts, etc.). Captured at bronze
--     time as `account_dimensions[*].portfolio`. The gold layer
--     can normalize these to its own taxonomy; silver preserves
--     Fidelity's labels verbatim. Portfolios are wealth-management
--     groupings — they never hold cash or securities directly.
--
--   instrument_key
--     Equity / ETF / mutual-fund TICKER as Fidelity surfaces it in
--     the Symbol column. Plan-internal codes (e.g. XXX###### for
--     state 529-plan target-date sleeves) and money-market core
--     codes (CORE_X, CORE_Y, …) flow through unchanged — silver
--     stores what Fidelity emits and a future `instrument_kind`
--     column or gold-side lookup discriminates. NULL only for
--     genuinely-instrumentless rows (pure cash transfers, fees,
--     deposits, interest accruals).
--
--   activity_id (transactions)
--     Fidelity activity CSVs do NOT carry a stable per-row
--     identifier. Synthesised as the hex SHA-256 prefix of
--     "<account_external_id>|<run_date>|<amount>|<description>|
--      <symbol>|<source_sha256>|<row_index_within_csv>".
--     Stable across re-loads of the same source CSV; not portable
--     across re-downloads of overlapping windows that produce
--     different row ordering (Fidelity's export is deterministic
--     in our experience, but the `row_index_within_csv` anchor
--     limits the blast radius if it ever isn't).
--
--   doc_sha256 (documents)
--     Content hash of the PDF on disk. Same row regardless of how
--     many dumps surfaced the same file; snapshot_at records the
--     first dump that observed it.
--
-- ------------------------------------------------------------
-- Gold merge strategy (informational; implemented in wealthdb)
-- ------------------------------------------------------------
--
-- The fidelity-web silver is the only Fidelity-side silver today
-- (no Akoya / API equivalent exists for retail clients — see
-- DESIGN.md §1.1). If a future institutional feed from a third-party investment manager materialises (see DESIGN.md §1.3) it would live in a separate
-- repo + silver and the gold layer would splice on value-date
-- per the schwab-api/web pattern.
--
-- Per-account ownership routing is driven by `portfolios.kind`
-- (see below). The gold layer carries an `owner` dimension
-- (`self` for personal accounts, `trust` for accounts under a
-- trust agreement); the loader pre-classifies via portfolios.kind
-- so gold can join without re-deriving.
-- ============================================================

PRAGMA foreign_keys = ON;

-- One row per applied migration. Loader writes a row at the end of
-- each migration file; current schema version = MAX(silver_schema_version).
CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                  -- Unix seconds UTC
);

-- One row per bronze dump ingested. snapshot_at is parsed from
-- the bronze dir name (YYYYMMDDTHHMMSSZ → Unix seconds UTC).
-- A given dump can carry zero or more of the per-phase artefacts
-- depending on the trigger config (--mode all vs a single phase);
-- the *_present columns let queries distinguish "phase ran but
-- produced no rows" from "phase wasn't in this dump's mode".
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL,                 -- absolute path on disk at load time
    mode                  TEXT,                              -- trigger config 'mode' (all|positions|...)
    activity_since        INTEGER,                           -- Unix seconds; trigger config 'since'
    activity_until        INTEGER,                           -- Unix seconds; trigger config 'until'
    positions_present     INTEGER NOT NULL DEFAULT 0,
    activity_present      INTEGER NOT NULL DEFAULT 0,
    documents_present     INTEGER NOT NULL DEFAULT 0,
    balances_present      INTEGER NOT NULL DEFAULT 0,
    performance_present   INTEGER NOT NULL DEFAULT 0
);


-- ============================================================
-- SNAPSHOT TABLES — slow-changing master data
-- ============================================================

-- One row per (snapshot, portfolio). `portfolio_external_id` is
-- the literal Fidelity account-selector group label captured at
-- bronze time (e.g. 'Education', 'Authorized').
--
-- `kind` is the loader's interpretation of that label:
--   '529'              Fidelity 529 College Investing Plan sleeves
--                      (group label 'Education')
--   'trust_managed'    Trust accounts under a third-party investment manager (group label 'Authorized'; see
--                      DESIGN.md §1.3)
--   'other'            Anything else surfaced under a different
--                      group label (e.g. a retail or DAF-adjacent
--                      group; the DAF itself is auto-excluded
--                      before silver). Lets gold route unknown
--                      labels without a schema migration.
CREATE TABLE portfolios (
    snapshot_at           INTEGER NOT NULL,
    portfolio_external_id TEXT    NOT NULL,                 -- e.g. 'Education', 'Authorized'
    kind                  TEXT    NOT NULL
        CHECK (kind IN ('529', 'trust_managed', 'other')),
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, portfolio_external_id)
);

-- One row per (snapshot, account). `nickname` is the user-set
-- display name (e.g. an account nickname); the literal
-- 'Trust: Under Agreement' prefix is what Fidelity emits for trust
-- sleeves (the disambiguating agreement number lives only in
-- statement/tax-form PDFs, not in any selector text we can read).
CREATE TABLE accounts (
    snapshot_at           INTEGER NOT NULL,
    account_external_id   TEXT    NOT NULL,                 -- 9-digit Fidelity account number
    portfolio_external_id TEXT,                              -- joins portfolios.portfolio_external_id
    nickname              TEXT,                              -- e.g. '<Nickname>'
    payload               TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);
CREATE INDEX ix_accounts_portfolio
    ON accounts(snapshot_at, portfolio_external_id);


-- ============================================================
-- SNAPSHOT TABLES — daily state
-- ============================================================

-- One row per (snapshot, account, instrument) from positions
-- exports. Two views fan into the same table:
--
--   'summary'  — the Overview preset CSV. Carries the primary
--                quantity / market-value / cost-basis snapshot.
--   'dividend' — the DividendView preset CSV. Carries ex-date,
--                pay-date, distribution-yield columns.
--
-- The loader collapses them on (account, instrument_key): the
-- summary row's columns land first, the dividend row's columns
-- merge in. Both views always agree on quantity / current-value
-- so the merge is non-destructive.
--
-- `instrument_key` is non-null in practice for every position
-- Fidelity surfaces; the loader's validation step asserts this.
-- The single exception family is core cash positions (FCASH**
-- footnoted), which DO carry a Symbol but it's a money-market
-- code, not a true ticker — they're still stored here.
CREATE TABLE positions (
    snapshot_at           INTEGER NOT NULL,
    account_external_id   TEXT    NOT NULL,
    instrument_key        TEXT    NOT NULL,                 -- ticker / plan-code / money-market code
    description           TEXT,                              -- e.g. 'ACME CORP'
    quantity              REAL,
    last_price            REAL,                              -- USD
    current_value         REAL,                              -- USD
    cost_basis_total      REAL,                              -- USD; NULL if Fidelity didn't compute
    average_cost_basis    REAL,                              -- USD; NULL ditto
    type                  TEXT,                              -- 'Cash' / 'Margin' / ...
    ex_date               INTEGER,                           -- Unix seconds; NULL if no dividend
    amount_per_share      REAL,                              -- USD; NULL if no dividend
    pay_date              INTEGER,                           -- Unix seconds; NULL if no dividend
    distribution_yield    REAL,                              -- fraction (0.05 = 5%); NULL if N/A
    sec_yield             REAL,                              -- fraction; NULL if N/A
    est_annual_income     REAL,                              -- USD; NULL if N/A
    payload               TEXT    NOT NULL,                  -- {summary: {...}, dividend: {...}}
    PRIMARY KEY (snapshot_at, account_external_id, instrument_key)
);
CREATE INDEX ix_positions_instrument
    ON positions(instrument_key);


-- ============================================================
-- EVENT TABLE — transactions
-- ============================================================

-- One row per Fidelity activity-CSV row. `activity_id` is the
-- synthetic stable id described in the header. Re-loading the
-- same source CSV is idempotent (PK-keyed INSERT OR REPLACE).
--
-- `kind` is the Action column's first word for fast filtering
-- (PURCHASE, SALE, DIVIDEND, INTEREST, JOURNAL, ...). The full
-- Action text and the Description column live in payload.
--
-- `instrument_key` is NULL for genuine-no-instrument rows: pure
-- cash deposits / withdrawals / journal entries, fees that aren't
-- tied to a security, etc. The loader's validation step asserts
-- that no non-cash activity carries a NULL instrument_key (every
-- DIVIDEND, PURCHASE, SALE, REINVESTMENT row must have one).
CREATE TABLE transactions (
    activity_id           TEXT    NOT NULL PRIMARY KEY,
    timestamp             INTEGER NOT NULL,                  -- Unix seconds at midnight, Run Date
    account_external_id   TEXT    NOT NULL,
    kind                  TEXT    NOT NULL,                  -- Action's first word
    instrument_key        TEXT,                              -- ticker; NULL only for pure cash
    quantity              REAL,
    price                 REAL,                              -- USD
    amount                REAL,                              -- USD signed
    settlement_date       INTEGER,                           -- Unix seconds at midnight
    source_sha256         TEXT    NOT NULL,                  -- sha256 of the source activity CSV
    payload               TEXT    NOT NULL                   -- full row dict
);
CREATE INDEX ix_transactions_account_timestamp
    ON transactions(account_external_id, timestamp);
CREATE INDEX ix_transactions_instrument
    ON transactions(instrument_key);
CREATE INDEX ix_transactions_source_sha256
    ON transactions(source_sha256);


-- ============================================================
-- DOCUMENTS CATALOG
--
-- Binary stays on disk under <bronze-root>/<dump-ts>/documents/
-- (statements + tax forms) and <dump-ts>/balances|performance/
-- (HTML snapshots of pages with no structured export). This
-- table is the index.
-- ============================================================

-- One row per unique-content artefact. Deduped on content_sha256:
-- the same PDF / HTML observed in multiple dumps collapses to one
-- row whose snapshot_at is the FIRST run that captured it.
--
-- doc_kind values:
--   'statement'        Fidelity quarterly / annual statements
--                      (accounts whose group exposes statements in
--                      the web document center; see DESIGN.md §1.2)
--   'tax_form'         Consolidated 1099 / 1099-Q PDFs, per
--                      tax-year, per account group
--   'balances_html'    balances/balances.html snapshot
--   'performance_html' performance/performance.html snapshot
--
-- file_format: 'pdf' | 'csv' | 'html'
CREATE TABLE documents (
    content_sha256        TEXT    NOT NULL PRIMARY KEY,
    snapshot_at           INTEGER NOT NULL,                  -- first dump that observed this file
    file_path             TEXT    NOT NULL,                  -- absolute path on disk at load time
    file_name             TEXT    NOT NULL,                  -- Fidelity-supplied (PDFs) or fixed (.html)
    size_bytes            INTEGER NOT NULL,
    doc_kind              TEXT    NOT NULL,
    file_format           TEXT    NOT NULL,
    tax_year              INTEGER,                           -- for tax_form; NULL otherwise
    account_external_id   TEXT,                              -- when discoverable from filename
    payload               TEXT    NOT NULL                   -- {row_label, year, ...}
);
CREATE INDEX ix_documents_kind          ON documents(doc_kind);
CREATE INDEX ix_documents_snapshot      ON documents(snapshot_at);
CREATE INDEX ix_documents_tax_year      ON documents(tax_year);
CREATE INDEX ix_documents_account       ON documents(account_external_id);


-- ============================================================
-- Migration-complete marker. Must be the last statement in the
-- file; the loader uses it to record the version.
-- ============================================================
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));
