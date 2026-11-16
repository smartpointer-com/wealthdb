-- ============================================================
-- carta silver schema, migration 0007 — a cash flow's date as unix seconds.
--
-- cash_flows.flow_date keeps the date as the source prints it, in one of
-- two shapes: 'YYYY-MM-DD' (a cancellation date) and 'MM/DD/YYYY'
-- (certificates, statements, notices, supplied rows). flow_at is that day
-- at UTC midnight, in unix seconds, so a reader compares days without
-- parsing text. It is NULL when flow_date is NULL, is not exactly one of
-- the two shapes, or names no calendar day (a February 30th).
--
-- Backfill: every row, from flow_date, by the rule the loader writes new
-- rows by (common._flow_at).
-- ============================================================

BEGIN;

ALTER TABLE cash_flows ADD COLUMN flow_at INTEGER;   -- flow_date at UTC midnight, unix seconds

UPDATE cash_flows
   SET flow_at = CAST(strftime('%s', flow_date) AS INTEGER)
 WHERE flow_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'
   AND date(flow_date) = flow_date;

UPDATE cash_flows
   SET flow_at = CAST(strftime('%s', substr(flow_date, 7, 4) || '-'
                                    || substr(flow_date, 1, 2) || '-'
                                    || substr(flow_date, 4, 2)) AS INTEGER)
 WHERE flow_date GLOB '[0-9][0-9]/[0-9][0-9]/[0-9][0-9][0-9][0-9]'
   AND date(substr(flow_date, 7, 4) || '-' || substr(flow_date, 1, 2) || '-'
            || substr(flow_date, 4, 2))
       = substr(flow_date, 7, 4) || '-' || substr(flow_date, 1, 2) || '-'
         || substr(flow_date, 4, 2);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (7, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
