-- ============================================================
-- schwab-web silver schema, migration 0013 — the rebuild level.
--
-- The highest REBUILD_ADVICE level (load.py) the loader knew when this
-- silver was built from empty (collectorkit.rebuild.REBUILD_LEVEL_DDL).
-- A load of a silver built before an entry prints that entry's line of
-- advice, and goes on. Seeded EMPTY: a silver that has loaded runs reads
-- as level 0, and the first load of an empty one records its level.
-- ============================================================

CREATE TABLE rebuild_level (
    level INTEGER NOT NULL
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (13, CAST(strftime('%s','now') AS INTEGER));
