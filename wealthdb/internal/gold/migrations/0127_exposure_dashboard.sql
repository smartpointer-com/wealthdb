-- ============================================================
-- gold schema, migration 0127 — the Exposure dashboard: the
-- look-through at every month end, materialized per reporting
-- currency (docs/EXPOSURE.md §12).
--
-- `wealthdb web-materialize` (MaterializeExposure,
-- exposure_materialize.go) rewrites report_exposure whole at every
-- run: one row per instant, output currency and combination of the
-- dimensions the dashboard charts, the value the leaves of that
-- combination sum to. web_exposure is the dashboard's reading of it.
--
-- OR REPLACE and IF NOT EXISTS keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

CREATE TABLE IF NOT EXISTS report_exposure (
    computed_at        BIGINT   NOT NULL,
    currency           TEXT     NOT NULL,   -- the output currency
    as_of              BIGINT   NOT NULL,   -- a month's last second, or the run's end of today
    asset_class        TEXT,
    sector             TEXT,
    country            TEXT,               -- country of risk
    region             TEXT,
    exposure_currency  TEXT,               -- the currency the value moves with
    currency_kind      TEXT,               -- fiat | metal | crypto
    via_funds          BOOLEAN  NOT NULL,  -- held through funds, not directly
    how                TEXT     NOT NULL,   -- how the leaves are classified (EXPOSURE.md §10)
    value              DOUBLE   NOT NULL,
    size               TEXT                -- a public equity leaf's size
);

-- web_exposure: report_exposure as the dashboard reads it. A missing
-- dimension reads `unclassified` (a metal or a coin has no country:
-- `(none)`; outside public equity there is no size: `(none)`), as in
-- the CLI views; share_pct is the share of that instant's net worth in
-- that currency.
CREATE OR REPLACE VIEW web_exposure AS
SELECT CAST(epoch_ms(r.as_of * 1000) AS DATE) AS as_of_day,
       r.currency,
       r.asset_class,
       COALESCE(r.sector, 'unclassified') AS sector,
       CASE WHEN r.country IS NOT NULL THEN r.country
            WHEN r.asset_class IN ('metal', 'crypto') THEN '(none)' ELSE 'unclassified' END AS country,
       co.name AS country_name,
       COALESCE(r.region, CASE WHEN r.asset_class IN ('metal', 'crypto') THEN '(none)' ELSE 'unclassified' END)
           AS region,
       COALESCE(r.exposure_currency, 'unclassified') AS exposure_currency,
       r.currency_kind,
       CASE WHEN r.asset_class <> 'public_equity' THEN '(none)' ELSE COALESCE(r.size, 'unclassified') END
           AS size,
       CASE WHEN r.via_funds THEN 'via funds' ELSE 'direct' END AS held,
       r.how,
       r.value,
       100 * r.value / NULLIF(SUM(r.value) OVER (PARTITION BY r.as_of, r.currency), 0) AS share_pct
  FROM report_exposure r
  LEFT JOIN exposure_countries co ON co.code = r.country;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (127, CAST(epoch(now()) AS BIGINT));
