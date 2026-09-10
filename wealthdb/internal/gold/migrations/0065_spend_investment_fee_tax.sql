-- Two extensions for what holding investments costs.
--
-- Brokerage accounts joined the spending scope in migration 0064, and
-- they book two things in volume that the vendored vocabulary has no
-- word for.
--
-- BANK_FEES_INVESTMENT_FEES. The vendored BANK_FEES values are ATM
-- fees, foreign-transaction fees, insufficient funds, interest charges
-- and overdrafts — every one a fee for BANKING. A custodian's ADR
-- depositary charge, a pension platform's quarterly fee and an
-- investment manager's bill are fees for INVESTING, and filing them
-- under `OTHER_BANK_FEES` buries the cost of being invested inside the
-- cost of having an account. It sits under BANK_FEES all the same:
-- that is the primary for "what a financial institution charged", and
-- an extension earns its keep by landing where a vendored value would,
-- so a later taxonomy refresh supersedes it as a clean diff.
--
-- GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX. Withholding is a tax and
-- belongs beside TAX_PAYMENT, but is not the same thing: a tax payment
-- is assessed and then paid, while withholding is deducted before the
-- money is ever received. A report that cannot tell them apart cannot
-- answer "what was paid in tax that was never seen" — which, where a
-- portfolio holds foreign dividend payers, can be the larger share.
--
-- Both are ordinary judgements about what a row IS, so the model tier
-- may emit them (canonical.ModelSpendDetailed derives from the
-- vendored rows plus the extensions), and usefully can: a withheld-tax
-- row's signature typically reads `NRA TAX <security>`.
--
-- Seeded from internal/canonical/spendtaxonomy.go exactly as
-- migrations 0040, 0045-0047 and 0056 seed theirs, and carrying the
-- columns the later migrations added: the display labels of 0058 and
-- the catch-all flag of 0060. Neither value is a catch-all — each
-- names a real thing — so both are FALSE rather than left NULL.
--
-- spending_lines_base is NOT re-issued: its exclusion list names
-- `internal_transfer` and `investment` one by one (0045), so a row
-- resolving to either value passes it by construction.
INSERT OR REPLACE INTO spend_categories
    (spend_primary, spend_detailed, description, label, primary_label, catch_all)
VALUES
    ('BANK_FEES', 'BANK_FEES_INVESTMENT_FEES',
     'Fees for holding or managing investments — advisory and management fees, custody and platform fees, and security-level pass-throughs such as ADR depositary charges; not a fee for banking itself',
     'Investment fees', 'Bank fees', FALSE),
    ('GOVERNMENT_AND_NON_PROFIT', 'GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX',
     'Tax withheld at source from investment income, such as foreign dividend or non-resident withholding; deducted before the money is received rather than paid on assessment',
     'Withholding tax', 'Government and non profit', FALSE);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (65, CAST(epoch(now()) AS BIGINT));
