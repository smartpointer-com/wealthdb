-- ============================================================
-- equityzen silver schema, migration 0002 — promote the per-cash-flow
-- share count + price/share into first-class columns.
--
-- Both numbers already ride in the cash_flow `payload`
-- (sharesPostSplit / pricePostSplit on the purchase + each distributed
-- transaction). Promoting them follows the silver's "stable columns are
-- promoted" convention and serves the gold adapter directly: it maps each
-- distribution by the offering's kind (DESIGN.md §6) — an SPV (single-
-- stock, tax-transparent) distribution is a realization of the underlying
-- and becomes a canonical `sell`, which needs the shares sold + price; a
-- multi-company-fund distribution stays a `distribution` (no share
-- semantics). With these columns the adapter reads the `sell` quantity /
-- price as plain columns instead of digging through JSON, and the cash
-- ledger is self-describing. Populated for purchases too (entry shares +
-- price). Additive — existing rows backfill on the next (re)load.
-- ============================================================

ALTER TABLE cash_flows ADD COLUMN shares          REAL;  -- sharesPostSplit (shares bought / sold-or-distributed at the event)
ALTER TABLE cash_flows ADD COLUMN price_per_share REAL;  -- pricePostSplit  (CLOSED price/share at the event)

-- Migration-complete marker — must be the LAST statement.
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s', 'now') AS INTEGER));
