-- ============================================================
-- gold schema, migration 0146 — the days the Income dashboard's
-- received figures cover, and the tax withheld at source on them
-- (web/DESIGN.md §6).
--
-- The Income dashboard's cards of the holdings' income put what was
-- received over the Time range picker beside what is expected over the
-- Horizon. A card reads the income received and the tax withheld from
-- two views, and a picker reaches one template tag of a card. So the
-- Time range reaches both through a third view of days:
--
--   * web_income_withheld: the tax withheld at source on the income's
--     accounts, one row per tax line, its value in each reporting
--     currency as a positive amount. These are the lines the income
--     summary's `withheld` memo reads (migration 0071): every negative
--     `tax` row on those accounts. It is what a source booked, not the
--     withholding table's model.
--   * web_income_days: each day to today from the earliest of the first
--     day income was received (report_yield_received, which holds the
--     income lines of the six types the projection counts,
--     gold.MaterializeYield), the first day tax was
--     withheld, and the first day of last year. Today is the day of the
--     yield report's latest instant. A card keeps the received rows and
--     the withheld tax of the days the picker selects, and counts the
--     months of the selection by those days.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

CREATE OR REPLACE VIEW web_income_withheld AS
SELECT t.silver_source_id, t.account_external_id,
       account_label(a.display_name, t.account_external_id, t.silver_source_id, a.account_kind) AS account_label,
       CAST(epoch_ms(t.occurred_at * 1000) AS DATE) AS day,
       -t.value_usd AS value_usd, -t.value_chf AS value_chf, -t.value_eur AS value_eur, -t.value_gbp AS value_gbp
  FROM serving_transactions t
  JOIN income_scoped_accounts() a
    ON a.silver_source_id = t.silver_source_id AND a.account_external_id = t.account_external_id
 WHERE t.kind = 'tax' AND t.net_amount < 0;

CREATE OR REPLACE VIEW web_income_days AS
WITH t AS (SELECT CAST(epoch_ms(max(as_of) * 1000) AS DATE) AS today FROM report_yield_positions),
s AS (SELECT least(make_date(year(today) - 1, 1, 1), (SELECT min(day) FROM report_yield_received),
                   (SELECT min(day) FROM web_income_withheld)) AS first, today
        FROM t)
SELECT CAST(d AS DATE) AS day
  FROM (SELECT unnest(generate_series(CAST(first AS TIMESTAMP), CAST(today AS TIMESTAMP), INTERVAL 1 DAY)) AS d
          FROM s);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (146, CAST(epoch(now()) AS BIGINT));
