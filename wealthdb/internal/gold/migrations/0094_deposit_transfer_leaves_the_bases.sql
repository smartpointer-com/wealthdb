-- ============================================================
-- gold schema, migration 0094 —
--   a bank deposit is not something the household bought, and not
--   something it earned.
--
-- 0090 added `deposit_transfer` to the taxonomy and never added it to
-- the two lists that keep a delta out of the family bases. The docs
-- were written as though it had: SPENDING.md §2 and INCOME.md §2 both
-- say the value leaves the base. It did not, and both reports counted
-- it — money moved INTO a call deposit read as spending, the principal
-- coming back read as income, and a year that opened a deposit
-- overstated by the size of the deposit.
--
-- The seven values above each line were excluded for the same reason
-- this one is: the money is still the household's. A delta is added to
-- the taxonomy and to these two lists together, or the vocabulary and
-- the reports disagree — which is what happened, silently, because a
-- value nothing excludes still resolves and still draws.
--
-- Carried forward whole from 0078; the one added line in each is the
-- only change.
-- ============================================================

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
      AND c.spend_detailed IS DISTINCT FROM 'deposit_transfer'
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

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
      AND c.income_detailed IS DISTINCT FROM 'deposit_transfer'
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (94, CAST(epoch(now()) AS BIGINT));
