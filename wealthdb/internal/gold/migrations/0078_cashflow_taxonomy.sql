-- The five values the cash flow statement needs, and the two bases
-- re-issued around them.
--
-- Cashflow reads the verdicts the two families already store, so it
-- adds no tier and almost no vocabulary. The exception is the movement
-- neither family books and whose far side the product does not hold:
-- a loan serviced at an untracked lender, and a crossing to an
-- earmarked pool nobody collects. Without a value for those, the money
-- has no honest home — it reads as uncategorised spending, or as a
-- receipt — and the statement is wrong in a way no reconciliation can
-- see, because the crossing is MISFILED rather than absent.
--
-- `debt_repayment` is the outflow mirror of `loan_proceeds` and is
-- what the dropped LOAN_PAYMENTS primary was for, minus the interest
-- share nothing in the data splits out. Spending family, out of the
-- spending base: it reduces a liability rather than buying anything,
-- exactly as `investment` deploys capital rather than buying anything.
--
-- The four crossings — `retirement_transfer`, `education_transfer`,
-- `health_transfer`, `trust_transfer` — are family 'both' in the
-- manner of `internal_transfer`: one row read from either side,
-- because one movement has a leg on each. The DIRECTION is the row's
-- own, never the value's: a withdrawal placed `retirement_transfer` is
-- a contribution and a deposit placed the same is a distribution. They
-- leave both bases for `internal_transfer`'s reason — the money is
-- still the holder's, and a crossing is neither a receipt nor a thing
-- bought.
--
-- One value per pool rather than one `vehicle_transfer` with a
-- parameter. A rule today is a pattern and a category; carrying which
-- pool it meant would need a second field in that surface, and money
-- set aside for retirement and money set aside for a child's education
-- are different decisions besides. Giving needs no value of its own:
-- the taxonomy's `GOVERNMENT_AND_NON_PROFIT_DONATIONS` already lands
-- there.
--
-- Every one is a DELTA, so the model tier can never emit one
-- (canonical.ModelSpendDetailed / ModelIncomeDetailed derive from the
-- vendored rows plus the extensions). Each is decided from structure a
-- counterparty's name cannot reveal — which institution holds the far
-- account, and what it is for — which is the definition of a delta.
--
-- Seeded from internal/canonical/spendtaxonomy.go as every value
-- before them was, carrying the display labels of 0058 and the family
-- column of 0069. TestSpendCategoriesMatchGoTable pins the dimension
-- to the Go table.
--
-- Replayable for the DDL-rerun test (see gold.Migrate's REPLAY note):
-- the INSERT is OR REPLACE, the catch-all recompute is a function of
-- each row's own spelling, and both macros are OR REPLACE.

INSERT OR REPLACE INTO spend_categories
    (spend_primary, spend_detailed, description, label, primary_label, family)
VALUES
    ('debt_repayment', 'debt_repayment',
     'An instalment paid to a lender the product does not track — a car or student loan serviced, a credit line paid down; principal and interest together, a liability reduced rather than anything consumed. The outflow mirror of `loan_proceeds`; a payment to a lender gold DOES hold is an own-account move and pairs',
     'Debt repayment', 'Debt repayment', 'spending'),
    ('retirement_transfer', 'retirement_transfer',
     'A move between the holder and a retirement plan the product does not track, either leg — a contribution wired out, a plan payout arriving. The row''s own direction says which; the money is the holder''s throughout, in a pool earmarked for a stage of life rather than for spending',
     'Retirement transfer', 'Retirement transfer', 'both'),
    ('education_transfer', 'education_transfer',
     'The same crossing for an education plan or savings account the product does not track — money paid in, or drawn out for the costs it was set aside for',
     'Education transfer', 'Education transfer', 'both'),
    ('health_transfer', 'health_transfer',
     'The same crossing for a health savings account the product does not track — a contribution paid in, or a medical cost reimbursed out of it',
     'Health transfer', 'Health transfer', 'both'),
    ('trust_transfer', 'trust_transfer',
     'The same crossing for a trust that is a separate taxpayer and that the product does not track — a funding transfer out, a distribution arriving. A grantor trust is not this: it is tax-transparent and its accounts are the holder''s own',
     'Trust transfer', 'Trust transfer', 'both');

-- catch_all is a function of a value's own spelling (0060, re-run by
-- 0069 and 0076), so the five new rows take the same rule rather than a
-- literal. A delta is primary-level and therefore never a catch-all;
-- the guard on NULL keeps the recompute to the rows that have not been
-- flagged yet.
UPDATE spend_categories
   SET catch_all = (spend_primary <> spend_detailed
                    AND starts_with(substr(spend_detailed, length(spend_primary) + 2), 'OTHER_'))
 WHERE catch_all IS NULL;

-- ============================================================
-- Both bases re-issued.
--
-- Each names its exclusions ONE BY ONE — 0045 on the outflow side,
-- 0070 on the inflow side — deliberately, so that adding a value to
-- the taxonomy cannot silently remove rows from a base. The cost of
-- that decision is exactly this: five new values mean two re-issued
-- macros, and the alternative (a flag column read by both) would have
-- dropped the rows the day they were seeded, with nothing saying so.
--
-- A value that is out of a base is still ENRICHED and still visible on
-- `wealthdb transactions`: the exclusion is about what a spending or
-- income report charts, and cashflow reads the resolved verdict rather
-- than either base for exactly that reason.
-- ============================================================

-- 0059's body, carried forward whole, less the five.
CREATE OR REPLACE MACRO spending_lines_base(p_from, p_to) AS TABLE (
    SELECT p.silver_source_id, p.transaction_external_id, p.occurred_at,
           p.account_external_id, p.account_kind, p.display_name,
           p.nickname, p.account_category,
           p.kind, p.currency, p.net_amount, p.description,
           p.counterparty, p.provider_category,
           c.merchant_signature, c.merchant_name, c.spend_detailed,
           c.spend_primary, c.spend_label, c.spend_primary_label,
           c.provenance,
           c.provider_spend_detailed, c.provider_spend_primary,
           c.provider_spend_label, c.provider_spend_primary_label
      FROM spend_enrichment_population(p_from, p_to) p
      LEFT JOIN spend_txn_categories() c
             ON c.silver_source_id        = p.silver_source_id
            AND c.transaction_external_id = p.transaction_external_id
     WHERE c.spend_detailed IS DISTINCT FROM 'internal_transfer'
       AND c.spend_detailed IS DISTINCT FROM 'investment'
       AND c.spend_detailed IS DISTINCT FROM 'debt_repayment'
       AND c.spend_detailed IS DISTINCT FROM 'retirement_transfer'
       AND c.spend_detailed IS DISTINCT FROM 'education_transfer'
       AND c.spend_detailed IS DISTINCT FROM 'health_transfer'
       AND c.spend_detailed IS DISTINCT FROM 'trust_transfer'
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

-- 0070's body, carried forward whole, less the four. `debt_repayment`
-- is absent here because it is absent from the income vocabulary: a
-- loan instalment is an outflow, and an income rule naming the value
-- is refused at config load.
CREATE OR REPLACE MACRO income_lines_base(p_from, p_to) AS TABLE (
    SELECT p.silver_source_id, p.transaction_external_id, p.occurred_at,
           p.account_external_id, p.account_kind, p.display_name,
           p.nickname, p.account_category,
           p.kind, p.currency, p.net_amount, p.description,
           p.counterparty, p.provider_category,
           c.payer_signature, c.payer_name, c.income_detailed,
           c.income_primary, c.income_label, c.income_primary_label,
           c.provenance,
           c.provider_income_detailed, c.provider_income_primary,
           c.provider_income_label, c.provider_income_primary_label
      FROM income_enrichment_population(p_from, p_to) p
      LEFT JOIN income_txn_categories() c
             ON c.silver_source_id        = p.silver_source_id
            AND c.transaction_external_id = p.transaction_external_id
     WHERE c.income_detailed IS DISTINCT FROM 'internal_transfer'
       AND c.income_detailed IS DISTINCT FROM 'capital_return'
       AND c.income_detailed IS DISTINCT FROM 'loan_proceeds'
       AND c.income_detailed IS DISTINCT FROM 'reimbursement'
       AND c.income_detailed IS DISTINCT FROM 'retirement_transfer'
       AND c.income_detailed IS DISTINCT FROM 'education_transfer'
       AND c.income_detailed IS DISTINCT FROM 'health_transfer'
       AND c.income_detailed IS DISTINCT FROM 'trust_transfer'
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (78, CAST(epoch(now()) AS BIGINT));
