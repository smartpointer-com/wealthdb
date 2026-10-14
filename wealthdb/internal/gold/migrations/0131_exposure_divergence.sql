-- ============================================================
-- gold schema, migration 0131 — a report's mix against its proxy's.
--
-- exposure_divergence: per fund whose look-through reads a report and
-- whose config names a proxy with a composition of its own on the day,
-- how far the two compositions' equity lines lie apart: by sector and
-- by country, half the sum of the differences in each one's share, in
-- points. A line's sector and country are the ones the leaves take: its
-- security's, else the line's own label; one with neither counts as its
-- own bucket. Only lines of positive weight count, so a short line
-- does not offset a long one. A dimension one side has no line for is
-- left out.
--
-- report_exposure_coverage: 0130's, with a `divergence` section.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
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
               COALESCE(sec.sector, ls.value, '(none)') AS sector,
               COALESCE(sec.risk_country, lc.value, sec.domicile, '(none)') AS country
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
                UNION ALL
                SELECT fund_ref, proxy_ref, side, w, 'country', country FROM lines)
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

-- report_exposure_coverage: what the figures rest on. Section `how`
-- splits net worth by how each leaf is classified and sums to it;
-- `age` splits what compositions describe by their distance from the
-- day; `unmapped` lists the provider labels no map knows; `unmatched`
-- the listed securities held directly that no source describes;
-- `untagged` the private holdings the tags ledger has no row for, each
-- with the silver_source_id and instrument a row names. In those three,
-- what has no gross value shares one line, `(no value)`. `divergence`
-- lists each fund read from a report beside its proxy: the value held
-- through it, and how far the two mixes lie apart.
CREATE OR REPLACE MACRO report_exposure_coverage(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l),
    how AS (
        SELECT 1 AS section_rank, 'how' AS section, how AS reason, COUNT(*) AS items, SUM(value) AS value,
               CAST(NULL AS VARCHAR) AS detail, CAST(NULL AS VARCHAR) AS silver_source_id,
               CAST(NULL AS VARCHAR) AS instrument,
               CASE how WHEN 'looked_through' THEN 1 WHEN 'proxied' THEN 2 WHEN 'security' THEN 3
                        WHEN 'tagged' THEN 4 WHEN 'cash' THEN 5 WHEN 'asset_class' THEN 6
                        WHEN 'remainder' THEN 7 WHEN 'truncated' THEN 8 ELSE 9 END AS reason_rank
          FROM l GROUP BY how),
    age AS (
        SELECT 2, 'age', bucket, COUNT(DISTINCT top_key), SUM(value), CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR),
               CAST(NULL AS VARCHAR), bucket_rank
          FROM (SELECT *,
                       CASE WHEN forward THEN 'forward'
                            WHEN abs(gap_days) <= 35 THEN '0-35 days'
                            WHEN abs(gap_days) <= 95 THEN '36-95 days'
                            WHEN abs(gap_days) <= 185 THEN '96-185 days'
                            ELSE '186+ days' END AS bucket,
                       CASE WHEN forward THEN 5 WHEN abs(gap_days) <= 35 THEN 1 WHEN abs(gap_days) <= 95 THEN 2
                            WHEN abs(gap_days) <= 185 THEN 3 ELSE 4 END AS bucket_rank
                  FROM l WHERE depth > 0)
         GROUP BY bucket, bucket_rank),
    unmapped_items AS (
        SELECT 3 AS section_rank, 'unmapped' AS section, f.field || ': ' || f.raw AS reason, COUNT(*) AS items,
               SUM(f.value) AS value, SUM(abs(f.value)) AS gross, f.provider AS detail,
               CAST(NULL AS VARCHAR) AS silver_source_id, CAST(NULL AS VARCHAR) AS instrument
          FROM (SELECT 'sector' AS field, sector_raw AS raw, provider, value FROM l
                 WHERE depth > 0 AND sector_raw IS NOT NULL AND sector_map IS NULL
                UNION ALL
                SELECT 'country', country_raw, provider, value FROM l
                 WHERE depth > 0 AND country_raw IS NOT NULL AND country_map IS NULL
                UNION ALL
                SELECT 'asset_type', asset_type_raw, provider, value FROM l
                 WHERE depth > 0 AND class_unmapped) f
         GROUP BY f.field, f.raw, f.provider),
    unmatched_items AS (
        SELECT CASE WHEN untagged THEN 5 ELSE 4 END AS section_rank,
               CASE WHEN untagged THEN 'untagged' ELSE 'unmatched' END AS section,
               holding AS reason, COUNT(*) AS items, SUM(value) AS value, SUM(abs(value)) AS gross,
               MAX(name) AS detail, src AS silver_source_id, iid AS instrument
          FROM (SELECT *, COALESCE(key, 'holding:' || src || ':' || COALESCE(iid, position_key)) AS holding,
                       list_contains(flags, 'untagged') AS untagged FROM l
                 WHERE list_contains(flags, 'unmatched') OR list_contains(flags, 'untagged'))
         GROUP BY untagged, holding, src, iid),
    -- A label or holding whose leaves have no gross value is listed
    -- only as a count.
    listed AS (
        SELECT section_rank, section, reason, items, value, detail, silver_source_id, instrument, 0 AS reason_rank
          FROM (SELECT * FROM unmapped_items UNION ALL BY NAME SELECT * FROM unmatched_items)
         WHERE round(gross, 2) <> 0
        UNION ALL
        SELECT section_rank, section, '(no value)', CAST(SUM(items) AS BIGINT), SUM(value), CAST(NULL AS VARCHAR),
               CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), 1
          FROM (SELECT * FROM unmapped_items UNION ALL BY NAME SELECT * FROM unmatched_items)
         WHERE round(gross, 2) = 0
         GROUP BY section_rank, section),
    divergence AS (
        SELECT 6, 'divergence', d.fund_ref, v.items, v.value,
               concat_ws(', ', printf('sector %.1f', d.sector_points), printf('country %.1f', d.country_points))
                   || ' points from ' || d.proxy_ref,
               CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), 0
          FROM exposure_divergence(p_asof) d
          JOIN (SELECT fund_ref, COUNT(DISTINCT key) AS items, SUM(value) AS value FROM l WHERE depth > 0 GROUP BY 1) v
            USING (fund_ref))
    SELECT section, reason, items, value, 100 * value / NULLIF(t.total, 0) AS share_pct, detail,
           silver_source_id, instrument
      FROM (SELECT * FROM how UNION ALL SELECT * FROM age UNION ALL SELECT * FROM listed
            UNION ALL SELECT * FROM divergence) x, t
     ORDER BY section_rank, reason_rank, abs(value) DESC, reason, silver_source_id, instrument
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (131, CAST(epoch(now()) AS BIGINT));
