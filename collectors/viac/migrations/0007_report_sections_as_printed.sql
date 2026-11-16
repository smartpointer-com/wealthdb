-- ============================================================
-- viac silver schema, migration 0007 — a report row's asset class is
-- its section as printed.
--
-- The Reporting-PDF pass writes a position's `asset_class` as the
-- section header the report prints above it (Equity, Real Estate, …),
-- and the gold adapter maps that label. The live REST rows keep their
-- own class. Rows written before held a class the parser mapped from
-- the section. The printed section is in the same row, in
-- `viac_asset_class`, so the rows are rewritten to it here:
--
--   * positions with a `report:<docid>` source;
--   * instruments a report entered (payload source 'report'), whose
--     payload `asset_class` key is rewritten too. A live load rewrites
--     an instrument's class, section and payload together, so such a
--     row's section is still the report's.
-- ============================================================
BEGIN;

UPDATE positions
   SET asset_class = viac_asset_class
 WHERE source LIKE 'report:%'
   AND viac_asset_class IS NOT NULL;

UPDATE instruments
   SET asset_class = viac_asset_class,
       payload = json_set(payload, '$.asset_class', viac_asset_class)
 WHERE json_extract(payload, '$.source') = 'report'
   AND viac_asset_class IS NOT NULL;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (7, CAST(strftime('%s','now') AS INTEGER));

COMMIT;
