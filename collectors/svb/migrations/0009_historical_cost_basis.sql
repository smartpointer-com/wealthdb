-- ============================================================
-- fidelity-web silver schema, migration 0009 — cost basis and
-- unrealized gain on historical position snapshots.
--
-- A supplied monthly statement prints, per holding, the total cost
-- basis and the unrealized gain or loss beside quantity, price and
-- market value. The parsed row in `payload` carries both (keys
-- `cost_basis` and `unrealized_gain`); these columns promote them so
-- an adapter can select them.
--
--   * Both are the statement's own figures, in USD, as printed.
--   * NULL means the statement states none: the core account prints
--     `not applicable`, a cell may print `--`, and the 529 and DAF
--     statement layouts carry no basis column at all.
--
-- The backfill reads the values straight out of `payload`, so rows
-- loaded before this migration need no re-parse.
-- ============================================================

ALTER TABLE historical_position_snapshots ADD COLUMN cost_basis REAL;            -- USD, total for the holding
ALTER TABLE historical_position_snapshots ADD COLUMN unrealized_gain_loss REAL;  -- USD, signed

UPDATE historical_position_snapshots
   SET cost_basis           = json_extract(payload, '$.cost_basis'),
       unrealized_gain_loss = json_extract(payload, '$.unrealized_gain');


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (9, CAST(strftime('%s', 'now') AS INTEGER));
