-- ============================================================
-- gold schema, migration 0130 — the coverage view names what the tags
-- ledger lacks.
--
-- report_exposure_coverage: the private holdings with no tag leave the
-- `unmatched` section for one of their own, `untagged`; both list each
-- holding with its silver_source_id and instrument, the two columns a
-- ledger row names (docs/EXPOSURE.md §9), so a holding at two sources
-- has a line for each. The macro is otherwise 0129's.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

-- report_exposure_coverage: what the figures rest on. Section `how`
-- splits net worth by how each leaf is classified and sums to it;
-- `age` splits what compositions describe by their distance from the
-- day; `unmapped` lists the provider labels no map knows; `unmatched`
-- the listed securities held directly that no source describes;
-- `untagged` the private holdings the tags ledger has no row for, each
-- with the silver_source_id and instrument a row names. In the last
-- three, what has no gross value shares one line, `(no value)`.
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
         GROUP BY section_rank, section)
    SELECT section, reason, items, value, 100 * value / NULLIF(t.total, 0) AS share_pct, detail,
           silver_source_id, instrument
      FROM (SELECT * FROM how UNION ALL SELECT * FROM age UNION ALL SELECT * FROM listed) x, t
     ORDER BY section_rank, reason_rank, abs(value) DESC, reason, silver_source_id, instrument
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (130, CAST(epoch(now()) AS BIGINT));
