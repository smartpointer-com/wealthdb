-- ============================================================
-- gold schema, migration 0153 — a proxy's divergence compares only
-- what the proxy's file states.
--
-- exposure_divergence reads a line's sector and country as the ones
-- the leaves take: its security's, else the line's own label. A proxy
-- whose file labels no country still has some lines placed by their
-- securities' own facts, so its country mix holds those alone and reads
-- as a divergence that is not there. A dimension the proxy's file
-- labels on none of its lines is left out; one a line of the file
-- labels itself is compared. A report states no sector of its own and a
-- country only in some layouts; its securities place the rest, so the
-- report side keeps the fallback. A pair left with no dimension leaves
-- the divergence section.
--
--   * own_sector, own_country: the line labels the dimension itself.
--   * stated: each fund's dimensions its proxy's file labels.
--
-- Carried forward whole from 0126; `lines` read once (MATERIALIZED),
-- its two flags, `stated` and its use in `paired` are the only
-- changes.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

-- exposure_divergence: per fund whose look-through reads a report and
-- whose config names a proxy with a composition of its own on the day,
-- how far the two compositions' equity lines lie apart: by sector and
-- by country, half the sum of the differences in each one's share, in
-- points. A line's sector and country are the ones the leaves take: its
-- security's, else the line's own label. Each mix holds only the lines
-- that state its dimension, and a dimension counts only where a line
-- of the proxy's file labels it itself. Only lines of positive
-- weight count, so a short line does not offset a long one.
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
    lines AS MATERIALIZED (
        SELECT s.fund_ref, s.proxy_ref, s.side, l.weight::DOUBLE AS w,
               COALESCE(sec.sector, ls.value) AS sector,
               COALESCE(sec.risk_country, lc.value, sec.domicile) AS country,
               trim(COALESCE(l.sector, '')) <> '' AS own_sector,
               trim(COALESCE(l.risk_country, l.country, '')) <> '' AS own_country
          FROM sides s
          JOIN (SELECT *, exposure_key(security_key) AS key FROM fund_lines) l
            ON l.silver_source_id = s.src AND l.fund_ref = s.ref AND l.as_of = s.as_of AND l.basis = s.basis
          LEFT JOIN fund_line_labels la ON la.provider = s.provider AND la.field = 'asset_type' AND la.raw = l.asset_type
          LEFT JOIN fund_line_labels ls ON ls.provider = s.provider AND ls.field = 'sector' AND ls.raw = l.sector
          LEFT JOIN fund_line_labels lc ON lc.provider = s.provider AND lc.field = 'country'
                                       AND lc.raw = COALESCE(l.risk_country, l.country)
          LEFT JOIN securities sec ON sec.security_key = l.key
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
    stated AS (
        SELECT fund_ref, 'sector' AS dim FROM lines WHERE side = 'proxy' AND own_sector
        UNION
        SELECT fund_ref, 'country' FROM lines WHERE side = 'proxy' AND own_country),
    paired AS (SELECT fund_ref, dim FROM mix GROUP BY 1, 2 HAVING COUNT(DISTINCT side) = 2
               INTERSECT SELECT fund_ref, dim FROM stated),
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

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (153, CAST(epoch(now()) AS BIGINT));
