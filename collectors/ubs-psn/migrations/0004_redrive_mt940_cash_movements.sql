-- ============================================================
-- ubs-psn silver schema, migration 0004 —
--   re-derive the MT940 cash_movement slice from bronze.
--
-- Background
-- ----------
-- Two defects in the MT940 (Z40) loader destroyed real bookings, and
-- both leave rows that no amount of future loading would repair.
--
--   1. The event id was the :61: bank reference alone. UBS books its
--      own charge under the same reference as the transfer it belongs
--      to, so the second of the two entries REPLACEd the first and the
--      charge disappeared.
--
--   2. The statement's DELETE was keyed on a value-date range while the
--      rows are timestamped with the :61: value date. An entry booked
--      for a forward value date lands outside its own statement's
--      window and is deleted — never re-inserted — by the next
--      statement, whose window does cover that date.
--
-- Why this migration deletes rather than repairs
-- ----------------------------------------------
-- What is wrong with the affected rows is not their shape: the missing
-- ones were never written, and the survivors carry no record of which
-- statement booked them, which is exactly what the fixed loader deletes
-- on. Neither can be reconstructed from silver — only the statements in
-- bronze know. So the whole cash_movement slice goes, and the loader
-- writes it again from the statements themselves. That is the reload
-- contract in DESIGN §6, narrowed to the one slice that needs it: the
-- deleted rows are re-derivable in full, because prune.py never
-- reclaims a run directory that holds a zip, so every Z40 statement
-- ever fetched is still on disk.
--
-- dump_runs goes with it, because it is what would otherwise prevent
-- the re-derive: the loader skips a dump it has already recorded, and
-- the skip is per dump, not per order type, so there is no way to ask
-- for the MT940 files alone. Replaying every dump is safe and already
-- exercised — it is what `download --recover` does every time it lands
-- dated archive copies of batches silver has already seen. Every table
-- either upserts on its natural key or dedups its change-point compare
-- at the file's as-of date, so the replay converges to the rows that
-- are already there, and re-fills dump_runs as it goes.
--
-- cash_balances is deliberately left alone: it is keyed per snapshot
-- and per balance kind, and the replay rewrites each row identically.
-- ============================================================

DELETE FROM events WHERE kind = 'cash_movement';
DELETE FROM dump_runs;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s', 'now') AS INTEGER));
