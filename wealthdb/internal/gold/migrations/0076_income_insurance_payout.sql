-- INCOME_INSURANCE_PAYOUT — what an insurer pays out on a policy.
--
-- The income side had nowhere to put it. A claim settled, a damage or health
-- cost covered, a premium refunded on cancellation all landed either in
-- INCOME_OTHER_INCOME, which says nothing, or — where a model guessed from
-- the compulsory-health-insurance context — in INCOME_GOVERNMENT_BENEFITS,
-- which is wrong: an insurer is not the state.
--
-- Why it is INCOME and not the `reimbursement` delta, which its description
-- also fits: the premium that bought the cover was already counted as
-- SPENDING (GENERAL_SERVICES_INSURANCE), and nothing in the data links a
-- payout back to the premiums it answers — different amounts, different
-- dates, often different years. Netting the payout out of income would
-- therefore count the outflow and silently drop the inflow, leaving the
-- household permanently short on one side. `reimbursement` stays for money
-- back on a specific outflow that IS identifiable — a utility credit against
-- a bill, a merchant reversing a charge.
INSERT OR REPLACE INTO spend_categories
    (spend_primary, spend_detailed, description, label, primary_label, family)
VALUES
    ('INCOME', 'INCOME_INSURANCE_PAYOUT',
     'What an insurer pays out on a policy — a claim settled, a damage or health cost covered, a premium refunded on cancellation. Income rather than a reimbursement because the premium that bought the cover was already counted as spending, and nothing links a payout back to the premiums it answers: netting the payout out would count the outflow and drop the inflow',
     'Insurance payout', 'Income', 'income');

-- catch_all is a function of the value's own spelling (0060, re-run by
-- 0069), so the new row needs the same rule applied rather than a literal:
-- INCOME_INSURANCE_PAYOUT is not a primary's catch-all and comes out false.
UPDATE spend_categories
   SET catch_all = (spend_primary <> spend_detailed
                    AND starts_with(substr(spend_detailed, length(spend_primary) + 2), 'OTHER_'))
 WHERE catch_all IS NULL;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (76, CAST(epoch(now()) AS BIGINT));
