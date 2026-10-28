-- ============================================================
-- gold schema, migration 0166 — the day the Allocation dashboard
-- reads for a chosen day.
--
-- The Allocation cards read the daily holdings history on the As of
-- day. The history ends on the day of the last refresh, so a later day
-- read nothing. web_history_calendar maps each day to the last history
-- day on or before it:
--
--   * as_of_day: every day from the history's first, through the later
--     of its last and tomorrow, UTC whatever the session's time zone.
--     Tomorrow covers a reader whose today is ahead of UTC;
--   * history_day: the last day of serving_global_history on or before
--     it, as the history views render a day.
--
-- A day the history holds maps to itself, a day after it to its last
-- day. A day before the history has no row. A card filters the view on
-- the chosen day and reads its rows of history_day.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

CREATE OR REPLACE VIEW web_history_calendar AS
    WITH h AS (SELECT DISTINCT as_of_day FROM serving_global_history),
    b AS (
        SELECT min(as_of_day) AS lo,
               greatest(max(as_of_day), CAST(epoch(now()) AS BIGINT) // 86400 * 86400 + 86400) AS hi
          FROM h),
    d AS (SELECT unnest(range(lo, hi + 1, 86400)) AS day FROM b WHERE lo IS NOT NULL)
    SELECT epoch_ms(d.day * 1000) AS as_of_day, epoch_ms(h.as_of_day * 1000) AS history_day
      FROM d ASOF JOIN h ON d.day >= h.as_of_day;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (166, CAST(epoch(now()) AS BIGINT));
