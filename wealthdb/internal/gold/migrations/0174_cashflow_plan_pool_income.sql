-- ============================================================
-- gold schema, migration 0174 — the income lines in the pool in the
-- cash flow projection's look-back (docs/CASHFLOW.md §12).
--
-- report_cashflow_plan gains a part, pool_income: the income lines in
-- the pool on their own day (cashflow_pool_spans), the types the income
-- projection projects aside, a monthly average of the same look-back
-- over the months other_income spreads over. Its rows are split as
-- outside_income's are (0172): by the operating-in class of the line's
-- income type and by the account.
--
-- other_income stays the statement's figure, which the cash flow
-- projection reads. The two differ where the statement counts a line
-- as operating income and no income line holds it, such as a
-- reimbursement. With pool_income and outside_income a reader has a
-- run-rate of the income lines on every account.
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
    VALUES (174, CAST(epoch(now()) AS BIGINT));
