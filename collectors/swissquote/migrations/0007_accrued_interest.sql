-- ============================================================
-- swissquote silver schema, migration 0007 — a statement bond's
-- accrued interest.
--
-- A Portfolio Performance statement prints a bond's accrued interest
-- on a line of its own under the bond's row, in the valuation column
-- ("Valuation in CHF incl. accrued interests"): a CHF amount on top of
-- the row's valuation, which is the clean quantity × price; the
-- section total adds the two. `accrued_interest_chf` holds it, as
-- printed; NULL on every other row.
--
-- A statement is parsed once, when its positions are first written,
-- so the statement rows are dropped here: the next load parses every
-- statement again and fills the column. Live rows stay.
-- ============================================================

ALTER TABLE positions ADD COLUMN accrued_interest_chf REAL;

DELETE FROM positions WHERE source LIKE 'pp:%';


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (7, CAST(strftime('%s', 'now') AS INTEGER));
