-- ============================================================
-- ubs-psn silver schema, migration 0005 —
--   promote the cost an MT535 holding states to columns.
--
-- An MT535 holding states its cost in three places, all kept as raw
-- SWIFT text in payload.fields:
--
--   - `:19A::BOOK//<CCY><amount>`: the total book cost.
--   - `AVER` in the `:70C::SUBB//` narrative: the average unit cost,
--     in the same currency as BOOK.
--   - `AEXR` in the same narrative: the average acquisition FX rate
--     between the instrument currency and the reference currency.
--     One unit of `from` is `rate` units of `to`.
--
-- The narrative also carries `AHOD`, which restates BOOK and is not
-- promoted.
--
-- New columns on `holdings`, all nullable. NULL means the holding does
-- not state the value. Figures are stored as printed, with no
-- conversion:
--
--   cost_basis            BOOK amount
--   cost_currency         BOOK currency, which AVER shares. holdings has
--                         no currency column of its own, so this is set
--                         whenever cost_basis or average_cost is.
--   average_cost          AVER amount, per unit
--   acquisition_fx_rate   AEXR rate
--   acquisition_fx_from   AEXR first currency (the instrument's)
--   acquisition_fx_to     AEXR second currency (the reference)
--
-- A trade_confirmation payload carries `charges_amount` /
-- `charges_currency` (`:19A::CHAR//`) beside the transaction tax and
-- stamp duty. They are payload keys, so they need no DDL here.
--
-- Backfill
-- --------
-- SQL cannot reuse the loader's SWIFT parse, so this migration only
-- adds the columns. On every run, load.py's backfill_cost_fields fills
-- the rows loaded before this migration from what silver already
-- stores: a holding's FIN block in payload.fields, a confirmation's
-- block 4 in payload.raw_fields. No bronze is read, and a filled row
-- equals a freshly loaded one.
-- ============================================================

ALTER TABLE holdings ADD COLUMN cost_basis          REAL;
ALTER TABLE holdings ADD COLUMN cost_currency       TEXT;
ALTER TABLE holdings ADD COLUMN average_cost        REAL;
ALTER TABLE holdings ADD COLUMN acquisition_fx_rate REAL;
ALTER TABLE holdings ADD COLUMN acquisition_fx_from TEXT;
ALTER TABLE holdings ADD COLUMN acquisition_fx_to   TEXT;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (5, CAST(strftime('%s', 'now') AS INTEGER));
