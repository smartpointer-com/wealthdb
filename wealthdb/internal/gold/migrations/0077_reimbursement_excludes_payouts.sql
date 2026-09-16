-- `reimbursement` stops claiming the insurance payout.
--
-- 0076 gave a payout its own income value and said why: the premium that
-- bought the cover was already counted as spending, and nothing links a
-- payout back to the premiums it answers, so netting it out would count the
-- outflow and drop the inflow. What it did not do is take the payout out of
-- the description on `reimbursement`, which had led with it since 0069 —
-- leaving the published vocabulary shipping two rows that both claim it.
--
-- That is not cosmetic. `income.rules` validates against these values, so a
-- reader following the description maps an insurer to `reimbursement`,
-- config accepts it, and the income base excludes it by name: the payout is
-- netted away while the premium still counts, which is the double-hit 0076
-- exists to prevent.
--
-- What is left to `reimbursement` is what it was always for — money back on
-- an outflow someone can actually point at.
UPDATE spend_categories
   SET description = 'Money back for money spent, where the outflow it answers is identifiable — an expense claim settled, a utility credit against a bill, a merchant reversing its own charge; a repayment of an outflow rather than income. An insurance payout is not this and is income (INCOME_INSURANCE_PAYOUT): the premium was already counted as spending, and nothing links a payout to the premiums it answers'
 WHERE spend_detailed = 'reimbursement';

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (77, CAST(epoch(now()) AS BIGINT));
