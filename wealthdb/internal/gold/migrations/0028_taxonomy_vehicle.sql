-- Two-dimensional instrument taxonomy: split exposure (asset_class)
-- from wrapper (vehicle). See docs/TAXONOMY.md.
--
-- Transition schema: the legacy `asset_class` column stays as-is (a
-- control), and two nullable columns are added alongside it —
-- `asset_class_new` (the V2 exposure) and `vehicle` (the wrapper).
-- Adapters populate them source-by-source (NULL where not yet
-- migrated). At cutover a later migration drops the legacy column and
-- renames asset_class_new -> asset_class.
--
-- Nullable (no NOT NULL, no default): a NULL pair means "this row's
-- source hasn't been migrated yet", distinct from a real ('other',
-- 'other') classification.

-- IF NOT EXISTS: the go-duckdb driver processes DDL twice when several
-- statements share one multi-statement Exec (prepare + execute), so a
-- bare ADD COLUMN would raise "column already exists" on the second
-- pass. Idempotent ADD sidesteps that and makes the migration
-- re-runnable.
ALTER TABLE instruments ADD COLUMN IF NOT EXISTS asset_class_new TEXT;
ALTER TABLE instruments ADD COLUMN IF NOT EXISTS vehicle         TEXT;

ALTER TABLE positions   ADD COLUMN IF NOT EXISTS asset_class_new TEXT;
ALTER TABLE positions   ADD COLUMN IF NOT EXISTS vehicle         TEXT;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (28, CAST(epoch(now()) AS BIGINT));
