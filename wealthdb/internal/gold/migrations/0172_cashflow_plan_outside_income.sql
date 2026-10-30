-- ============================================================
-- gold schema, migration 0172 — the income lines outside the pool in
-- the cash flow projection's look-back (docs/CASHFLOW.md §12).
--
-- report_cashflow_plan gains a part, outside_income: the income lines
-- outside the pool on their own day (cashflow_pool_spans), the types the
-- income projection projects aside, a monthly average of the same
-- look-back. Its rows are split as other_income's are (0169): by the
-- statement's operating-in class and by the account. Each account's
-- average spreads over the window's months from its first such line.
--
-- The cash flow projection reads no outside_income row: the household
-- boundary keeps the income of a vehicle, or of an account
-- cashflow.accounts fences out, out of the pool. With both parts a
-- reader has a run-rate for every account an income line arrives on.
--
-- The table needs no new column. web_cashflow_plan is redefined as it
-- was, so a reader can pin to the version whose refresh writes the
-- part.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

CREATE OR REPLACE VIEW web_cashflow_plan AS
SELECT currency, month, month_start, part, class, silver_source_id, account_external_id, value, quality
  FROM report_cashflow_plan;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (172, CAST(epoch(now()) AS BIGINT));
