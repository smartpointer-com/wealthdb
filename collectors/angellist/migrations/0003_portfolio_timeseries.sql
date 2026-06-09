-- Portfolio NAV time series (PortfolioDashboardQuery summary.timeSeries):
-- a ~monthly history of portfolio value / invested / realized / unrealized
-- that AngelList computes back to the first investment — longer than our
-- own snapshot history and not reconstructable from positions alone, so
-- worth promoting out of portfolio_summary.payload.
--
-- One row per (invest_account_slug, as_of_date), upserted latest-snapshot-
-- wins: each download re-reports the full series (and may revise past
-- months as valuations settle), so the newest snapshot's value for a given
-- date overwrites an older one.

CREATE TABLE IF NOT EXISTS portfolio_timeseries (
    invest_account_slug  TEXT    NOT NULL,
    as_of_date           TEXT    NOT NULL,   -- 'YYYY-MM-DD'
    currency             TEXT,
    total_value_minor    INTEGER,
    total_invested_minor INTEGER,
    realized_minor       INTEGER,
    unrealized_minor     INTEGER,
    offline_change_minor INTEGER,
    is_approximate       INTEGER,            -- 0/1
    snapshot_at          INTEGER NOT NULL,   -- provenance: dump that last wrote this row
    PRIMARY KEY (invest_account_slug, as_of_date)
);

INSERT INTO schema_meta (silver_schema_version) VALUES (3);
