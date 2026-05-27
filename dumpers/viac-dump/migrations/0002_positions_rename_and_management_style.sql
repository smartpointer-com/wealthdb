-- ============================================================
-- viac-dump silver schema, migration 0002.
--
-- Three changes, all driven by feedback from the first wealthdb
-- VIAC adapter pass:
--
--   1. positions.amount → positions.quantity
--      The column holds fund UNITS (VIAC's JSON key is `amount`,
--      which is confusing — units, not CHF). Rename so the silver
--      column name matches the canonical/gold semantic
--      (quantity = units of a fund held).
--
--   2. positions.ratio_chf → positions.market_value_chf
--      Holds the CHF market value of the position (= quantity ×
--      assetPrice; in VIAC's JSON it's `ratioInChf`). Rename so
--      a SELECT * doesn't mislead anyone querying silver into
--      treating it as "ratio in CHF".
--
--   3. accounts: add `management_style TEXT` column.
--      Today every VIAC product line is robo-managed and the gold
--      adapter defaults to 'automated' for every VIAC account.
--      Promoting the column to silver lets a future non-robo
--      VIAC product surface without a wealthdb release — the
--      adapter reads it via hasColumn-gated SELECT.
--      Backfilled to 'automated' for every existing row.
--
-- product_code (set in migration 0001) is also documented more
-- prominently here. The mapping from VIAC's dotted-number first
-- segment to product line is:
--
--   accounts.product_code values:
--     '3' → Pillar-3a (p3a)
--     '2' → Pillar-2 vested benefits (pvb)
--     '1' → non-retirement investment product line (inv;
--           not observed in any user yet)
--
-- The mapping is set by load.py from the first dotted segment of
-- the portfolio number, e.g. `3.NNN.NNN.NNN.NN` → product_code='3'.
-- ============================================================

BEGIN;

ALTER TABLE positions RENAME COLUMN amount    TO quantity;
ALTER TABLE positions RENAME COLUMN ratio_chf TO market_value_chf;

ALTER TABLE accounts ADD COLUMN management_style TEXT;
UPDATE accounts SET management_style = 'automated'
    WHERE management_style IS NULL;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
