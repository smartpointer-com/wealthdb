-- angellist silver schema v1.
--
-- Source-shaped SQLite for the AngelList LP book, parsed from the venture
-- GraphQL captured by download.py. Money is stored exactly as AngelList
-- returns it: minor units (`*_minor`, the GraphQL `fractional`) + an ISO
-- `currency`. JSON `payload` columns carry the full source node so format
-- drift is absorbed here, not at gold. See DESIGN.md for the gold mapping.
--
-- NOTE on cash flows: the venture portal exposes per-position *cumulative*
-- contributed (capital called) and realized (distributions) amounts, plus
-- a portfolio-level value time series — NOT a dated per-event capital-call/
-- distribution ledger (the "activity" feed is unstructured VenturePosts).
-- So `positions` carries the cumulative call/distribution state; a dated
-- cash-flow ledger is not derivable from this surface.

CREATE TABLE IF NOT EXISTS schema_meta (
    silver_schema_version INTEGER PRIMARY KEY,
    applied_at            TEXT NOT NULL DEFAULT (datetime('now'))
);

-- One row per ingested bronze snapshot (skip-already-loaded bookkeeping).
CREATE TABLE IF NOT EXISTS dump_runs (
    snapshot_at         INTEGER PRIMARY KEY,   -- unix seconds (bronze run UTC ts)
    invest_account_slug TEXT,
    loaded_at           TEXT NOT NULL DEFAULT (datetime('now')),
    payload             TEXT                   -- run.json manifest
);

-- One row per held investable (SPV / fund deal). Identity is
-- AngelList's stable investableGuid.
CREATE TABLE IF NOT EXISTS vehicles (
    vehicle_external_id TEXT PRIMARY KEY,       -- investableGuid
    name                TEXT,                   -- investableName
    avatar_url          TEXT,
    first_seen_at       INTEGER NOT NULL,
    last_seen_at        INTEGER NOT NULL,
    payload             TEXT
);

-- Per-snapshot funded position in each vehicle.
CREATE TABLE IF NOT EXISTS positions (
    snapshot_at                INTEGER NOT NULL,
    position_external_id       TEXT    NOT NULL,   -- node id
    vehicle_external_id        TEXT    NOT NULL,   -- investableGuid
    status                     TEXT,
    status_label               TEXT,
    currency                   TEXT,
    commitment_minor           INTEGER,            -- commitmentAmount
    contributed_minor          INTEGER,            -- contributedAmount (capital called)
    investment_minor           INTEGER,            -- investmentAmount
    realized_minor             INTEGER,            -- realizedValue (cumulative distributions)
    recycled_minor             INTEGER,            -- recycledValue
    unrealized_minor           INTEGER,            -- unrealizedValue (nullable)
    total_value_minor          INTEGER,            -- totalValue (nullable)
    tvpi                       REAL,               -- nullable
    investment_date            INTEGER,            -- unix seconds
    fc_id                      TEXT,
    is_online                  INTEGER,            -- 0/1
    has_non_standard_reporting INTEGER,            -- 0/1
    payload                    TEXT,
    PRIMARY KEY (snapshot_at, position_external_id)
);
CREATE INDEX IF NOT EXISTS idx_positions_vehicle ON positions(vehicle_external_id);

-- Per-snapshot portfolio summary (dashboard totals + ratios). The NAV
-- time series and round/vintage insights ride in payload.
CREATE TABLE IF NOT EXISTS portfolio_summary (
    snapshot_at             INTEGER PRIMARY KEY,
    invest_account_slug     TEXT,
    currency                TEXT,
    data_date               INTEGER,
    total_committed_minor   INTEGER,
    total_contributed_minor INTEGER,
    total_invested_minor    INTEGER,
    total_realized_minor    INTEGER,
    total_unrealized_minor  INTEGER,
    total_value_minor       INTEGER,
    irr                     REAL,
    tvpi                    REAL,
    dpi                     REAL,
    total_funds_count       INTEGER,
    total_investments_count INTEGER,
    total_startups_count    INTEGER,
    payload                 TEXT
);

-- Per-snapshot open (unfunded) commitments — the obligation side.
-- (wireInfo / banking details are deliberately NOT promoted to columns.)
CREATE TABLE IF NOT EXISTS commitments (
    snapshot_at             INTEGER NOT NULL,
    commitment_external_id  TEXT    NOT NULL,      -- openInvestment id
    name                    TEXT,                  -- opportunity.investableName
    investable_slug         TEXT,                  -- opportunity.investableSlug
    syndicate_name          TEXT,
    syndicate_slug          TEXT,
    opportunity_type        TEXT,                  -- e.g. FundCampaign
    state                   TEXT,
    currency                TEXT,
    commitment_minor        INTEGER,               -- commitmentAmount
    payment_minor           INTEGER,               -- paymentAmount
    remaining_to_fund_minor INTEGER,               -- remainingAmountNeededToFund
    close_date              INTEGER,               -- opportunity.closeDate (unix seconds)
    funding_deadline        TEXT,                  -- opportunity.fundingDeadlineDate
    needs_to_wire           INTEGER,               -- 0/1
    needs_to_sign           INTEGER,               -- 0/1
    payload                 TEXT,
    PRIMARY KEY (snapshot_at, commitment_external_id)
);

INSERT INTO schema_meta (silver_schema_version) VALUES (1);
