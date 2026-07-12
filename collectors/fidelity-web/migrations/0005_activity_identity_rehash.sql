-- ============================================================
-- fidelity-web silver schema, migration 0005 — activity identity
-- rehash.
--
-- activity_id switches from a hash of the row's FULL payload to a
-- hash of its structural columns (account, run date, action kind,
-- symbol, quantity, price, amount, settlement date + occurrence
-- index). The full-payload key broke dedup whenever Fidelity
-- re-labelled a security between exports — the same transaction's
-- Action/Description text drifted (e.g. "SPONSORED ADR" vs an
-- abbreviated form), the hash changed, and overlapping backfill
-- windows grew duplicate rows.
--
-- Old ids are unreachable under the new scheme, so loading new
-- dumps against the existing rows would duplicate everything.
-- Drop the transactions and the dump ledger: the next load
-- re-ingests every bronze dump and converges on the new identity
-- (all other tables re-converge by their content-derived keys).
-- ============================================================

DELETE FROM transactions;
DELETE FROM dump_runs;


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (5, CAST(strftime('%s', 'now') AS INTEGER));
