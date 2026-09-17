-- ============================================================
-- gold schema, migration 0087 —
--   the earmarked vehicles are named for what they are.
--
-- `Education` named two different things. A crossing into a 529 or a
-- Coverdell is the household putting money ASIDE for a cost it has not
-- yet met; a tuition payment is the cost itself, and lands in
-- consumption under the spending vocabulary's own `Education` label.
-- Different sections, so no money was ever commingled — but a diagram
-- drawn at group level put two nodes on the page reading the same word,
-- and a reader has no way to tell which is which.
--
-- `Retirement` carries the same ambiguity the day a plan starts paying
-- out: a contribution and a pension are not the same direction of the
-- same thing. Naming the vehicle class for the ACT — setting money
-- aside — leaves `Pensions & benefits` free to name what comes back.
--
-- Health and Trusts are left alone deliberately. "Health savings" would
-- be right for an HSA and wrong for the class the day it holds anything
-- else, and "Trust savings" is not a thing anyone says.
--
-- 0081 issued this macro and 0085 re-issued it whole; both are applied,
-- so it is carried forward again rather than edited. The labels are the
-- only change — every other arm is byte-identical to 0085's.
-- ============================================================

CREATE OR REPLACE MACRO cashflow_class_label(p_class) AS (
    CASE p_class
        -- Operating in: money from labour, from assets, from
        -- entitlements, and everything else that arrived.
        WHEN 'earnings'       THEN 'Earnings'
        WHEN 'yield'          THEN 'Yield'
        WHEN 'benefits'       THEN 'Pensions & benefits'
        WHEN 'other_receipts' THEN 'Other receipts'
        -- Operating out, with the three classes every household-balance
        -- diagram lifts out of spending.
        WHEN 'consumption'    THEN 'Consumption'
        WHEN 'fees'           THEN 'Fees'
        WHEN 'taxes'          THEN 'Taxes'
        WHEN 'giving'         THEN 'Giving'
        -- The backlog is a node on each side, not a leaf hidden inside
        -- one: both families made it a visible label on purpose.
        WHEN '(uncategorized)' THEN 'Uncategorised'
        -- Investing: the whole under the default grain, the rows with
        -- no instrument, then the asset classes.
        WHEN 'investments'       THEN 'Investments'
        WHEN 'elsewhere'         THEN 'Untracked investments'
        WHEN 'public_equity'     THEN 'Public equity'
        WHEN 'private_equity'    THEN 'Private markets'
        WHEN 'fixed_income'      THEN 'Fixed income'
        WHEN 'private_debt'      THEN 'Private debt'
        WHEN 'real_estate'       THEN 'Real estate'
        WHEN 'infrastructure'    THEN 'Infrastructure'
        WHEN 'metal'             THEN 'Metals'
        WHEN 'crypto'            THEN 'Crypto'
        WHEN 'hedge_fund'        THEN 'Hedge funds'
        WHEN 'multi_asset'       THEN 'Multi-asset'
        WHEN 'foreign_exchange'  THEN 'Foreign exchange'
        WHEN 'other'             THEN 'Other'
        -- Financing, the vehicles, and the residual.
        WHEN 'mortgage'   THEN 'Mortgage'
        WHEN 'loans'      THEN 'Loans'
        WHEN 'retirement' THEN 'Retirement savings'
        WHEN 'education'  THEN 'Education savings'
        WHEN 'health'     THEN 'Health'
        WHEN 'trusts'     THEN 'Trusts'
        WHEN 'untracked'  THEN 'Untracked accounts'
        WHEN 'cash'       THEN 'Cash'
        ELSE p_class
    END
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (87, CAST(epoch(now()) AS BIGINT));
