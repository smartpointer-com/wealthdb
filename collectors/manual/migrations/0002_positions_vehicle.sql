-- ============================================================
-- manual silver schema, migration 0002 — position `vehicle` column.
--
-- The two-dimensional instrument taxonomy (wealthdb docs/TAXONOMY.md)
-- splits the single asset class into orthogonal exposure (asset_class)
-- and wrapper (vehicle). The manual collector is the one place the
-- owner supplies the classification directly, so the CSV gains an
-- optional `vehicle` column here.
--
-- `kind` is unchanged — it stays the legacy 1-D asset_class (the gold
-- adapter still maps it identically). `vehicle` records how the
-- exposure is held (physical property, a fund LP interest, an SPV, a
-- private loan, an escrow receivable, ...). When the CSV omits it,
-- load.py fills a sensible default from `kind` (real_estate→physical,
-- private_equity→stock, spv→spv, private_fund→fund,
-- convertible_note→convertible_note, mortgage→mortgage, other→other),
-- so existing CSVs load unchanged. The gold adapter derives the new
-- exposure (asset_class_new) from (kind, vehicle).
--
-- Nullable: the loader always writes a value (explicit or defaulted),
-- but NULL is tolerated for forward compatibility. Allowed vehicle
-- vocabulary lives in load.py (POSITION_VEHICLES), like POSITION_KINDS.

BEGIN;

ALTER TABLE positions ADD COLUMN vehicle TEXT;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
