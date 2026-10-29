-- ============================================================
-- gold schema, migration 0169 — the cash flow projection's other
-- income by class and account (docs/CASHFLOW.md §12).
--
-- other_income is the operating income the income projection leaves
-- out, a monthly average of the look-back. report_cashflow_plan keeps it
-- split by the statement's operating-in class and by the account the
-- lines arrived on, so a reader can show it either way:
--
--   * silver_source_id and account_external_id: the account of an
--     other_income row, empty on every other part's.
--   * class: on an other_income row, the operating-in class of its
--     lines (earnings, yield, benefits, other_receipts or
--     (uncategorized)).
--
-- The split sums to the figure `wealthdb cashflow projection` prints.
-- web_cashflow_plan serves the two columns.
--
-- web_income_days (0146) also starts at the first income line of any
-- type, so a Time range reaches every line Net income counts: the
-- received figures read every income line, not the six types alone.
--
-- IF NOT EXISTS and OR REPLACE keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

ALTER TABLE report_cashflow_plan ADD COLUMN IF NOT EXISTS silver_source_id VARCHAR DEFAULT '';
ALTER TABLE report_cashflow_plan ADD COLUMN IF NOT EXISTS account_external_id VARCHAR DEFAULT '';

CREATE OR REPLACE VIEW web_cashflow_plan AS
SELECT currency, month, month_start, part, class, silver_source_id, account_external_id, value, quality
  FROM report_cashflow_plan;

CREATE OR REPLACE VIEW web_income_days AS
WITH t AS (SELECT CAST(epoch_ms(max(as_of) * 1000) AS DATE) AS today FROM report_yield_positions),
s AS (SELECT least(make_date(year(today) - 1, 1, 1), (SELECT min(day) FROM report_yield_received),
                   (SELECT min(day) FROM web_income_withheld),
                   (SELECT CAST(min(occurred_at) AS DATE) FROM web_income)) AS first, today
        FROM t)
SELECT CAST(d AS DATE) AS day
  FROM (SELECT unnest(generate_series(CAST(first AS TIMESTAMP), CAST(today AS TIMESTAMP), INTERVAL 1 DAY)) AS d
          FROM s);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (169, CAST(epoch(now()) AS BIGINT));
