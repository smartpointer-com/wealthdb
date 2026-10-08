-- ============================================================
-- fidelity-web silver schema, migration 0013 — the account and tax
-- year of each Consolidated 1099 in `documents`.
--
-- The Consolidated 1099 pass reads each copy's identity off the form:
-- its account and tax year. It files both into the copy's `documents`
-- row wherever the row holds NULL. The newer file name states neither,
-- so the file name alone leaves them NULL.
--
-- The pass reads only the dumps a load ingests, unless its generation
-- is stale; then it reads every dump in bronze. Dropping the stamp
-- here makes the next load read every dump once, which fills the
-- copies loaded before this migration. It also re-derives the 1099-B
-- lots, from the same parser, into the same rows.
-- ============================================================

DELETE FROM parser_generations WHERE scope = 'consolidated_1099';


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (13, CAST(strftime('%s', 'now') AS INTEGER));
