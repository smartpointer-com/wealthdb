-- ============================================================
-- equityzen silver schema, migration 0001 — initial schema.
--
-- Migration discipline: every change lands as a new numbered file here.
-- collectorkit.silver applies any migration whose number exceeds the max
-- silver_schema_version in schema_meta, in order; each ends by inserting
-- its own version row.
--
-- Storage conventions: snapshot_at / *_at are INTEGER Unix seconds (UTC).
-- Source *dates* (purchase / tender / deal start) are kept as their source
-- ISO TEXT (calendar dates, parsed by the gold adapter). Stable filter
-- columns are promoted; the full source node rides in `payload`.
--
-- Source: EquityZen's investor GraphQL. The source is the buyer side, holding
-- membership interests in SPV / multi-company-fund LLCs (partnerships).
-- Identity keys are Relay global ids: deal_external_id = base64("DealNode:N").
-- See collectors/equityzen/DESIGN.md.
--
-- Valuation principle: only CLOSED-deal prices are reliable —
-- the buyer's purchase (entry) and tenders (closed secondary sales). Order-
-- book asks (live listings, open invOpps) and deal-era implied valuations
-- are NOT used (see DESIGN.md §4 "Why no /equity/ capture", §6).
-- ============================================================

PRAGMA foreign_keys = ON;

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                -- Unix seconds UTC
);

-- One row per ingested bronze run, written last so a mid-load failure
-- leaves no trace.
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,   -- Unix seconds UTC
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL,               -- absolute bronze run path
    payload               TEXT                            -- run.json manifest
);

-- ============================================================
-- offerings — IMMUTABLE per investment (keyed by deal_external_id, NOT
-- snapshot). Identity + entry terms are fixed at purchase, so they live
-- here once instead of repeating on every snapshot. Upserted each load
-- (latest values win; last_seen_at records the most recent run that saw
-- it). A stock split is the only event that can move purchase_price /
-- shares_original — the upsert absorbs it.
-- ============================================================
CREATE TABLE offerings (
    deal_external_id    TEXT    NOT NULL PRIMARY KEY,  -- deal.id, base64("DealNode:N")
    kind                TEXT,               -- 'spv' | 'private_fund' (from asset_class)
    asset_class         TEXT,               -- ASSET_COMPANY | ASSET_MULTI_COMPANY_FUND
    company_external_id TEXT,               -- deal.company.id
    company_name        TEXT,               -- spv: the company; private_fund: fund label
    fund_external_id    TEXT,               -- deal.fund.id (the LLC)
    fund_name           TEXT,               -- deal.fund.name
    parent_deal_name    TEXT,               -- deal.parentDeal.name (series parent), if any
    ticker_symbol       TEXT,               -- deal.company.tickerSymbol (usually null, pre-IPO)
    flavor              TEXT,               -- deal.flavor (e.g. REGULAR)
    date_start          TEXT,               -- deal.dateStart (source ISO)
    deal_share_price    REAL,               -- deal.sharePrice (the deal's offering price)
    basis               REAL,               -- investmentSize (what the buyer paid)
    purchase_price      REAL,               -- pricePostSplit (entry price/share, a closed deal)
    shares_original     REAL,               -- sharesPostSplit (shares originally bought)
    currency            TEXT    NOT NULL DEFAULT 'USD',
    last_seen_at        INTEGER,            -- snapshot_at of the most recent run that observed it
    payload             TEXT    NOT NULL    -- the deal node
);

-- ============================================================
-- positions — EVENT-SOURCED valuation history. One row per CAPITAL EVENT of
-- a position (a *change*), NOT a full-portfolio snapshot per date. Replayed
-- by load.py from the source ledger from the original investment date
-- forward: event_seq 0 = the original investment, then each disposition
-- (tender / distributed transaction) in date order, then a terminal `exit`
-- event when the deal is EXITED. Each row is the position state effective
-- from `as_of_date`.
--
-- Reconstruct the holdings as of any date D:
--     WITH latest AS (
--       SELECT deal_external_id, MAX(event_seq) AS seq
--       FROM positions WHERE as_of_date <= :D
--       GROUP BY deal_external_id)
--     SELECT p.* FROM positions p JOIN latest l
--       ON p.deal_external_id = l.deal_external_id AND p.event_seq = l.seq
--     WHERE p.is_open = 1;        -- drop positions whose latest event is an exit
-- (event_seq increases with as_of_date, so MAX(event_seq) ≤ D is the latest
-- event on/before D.) Omit the date filter for the current holdings.
--
-- Valuation uses CLOSED-deal prices: `price_per_share` is the entry price at
-- the investment event and the tender price at each disposition;
-- `market_value = shares_held * price_per_share`. So an SPV's mark steps only
-- on a real closed transaction — a name that has never tendered carries cost
-- until one. Multi-company funds, which have no per-share tender price,
-- instead revalue to the parsed capital-account-statement NAV via injected
-- `statement` events (see capital_account_statements below). No capital
-- calls: EquityZen vehicles are funded upfront.
-- ============================================================
CREATE TABLE positions (
    deal_external_id         TEXT    NOT NULL,   -- → offerings.deal_external_id
    event_seq                INTEGER NOT NULL,   -- 0 = original investment; 1..n later events, in date order
    as_of_date               TEXT,               -- event date (source ISO); state effective from here
    event_type               TEXT    NOT NULL,   -- 'investment' | 'disposition' | 'exit'
    status                   TEXT,               -- 'CLOSED' (held) | 'EXITED' (closed out at this event)
    is_open                  INTEGER,            -- 1 = still held after this event; 0 = closed/exited (holdings filter)
    event_external_id        TEXT,               -- source transaction id that drove the event (provenance)
    shares_held              REAL,               -- shares still held AFTER this event
    cost_basis_remaining     REAL,               -- shares_held * purchase_price (book value of remaining stake)
    price_per_share          REAL,               -- CLOSED price effective at this event (entry / tender / exit); NULL if none
    market_value             REAL,               -- shares_held * price_per_share (mark of the remaining stake)
    distributions_cumulative REAL,               -- realized distributions to date
    total_value              REAL,               -- market_value + distributions_cumulative (value created to date)
    snapshot_at              INTEGER NOT NULL,   -- download run that computed this replay (provenance)
    payload                  TEXT    NOT NULL,   -- the driving transaction node
    PRIMARY KEY (deal_external_id, event_seq)
);
CREATE INDEX idx_positions_asof ON positions(as_of_date);

-- ============================================================
-- LEDGER TABLES — keyed by a stable source id, so they accumulate across
-- snapshots (INSERT OR REPLACE upserts). snapshot_at records the run that
-- last observed the row.
-- ============================================================

-- cash_flows: the dated money ledger per offering. Amounts are positive
-- magnitudes; `kind` carries direction (purchase = outflow, distribution =
-- inflow) and the gold adapter applies the sign. execution_fee is
-- informative (not netted into amount). No capital-call rows.
CREATE TABLE cash_flows (
    cash_flow_external_id TEXT    NOT NULL PRIMARY KEY,  -- primaryTransaction.id | distribution.id
    deal_external_id      TEXT    NOT NULL,
    snapshot_at           INTEGER NOT NULL,              -- run that last saw it
    kind                  TEXT    NOT NULL,              -- 'purchase' | 'distribution'
    flow_date             TEXT,                          -- transactionDate (source ISO)
    amount                REAL,                          -- investmentSize | distribution.value
    execution_fee         REAL,                          -- informative; not netted into amount
    method                TEXT,                          -- transfer type, e.g. 'ACH'
    currency              TEXT    NOT NULL DEFAULT 'USD',
    description           TEXT,
    payload               TEXT    NOT NULL
);
CREATE INDEX idx_cash_flows_deal ON cash_flows(deal_external_id);

-- tax_documents: per-offering document metadata + archive bookkeeping.
-- `download --documents` fetches each blob (sets local_path / content_hash /
-- retrieved_at); load.py then parses capital-account statements and K-1s
-- into the two tables below. Rows without a fetched blob keep those NULL.
CREATE TABLE tax_documents (
    document_external_id  TEXT    NOT NULL PRIMARY KEY,  -- document.id
    deal_external_id      TEXT,
    snapshot_at           INTEGER NOT NULL,
    document_type         TEXT,                          -- K1 | CAPITAL_ACCOUNT_STATEMENT | QUARTERLY_REPORT | ...
    document_type_display TEXT,
    download_url          TEXT,
    local_path            TEXT,                          -- bronze blob path, once fetched (else NULL)
    content_hash          TEXT,                          -- sha256 of the blob (NULL until fetched)
    retrieved_at          INTEGER,                       -- run snapshot_at that fetched the blob (NULL until fetched)
    payload               TEXT    NOT NULL
);
CREATE INDEX idx_tax_documents_deal ON tax_documents(deal_external_id);

-- ============================================================
-- PARSED-STATEMENT TABLES — figures extracted from the document PDFs
-- (collectors/equityzen/statements.py, via pdftotext). Keyed by the source
-- document id. Populated by load.py when the blob is present + parseable.
-- ============================================================

-- capital_account_statements: the quarterly partner's Statement of Capital
-- Account. `ending_nav` (Net Ending Capital Account Balance) is the fund's
-- fair-value NAV for this partner at `period_end` — the valuation signal the
-- holdings API lacks for multi-company funds (it is injected into positions
-- as a 'statement' revaluation event for kind='private_fund'). SPV statements
-- are stored here too but SPV positions stay tender-driven.
CREATE TABLE capital_account_statements (
    document_external_id TEXT    NOT NULL PRIMARY KEY,
    deal_external_id     TEXT    NOT NULL,
    period_end           TEXT,                           -- statement period end (source ISO)
    beginning_balance    REAL,
    contributions        REAL,
    withdrawals          REAL,
    transfers            REAL,
    profit_loss          REAL,
    carried_interest     REAL,
    ending_nav           REAL,                           -- Net Ending Capital Account Balance (fair-value NAV)
    currency             TEXT    NOT NULL DEFAULT 'USD',
    content_hash         TEXT,
    snapshot_at          INTEGER NOT NULL,
    payload              TEXT    NOT NULL
);
CREATE INDEX idx_cap_acct_deal ON capital_account_statements(deal_external_id);

-- k1_documents: Schedule K-1 (Form 1065). Item L — the partner's capital-
-- account analysis — parses reliably; `ending_capital` is the tax-basis NAV.
-- Part III box amounts (income / gains / distributions) are NOT stored: they
-- are form-grid-positioned and a naive parse grabs box numbers, not values
-- (re-parse from the bronze PDF when a grid-aware extractor exists). The full
-- K-1 text is intentionally not retained (it carries SSN/EIN/address).
CREATE TABLE k1_documents (
    document_external_id      TEXT    NOT NULL PRIMARY KEY,
    deal_external_id          TEXT    NOT NULL,
    tax_year                  INTEGER,
    is_final                  INTEGER,
    beginning_capital         REAL,   -- Item L: beginning tax-basis capital
    current_year_income       REAL,   -- Item L: current-year net income (loss)
    withdrawals_distributions REAL,   -- Item L: withdrawals & distributions
    ending_capital            REAL,   -- Item L: ending tax-basis capital (tax-basis NAV)
    currency                  TEXT    NOT NULL DEFAULT 'USD',
    content_hash              TEXT,
    snapshot_at               INTEGER NOT NULL,
    payload                   TEXT    NOT NULL
);
CREATE INDEX idx_k1_deal ON k1_documents(deal_external_id);

-- Migration-complete marker — must be the LAST statement.
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));
