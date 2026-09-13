-- The income side of the vocabulary, and the column that tells the two
-- families apart.
--
-- `spend_categories` has held one vocabulary since migration 0040: what
-- a merchant sold. It now holds two, because the same dimension answers
-- what a payer paid, and the two are read by different macros, validated
-- by different predicates and chosen from by different conversations
-- with the model. A `family` column says which vocabulary a row belongs
-- to — 'spending', 'income', or 'both' for the deltas that mean the same
-- thing whichever way the money moved. Every row seeded before this
-- migration is a spending value, and three of those become shared.
--
-- The seven INCOME values are Plaid's, verbatim. INCOME is the FIRST
-- primary in transactions-personal-finance-category-taxonomy.csv and was
-- dropped by the original vendoring because nothing read it yet; the
-- values and descriptions are the CSV's, untouched, so a refreshed CSV
-- still diffs cleanly against the table.
--
-- Eight EXTENSIONS sit under INCOME in the vendored shape, so the model
-- may emit them and a future Plaid value supersedes one as a clean diff
-- rather than sitting beside it. They name what a household commonly
-- receives and the vendored vocabulary has no word for: self-employment,
-- state benefits other than a pension or unemployment, rent, royalties,
-- maintenance, staking, rewards, and a fund's payout of realised gains.
--
-- Five DELTAS are new and primary-level like the six before them.
-- `capital_return`, `loan_proceeds` and `reimbursement` name money that
-- arrived without being earned and are excluded from the income base;
-- `inheritance` and `cash_deposit` are receipts in their own right and
-- stay in it. `internal_transfer`, `gift` and `other` are re-seeded as
-- family 'both' with a description that no longer reads as an outflow.
--
-- Seeded from internal/canonical/spendtaxonomy.go as migrations 0040,
-- 0045-0047, 0056 and 0065 seed theirs, carrying the display labels of
-- 0058. TestSpendCategoriesMatchGoTable pins the dimension to the Go
-- table, family included.
--
-- Nothing else changes. No spending macro selects on family — the
-- spending base names the values it excludes one by one (0045), and
-- every reader of this dimension joins it on spend_detailed rather than
-- enumerating it — so the rows added here are invisible to every
-- spending report until the income base is built over them.
--
-- Replayable for the DDL-rerun test (see gold.Migrate's REPLAY note):
-- the ALTER is IF NOT EXISTS, the INSERTs are OR REPLACE, and both
-- UPDATEs are idempotent — the first is guarded on NULL, the second
-- computes each flag from the row itself.
ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS family TEXT;

-- Every value the dimension already held is a spending value. The guard
-- is not for the rows below — the INSERTs re-assert their family a few
-- statements later, so a replay restores them either way — but for rows
-- a LATER migration seeds into a family of its own. Replayed against a
-- database that already holds one, an unguarded stamp would demote it to
-- spending, and nothing downstream would say so.
UPDATE spend_categories SET family = 'spending' WHERE family IS NULL;

-- Plaid's INCOME primary, then our extensions under it, then the deltas
-- the income side adds.
INSERT OR REPLACE INTO spend_categories
    (spend_primary, spend_detailed, description, label, primary_label, family)
VALUES
    ('INCOME', 'INCOME_DIVIDENDS',
     'Dividends from investment accounts',
     'Dividends', 'Income', 'income'),
    ('INCOME', 'INCOME_INTEREST_EARNED',
     'Income from interest on savings accounts',
     'Interest earned', 'Income', 'income'),
    ('INCOME', 'INCOME_RETIREMENT_PENSION',
     'Income from pension payments',
     'Retirement pension', 'Income', 'income'),
    ('INCOME', 'INCOME_TAX_REFUND',
     'Income from tax refunds',
     'Tax refund', 'Income', 'income'),
    ('INCOME', 'INCOME_UNEMPLOYMENT',
     'Income from unemployment benefits, including unemployment insurance and healthcare',
     'Unemployment', 'Income', 'income'),
    ('INCOME', 'INCOME_WAGES',
     'Income from salaries, gig-economy work, and tips earned',
     'Wages', 'Income', 'income'),
    ('INCOME', 'INCOME_OTHER_INCOME',
     'Other miscellaneous income, including alimony, social security, child support, and rental',
     'Other income', 'Income', 'income'),
    ('INCOME', 'INCOME_SELF_EMPLOYMENT',
     'Freelance, contractor and sole-trader earnings — client invoices, a business''s own takings, an owner''s draw from their company; not a salary, tips or gig-platform earnings, which are wages',
     'Self employment', 'Income', 'income'),
    ('INCOME', 'INCOME_GOVERNMENT_BENEFITS',
     'State transfers other than a pension or an unemployment benefit — child and family allowances, parental-leave pay, disability and housing benefits, stimulus payments',
     'Government benefits', 'Income', 'income'),
    ('INCOME', 'INCOME_RENT',
     'Rent received from a tenant, directly or through a letting agent or a property manager; not a tenancy deposit returned and not the proceeds of selling the property',
     'Rent', 'Income', 'income'),
    ('INCOME', 'INCOME_ROYALTIES',
     'Royalties and creator payouts — book, music, software-licence and patent royalties, and a platform''s share of what a creator''s work earned',
     'Royalties', 'Income', 'income'),
    ('INCOME', 'INCOME_ALIMONY_AND_CHILD_SUPPORT',
     'Maintenance received from a former partner or a parent — alimony, spousal maintenance, child support; not a cash gift and not family support given freely',
     'Alimony and child support', 'Income', 'income'),
    ('INCOME', 'INCOME_STAKING',
     'Proof-of-stake rewards and validator income earned by committing a crypto holding; kept apart from interest because jurisdictions tax the two differently',
     'Staking', 'Income', 'income'),
    ('INCOME', 'INCOME_REWARDS',
     'Card cashback, statement credits, account-opening and referral bonuses, and airdrops — earned on spending or on holding an account, and never netted against the spending itself',
     'Rewards', 'Income', 'income'),
    ('INCOME', 'INCOME_DISTRIBUTIONS',
     'A fund''s cash payout of realised gains on a holding still held, returning no basis; not a company''s dividend, and not a private fund returning contributed capital',
     'Distributions', 'Income', 'income'),
    ('capital_return', 'capital_return',
     'Capital of the holder''s own coming back from a destination the product does not track — a private fund returning contributed basis, a loan the holder made repaid, a personal asset sold, a deposit refunded; returned rather than earned',
     'Capital return', 'Capital return', 'income'),
    ('loan_proceeds', 'loan_proceeds',
     'Money borrowed arriving from a lender the product does not track — a loan disbursed, a mortgage or a credit line drawn; a liability incurred rather than income',
     'Loan proceeds', 'Loan proceeds', 'income'),
    ('reimbursement', 'reimbursement',
     'Money back for money spent — an insurance payout, an expense claim settled, a merchant refunding by bank transfer; a repayment of an outflow rather than income',
     'Reimbursement', 'Reimbursement', 'income'),
    ('inheritance', 'inheritance',
     'An estate''s distribution to the holder; kept apart from a gift because it arrives once or twice in a life and is often the largest receipt in it',
     'Inheritance', 'Inheritance', 'income'),
    ('cash_deposit', 'cash_deposit',
     'Cash paid in at a counter or a machine; where it came from is unobservable',
     'Cash deposit', 'Cash deposit', 'income');

-- The three deltas both families read: one row each, re-seeded with a
-- direction-neutral description and family 'both'.
INSERT OR REPLACE INTO spend_categories
    (spend_primary, spend_detailed, description, label, primary_label, family)
VALUES
    ('internal_transfer', 'internal_transfer',
     'Movement between two accounts the product already tracks, either leg — card payments, funding wires, mortgage payments, a pension contribution arriving',
     'Internal transfer', 'Internal transfer', 'both'),
    ('gift', 'gift',
     'A cash gift or family support, given or received — with no merchant, employer or issuer behind it; not a gift item bought in a shop, and not a donation to a non-profit',
     'Gift', 'Gift', 'both'),
    ('other', 'other',
     'Money moved that no rule, matcher or model could place — a payment on the outflow side, a receipt on the inflow side',
     'Other', 'Other', 'both');

-- Migration 0060's rule, carried forward whole rather than restated as a
-- list of TRUE and FALSE. It has to run again because it ran ONCE, at
-- version 60, and can never see a row seeded after it: all twenty new
-- rows would carry no flag at all — which is not FALSE, and is what
-- TestSpendCategoryCatchAllMatchesGoTable refuses. The three re-seeded
-- deltas keep theirs, an INSERT OR REPLACE naming a subset of the
-- columns writing only that subset, so the recompute is a no-op for
-- every row but the twenty and idempotent for those, each flag being a
-- function of the value's own spelling. It leaves income's one catch-all
-- marked by the same rule that marks spending's twelve.
UPDATE spend_categories
   SET catch_all = (spend_primary <> spend_detailed
                    AND starts_with(substr(spend_detailed, length(spend_primary) + 2), 'OTHER_'));

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (69, CAST(epoch(now()) AS BIGINT));
