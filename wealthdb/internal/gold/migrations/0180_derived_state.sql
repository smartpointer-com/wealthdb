-- ============================================================
-- gold schema, migration 0180 — the state of the derived set.
--
-- Every macro and view lives in derived/, one file per family, and
-- not in a migration. Migrate applies that set whole where the
-- database's differs from the binary's (derived.go). derived_state
-- holds the hash of the set the database carries: one row, none
-- before the first apply. From here on a migration holds tables and
-- data moves only.
-- ============================================================

CREATE TABLE IF NOT EXISTS derived_state (
    hash TEXT NOT NULL
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (180, CAST(epoch(now()) AS BIGINT));
