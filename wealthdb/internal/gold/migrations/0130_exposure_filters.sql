-- ============================================================
-- gold schema, migration 0130 — the Exposure dashboard narrows to a
-- source and a tax wrapper (web/DESIGN.md §6). report_exposure gains
-- the leaf's holding's source and its account's tax wrapper, two more
-- keys the materializer groups on (exposure_materialize.go), and
-- web_exposure exposes them. The view's share of net worth goes: a
-- share of what the pickers select is each card's own, computed over
-- the rows it reads.
--
-- OR REPLACE and IF NOT EXISTS keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

ALTER TABLE report_exposure ADD COLUMN IF NOT EXISTS silver_source_id TEXT;
ALTER TABLE report_exposure ADD COLUMN IF NOT EXISTS tax_wrapper TEXT;

-- web_exposure: report_exposure as the dashboard reads it, every label
-- filled in: a missing dimension reads unclassified, a metal's or a
-- coin's country and region (none), the size of anything but public
-- equity (none). silver_source_id and tax_wrapper are the holding's
-- source and its account's wrapper, NULL where the account states none.
CREATE OR REPLACE VIEW web_exposure AS
SELECT CAST(epoch_ms(r.as_of * 1000) AS DATE) AS as_of_day,
       r.currency,
       r.silver_source_id,
       r.tax_wrapper,
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
       COALESCE(r.situs, 'unclassified') AS situs,
       COALESCE(r.situs_basis, 'unclassified') AS situs_basis,
       r.value
  FROM report_exposure r
  LEFT JOIN exposure_countries co ON co.code = r.country;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (130, CAST(epoch(now()) AS BIGINT));
