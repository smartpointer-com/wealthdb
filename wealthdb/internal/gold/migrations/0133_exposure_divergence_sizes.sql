-- ============================================================
-- gold schema, migration 0133 — divergence over what each side states;
-- sizes on the Exposure dashboard.
--
-- exposure_divergence: 0131's, but each mix holds only the lines that
-- state the dimension: a line with no sector, or no country, is left
-- out of that mix rather than counted as a group of its own. So a proxy
-- whose lines state no country, and that no source places, compares on
-- sectors alone.
--
-- report_exposure gains `size`, the size of a public equity leaf's
-- security (exposure_materialize.go); web_exposure reads it as the CLI's
-- sizes view does: `unclassified` where none is known, `(none)` outside
-- public equity.
--
-- OR REPLACE and IF NOT EXISTS keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

CREATE OR REPLACE MACRO exposure_divergence(p_asof) AS TABLE (
    WITH fa AS MATERIALIZED (SELECT * FROM exposure_funds_at(p_asof)),
    sides AS (
        SELECT f.fund_ref, p.fund_ref AS proxy_ref, x.side,
               CASE x.side WHEN 'fund' THEN f.silver_source_id ELSE p.silver_source_id END AS src,
               CASE x.side WHEN 'fund' THEN f.stored_ref ELSE p.stored_ref END AS ref,
               CASE x.side WHEN 'fund' THEN f.as_of ELSE p.as_of END AS as_of,
               CASE x.side WHEN 'fund' THEN f.basis ELSE p.basis END AS basis,
               CASE x.side WHEN 'fund' THEN f.provider ELSE p.provider END AS provider
          FROM fa f
          JOIN fund_compositions c ON c.silver_source_id = f.silver_source_id AND c.fund_ref = f.stored_ref
                                  AND c.as_of = f.as_of AND c.basis = f.basis
          JOIN fa p ON p.fund_ref = f.proxy_ref AND p.via = 'direct',
               (VALUES ('fund'), ('proxy')) x(side)
         WHERE f.via = 'direct' AND c.source_kind = 'report'),
    lines AS (
        SELECT s.fund_ref, s.proxy_ref, s.side, l.weight::DOUBLE AS w,
               COALESCE(sec.sector, ls.value) AS sector,
               COALESCE(sec.risk_country, lc.value, sec.domicile) AS country
          FROM sides s
          JOIN fund_lines l ON l.silver_source_id = s.src AND l.fund_ref = s.ref AND l.as_of = s.as_of
                           AND l.basis = s.basis
          LEFT JOIN fund_line_labels la ON la.provider = s.provider AND la.field = 'asset_type' AND la.raw = l.asset_type
          LEFT JOIN fund_line_labels ls ON ls.provider = s.provider AND ls.field = 'sector' AND ls.raw = l.sector
          LEFT JOIN fund_line_labels lc ON lc.provider = s.provider AND lc.field = 'country'
                                       AND lc.raw = COALESCE(l.risk_country, l.country)
          LEFT JOIN security_aliases sa ON sa.alias = l.security_key
          LEFT JOIN securities sec ON sec.security_key = COALESCE(sa.security_key, l.security_key)
         WHERE COALESCE(la.value, 'public_equity') = 'public_equity' AND l.weight > 0),
    mix AS (
        SELECT fund_ref, proxy_ref, side, dim, bucket,
               SUM(w) / SUM(SUM(w)) OVER (PARTITION BY fund_ref, side, dim) AS share
          FROM (SELECT fund_ref, proxy_ref, side, w, 'sector' AS dim, sector AS bucket FROM lines
                 WHERE sector IS NOT NULL
                UNION ALL
                SELECT fund_ref, proxy_ref, side, w, 'country', country FROM lines
                 WHERE country IS NOT NULL)
         GROUP BY fund_ref, proxy_ref, side, dim, bucket),
    paired AS (SELECT fund_ref, dim FROM mix GROUP BY 1, 2 HAVING COUNT(DISTINCT side) = 2),
    apart AS (
        SELECT COALESCE(f.fund_ref, p.fund_ref) AS fund_ref, COALESCE(f.proxy_ref, p.proxy_ref) AS proxy_ref,
               COALESCE(f.dim, p.dim) AS dim, 50 * SUM(abs(COALESCE(f.share, 0) - COALESCE(p.share, 0))) AS points
          FROM (SELECT * FROM mix WHERE side = 'fund') f
          FULL JOIN (SELECT * FROM mix WHERE side = 'proxy') p
            ON p.fund_ref = f.fund_ref AND p.dim = f.dim AND p.bucket = f.bucket
          SEMI JOIN paired b ON b.fund_ref = COALESCE(f.fund_ref, p.fund_ref) AND b.dim = COALESCE(f.dim, p.dim)
         GROUP BY 1, 2, 3)
    SELECT fund_ref, proxy_ref,
           MAX(points) FILTER (WHERE dim = 'sector') AS sector_points,
           MAX(points) FILTER (WHERE dim = 'country') AS country_points
      FROM apart GROUP BY fund_ref, proxy_ref
);

ALTER TABLE report_exposure ADD COLUMN IF NOT EXISTS size TEXT;

-- web_exposure: 0128's, with the size of each public equity row.
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
    VALUES (133, CAST(epoch(now()) AS BIGINT));
