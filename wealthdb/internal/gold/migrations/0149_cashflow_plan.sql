-- ============================================================
-- gold schema, migration 0149 — the cash-flow plan's look-back parts,
-- served to the Cash Flow dashboard's Plan section (docs/CASHFLOW.md
-- §12).
--
-- The plan sets the income projection against the household's
-- outflows. Its investment income and principal are the projection's,
-- which web_yield_parts, web_yield_rates and web_yield_events of 0138
-- already serve under every picker. The plan splits them at the pool's
-- boundary (web_cashflow_pool). The rest depends on no picker, so a web
-- refresh stores it once (gold.MaterializeYield):
--
--   * report_cashflow_plan: per reporting currency, month after the
--     current one (1 to 36, the projection's), part and class, the
--     part's figure in that month: other_income, operating_out (one row
--     per operating-out class), debt_service and vehicles, each a
--     monthly average of the look-back (gold.CashflowPlanLookback), the
--     debt service ending at each mortgage's maturity. operating_out and
--     debt_service are positive outflows, vehicles signed as the
--     statement signs it. quality is the month's tags of these parts:
--     trailing, terms, thin.
--   * web_cashflow_plan: the table as the dashboard reads it.
--   * web_cashflow_pool: the accounts of the cash pool
--     (cashflow_pool_accounts of 0080). A projected part on one of them
--     is the plan's investment income or principal. A part on any other
--     account is the plan's vehicles' income.
--
-- IF NOT EXISTS and OR REPLACE keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

CREATE TABLE IF NOT EXISTS report_cashflow_plan (
    computed_at  BIGINT   NOT NULL,
    currency     VARCHAR  NOT NULL,
    month        INTEGER  NOT NULL,
    month_start  DATE     NOT NULL,
    part         VARCHAR  NOT NULL,
    class        VARCHAR  NOT NULL,
    value        DOUBLE   NOT NULL,
    quality      VARCHAR  NOT NULL
);

CREATE OR REPLACE VIEW web_cashflow_plan AS
SELECT currency, month, month_start, part, class, value, quality FROM report_cashflow_plan;

CREATE OR REPLACE VIEW web_cashflow_pool AS
SELECT silver_source_id, account_external_id FROM cashflow_pool_accounts();

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (149, CAST(epoch(now()) AS BIGINT));
