-- ============================================================
-- fidelity-web silver schema, migration 0014 — the activity
-- identity drops the Symbol cell.
--
-- Fidelity fills an activity row's Symbol in one export and leaves
-- it blank in another, naming the security only inside the Action
-- text. activity_id hashed the cell as exported, so one transaction
-- seen in both kinds of export became two rows. The identity is now
-- the row's economic columns alone (account, run date, kind,
-- quantity, price, amount, settlement date) plus the per-file
-- occurrence index (DESIGN.md §3.3).
--
-- The old ids are unreachable under the new scheme, as in migration
-- 0005. Drop the export rows and the dump ledger: the next load
-- re-ingests every bronze dump and converges on the new identity
-- (every other table re-converges by its content-derived key). Rows
-- of the supplied statements carry `stmt_` ids no export shares and
-- stay.
-- ============================================================

DELETE FROM transactions WHERE activity_id NOT LIKE 'stmt\_%' ESCAPE '\';
DELETE FROM dump_runs;


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (14, CAST(strftime('%s', 'now') AS INTEGER));
