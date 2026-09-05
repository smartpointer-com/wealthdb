-- Two-dimensional instrument taxonomy: split exposure (asset_class)
-- from wrapper (vehicle). See docs/TAXONOMY.md.
--
-- Transition schema: the legacy `asset_class` column stays as-is (a
-- control), and two nullable columns are added alongside it —
-- `asset_class_new` (the V2 exposure) and `vehicle` (the wrapper).
-- Adapters populate them source-by-source (NULL where not yet
-- migrated). Cutover landed in migration 0031: `asset_class` itself
-- now carries the V2 exposure and `asset_class_new` is left in place,
-- dead and unprojected — DuckDB refuses to drop a column the report
-- macros depend on.
--
-- Nullable (no NOT NULL, no default): a NULL pair means "this row's
-- source hasn't been migrated yet", distinct from a real ('other',
-- 'other') classification.

-- IF NOT EXISTS keeps this replayable for the DDL-rerun test (see
-- gold.Migrate's REPLAY note): a bare ADD COLUMN would raise "column
-- already exists" when the body is re-executed against an
-- already-migrated database.
ALTER TABLE instruments ADD COLUMN IF NOT EXISTS asset_class_new TEXT;
ALTER TABLE instruments ADD COLUMN IF NOT EXISTS vehicle         TEXT;

ALTER TABLE positions   ADD COLUMN IF NOT EXISTS asset_class_new TEXT;
ALTER TABLE positions   ADD COLUMN IF NOT EXISTS vehicle         TEXT;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (28, CAST(epoch(now()) AS BIGINT));
