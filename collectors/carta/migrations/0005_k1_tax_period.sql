-- ============================================================
-- carta silver schema, migration 0005 — a K-1's fiscal tax period.
--
-- A K-1's heading reads "For calendar year YYYY, or tax year beginning …
-- ending …". A fiscal-year form fills in the two dates; a calendar-year form
-- leaves them blank. The loader stores the dates the form prints and takes
-- the tax year from the first of them: a fiscal year is filed on the form
-- edition of the year it begins in. A calendar-year form keeps the
-- heading's year and leaves both columns NULL (DESIGN.md §5.3).
--
-- Backfill: none. The dates were never parsed, so existing rows gain them
-- on a reload from bronze (`load --force`).
-- ============================================================

BEGIN;

ALTER TABLE k1_capital_accounts ADD COLUMN period_start TEXT;   -- fiscal year's first day (YYYY-MM-DD)
ALTER TABLE k1_capital_accounts ADD COLUMN period_end   TEXT;   -- fiscal year's last day (YYYY-MM-DD)

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (5, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
