-- angellist silver schema v5 — event-sourced position snapshots + immutable
-- offerings, replacing the v1–v4 per-download `positions` table.
--
-- The earlier model stamped every current position at the DOWNLOAD time and
-- left the gold adapter to compute each mark and reconstruct portfolios.
-- That put valuation logic in the adapter and dated the current holdings
-- "as of the download" rather than as of the data. New model (mirrors the
-- equityzen collector): the COLLECTOR replays a per-position event timeline
-- and computes the mark; the adapter just forward-fills.
--
--   offerings          — IMMUTABLE per investment (one row per position /
--                        SPV stake, keyed by the AngelList position id, NOT
--                        by snapshot). Identity + entry terms: the company,
--                        the SPV legal name + EIN (from the linked K-1), the
--                        investment date. Upserted each load.
--
--   position_snapshots — the per-position valuation TIME SERIES: one row per
--                        CAPITAL EVENT, stamped at the EVENT date
--                        (as_of_date), with a collector-computed
--                        market_value_minor + its valuation_basis. Events:
--                        'investment' (original capital, at the investment
--                        date), 'statement' (each annual Schedule K-1 /
--                        capital-account statement, at its tax year-end, the
--                        tax-basis NAV), 'valuation' (the current portal FMV,
--                        at the portfolio data date). is_open flips to 0 at a
--                        final K-1 (exit).
--
-- Reading holdings as of day D: take each position's latest
-- position_snapshots row with as_of_date <= D, then drop the is_open = 0
-- ones. No full-portfolio snapshot per day is stored; an exited position
-- drops out exactly at its exit date.

DROP INDEX IF EXISTS idx_positions_vehicle;
DROP TABLE IF EXISTS positions;

CREATE TABLE offerings (
    position_external_id TEXT    PRIMARY KEY,   -- AngelList position node id (the SPV stake)
    vehicle_external_id  TEXT,                  -- -> vehicles (the company / investableGuid)
    kind                 TEXT,                  -- 'spv' | 'fund'
    company_name         TEXT,                  -- investableName (the underlying company / fund)
    fund_name            TEXT,                  -- SPV legal name (from the linked K-1)
    fund_tax_id          TEXT,                  -- SPV EIN (from the linked K-1)
    investment_date      INTEGER,               -- acquisition (unix seconds UTC)
    currency             TEXT NOT NULL DEFAULT 'USD',
    first_seen_at        INTEGER,
    last_seen_at         INTEGER,
    payload              TEXT
);

CREATE TABLE position_snapshots (
    position_external_id TEXT    NOT NULL,      -- -> offerings
    as_of_date           INTEGER NOT NULL,      -- EVENT date (unix seconds UTC)
    event_type           TEXT    NOT NULL,      -- 'investment' | 'statement' | 'valuation'
    status               TEXT,                  -- AngelList status, or 'exited'
    is_open              INTEGER NOT NULL DEFAULT 1,  -- 1 = still held after this event
    currency             TEXT,
    market_value_minor   INTEGER,               -- the mark at this event (minor units / cents)
    valuation_basis      TEXT,                  -- 'cost' | 'tax_basis' | 'fmv'
    contributed_minor    INTEGER,               -- cumulative capital called as of the event
    distributions_minor  INTEGER,               -- cumulative distributions as of the event
    snapshot_at          INTEGER NOT NULL,      -- download run that computed this row (provenance)
    payload              TEXT,
    PRIMARY KEY (position_external_id, as_of_date)
);
CREATE INDEX idx_psnap_asof ON position_snapshots(as_of_date);

INSERT INTO schema_meta (silver_schema_version) VALUES (5);
