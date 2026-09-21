-- ============================================================
-- gold schema, migration 0101 —
--   a mortgage servicer the product does not hold gets a verdict.
--
-- `financing · Mortgage` has been reachable two ways, and both need
-- the far side: a far account of kind `mortgage`, or `far_class =
-- 'mortgage'`, which only the built-in tier may write and which the
-- resolution clears everywhere else. So an instalment to a servicer
-- gold holds no account for had no road to that node at all — it fell
-- through to `vehicles · Untracked accounts`, drawn beside the
-- destinations nothing could name.
--
-- The narrative is why the built-in tier cannot answer it. A servicer
-- is its own legal entity and the row names it; a tracked built-in
-- rule carrying that name would put a counterparty in the repository,
-- which is the one thing the config tier exists to keep out.
--
-- `mortgage_transfer` is that verdict. It is a delta beside the
-- vehicle crossings and reaches `place = 'mortgage'` directly, ahead
-- of any far-account test, so it needs neither road. `debt_repayment`
-- stays the value for every other untracked lender and keeps drawing
-- as `loans`: a mortgage is split into interest and principal and a
-- car loan is not, which is the whole of the difference.
--
-- The direction is the ROW's, as for the crossings — an outflow is an
-- instalment, an inflow a drawdown — and `cashflow_lines_base` (0096)
-- already reads the sign. A servicer with no balance series in gold
-- has no principal to apportion, so its instalments draw whole as
-- `Mortgage interest`: exact for an interest-only tranche, and a
-- visible approximation otherwise.
--
-- Carried forward whole from 0100 (the node macro) and 0094 (the two
-- family bases); the one `place` arm, the dimension row and the one
-- added exclusion in each base are the only changes.
--
-- Replayable for the DDL-rerun test: the INSERT is OR REPLACE, the
-- catch_all recompute is a function of the row's own spelling, and the
-- macro is OR REPLACE.
-- ============================================================

INSERT OR REPLACE INTO spend_categories
    (spend_primary, spend_detailed, description, label, primary_label, family)
VALUES
    ('mortgage_transfer', 'mortgage_transfer',
     'An instalment paid to, or a tranche drawn from, a mortgage servicer the product does not hold as an account — the row''s own direction says which. Drawn under financing beside a tracked mortgage rather than as a vehicle crossing; `debt_repayment` is the value for any other untracked lender. A payment to a mortgage gold does hold is an own-account move and pairs instead',
     'Mortgage transfer', 'Mortgage transfer', 'both');

UPDATE spend_categories
   SET catch_all = (spend_primary <> spend_detailed
                    AND starts_with(substr(spend_detailed, length(spend_primary) + 2), 'OTHER_'))
 WHERE catch_all IS NULL;

CREATE OR REPLACE MACRO cashflow_txn_nodes(p_from, p_to) AS TABLE (
    WITH pool AS (
        SELECT silver_source_id, account_external_id FROM cashflow_pool_accounts()
    ),
    joined AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
               t.account_external_id, t.kind, t.currency, t.net_amount,
               t.description, t.counterparty, t.instrument_external_id,
               a.account_kind, a.display_name, a.nickname, a.account_category,
               -- What was TRADED, the trade's own word first. A feed
               -- that cannot name the instrument can still name the
               -- exposure — a precious-metal sale, a capital call, a
               -- subscription right — and 0097 gave it somewhere to
               -- say so. Coalescing HERE rather than at each use is
               -- what makes every rule below read the same answer,
               -- the cash-is-internal test included.
               COALESCE(t.asset_class, i.asset_class) AS asset_class,
               COALESCE(i.name, i.symbol) AS instrument_name,
               sc.spend_detailed, sc.merchant_name, sc.provenance AS spend_provenance,
               ic.income_detailed, ic.payer_name, ic.provenance AS income_provenance,
               e.far_silver_source_id, e.far_account_external_id, e.far_class,
               fw.side  AS far_side,
               fw.class AS far_wrapper_class,
               fa.account_kind AS far_account_kind,
               (fa.silver_source_id IS NOT NULL)    AS far_known,
               (fp.account_external_id IS NOT NULL) AS far_pooled
          FROM transactions t
          LEFT JOIN accounts a
                 ON a.silver_source_id    = t.silver_source_id
                AND a.account_external_id = t.account_external_id
          LEFT JOIN instruments i
                 ON i.silver_source_id       = t.silver_source_id
                AND i.instrument_external_id = t.instrument_external_id
          LEFT JOIN spend_txn_categories() sc
                 ON sc.silver_source_id        = t.silver_source_id
                AND sc.transaction_external_id = t.transaction_external_id
          LEFT JOIN income_txn_categories() ic
                 ON ic.silver_source_id        = t.silver_source_id
                AND ic.transaction_external_id = t.transaction_external_id
          -- The overlay itself, not its resolution macro: the far
          -- account is a raw fact about a pairing rather than part of
          -- the spending vocabulary (migration 0079).
          LEFT JOIN spend_txn_enrichment e
                 ON e.silver_source_id        = t.silver_source_id
                AND e.transaction_external_id = t.transaction_external_id
          LEFT JOIN accounts fa
                 ON fa.silver_source_id    = e.far_silver_source_id
                AND fa.account_external_id = e.far_account_external_id
          LEFT JOIN cashflow_wrapper_sides fw ON fw.tax_wrapper = fa.tax_wrapper
          LEFT JOIN pool fp
                 ON fp.silver_source_id    = e.far_silver_source_id
                AND fp.account_external_id = e.far_account_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to
    ),
    verdicted AS (
        SELECT j.*,
               CASE WHEN j.kind IN ('dividend', 'coupon', 'staking', 'capital_gain',
                                    'reward', 'distribution', 'deposit')
                      OR (j.kind = 'interest' AND j.net_amount > 0)
                    THEN COALESCE(j.income_detailed, j.spend_detailed)
                    ELSE COALESCE(j.spend_detailed, j.income_detailed)
               END AS verdict
          FROM joined j
    ),
    -- THE ORDERED FAR-ACCOUNT TEST (docs/CASHFLOW.md §4). First match
    -- wins, and
    -- the WRAPPER is asked before the account KIND: a mortgage on a
    -- trust-owned property and a card inside a foundation are both, and
    -- the wrapper is what says whose money moved.
    --
    -- A far account whose wrapper is unset falls past the first three
    -- arms to the pool test, which admits it — the same render-time
    -- default the pool macro applies, reached by the same rule rather
    -- than by accident.
    --
    -- The last two arms are where a rule-placed move lands, there being
    -- no far account to read at all: the mortgage rule's own word if it
    -- left one, and otherwise the household's money at an institution
    -- the product does not collect. Drawn as invisible that money would
    -- vanish into the residual forever; drawn as a node it is visible as
    -- what it is, and the remedy — collect that account — is obvious.
    far AS (
        SELECT v.*,
               CASE
                   WHEN v.far_side = 'vehicle'                     THEN 'vehicle'
                   WHEN v.far_side = 'giving' AND v.net_amount < 0  THEN 'giving_out'
                   WHEN v.far_side = 'giving'                       THEN 'giving_in'
                   WHEN v.far_account_kind = 'mortgage'             THEN 'mortgage'
                   WHEN v.far_pooled                                THEN 'internal'
                   WHEN v.far_class = 'mortgage'                    THEN 'mortgage'
                   ELSE 'untracked'
               END AS far_place
          FROM verdicted v
    ),
    -- THE KIND TABLE (docs/CASHFLOW.md §4), as one ladder over the verdict and
    -- the kind. Ordered because the verdict outranks the kind wherever
    -- both have something to say: an own-account move is sorted by its
    -- far account whatever kind carried it, and a withdrawal that
    -- deployed capital is investing rather than spend.
    placed AS (
        SELECT f.*,
               -- The dimension row of the resolved verdict, read once:
               -- its primary is what tells a fee from a tax from a
               -- donation without restating the taxonomy here.
               vc.spend_primary AS verdict_primary,
               CASE
                   -- THE KINDS NO VERDICT MAY PLACE, tested ahead of
                   -- every verdict arm. gold pins no canonical sign for
                   -- these six — the adapter passes the source's
                   -- through — so their direction is a per-source
                   -- convention, and a section that reads direction
                   -- cannot admit them. A pin or a rule may call such a
                   -- row an own-account move; it still cannot make an
                   -- unsigned amount readable. `fx` is cash becoming
                   -- cash besides, and the two catch-alls are the
                   -- exclusion both families already make.
                   WHEN f.kind IN ('fx', 'fx_forward', 'fx_swap',
                                   'corporate_action', 'journal', 'other')
                        THEN 'excluded'
                   -- An own-account move: the money stayed the holder's,
                   -- and the far account says whether it stayed in the
                   -- pool.
                   WHEN f.verdict = 'internal_transfer' THEN
                        CASE f.far_place
                            WHEN 'vehicle'    THEN 'vehicle_far'
                            WHEN 'giving_out' THEN 'giving_out'
                            WHEN 'giving_in'  THEN 'giving_in'
                            WHEN 'mortgage'   THEN 'mortgage'
                            WHEN 'internal'   THEN 'internal'
                            ELSE 'vehicle_untracked'
                        END
                   -- A mortgage servicer the product holds no account
                   -- for. Ahead of the crossings and needing no far
                   -- account: the two roads to `financing · Mortgage`
                   -- both read the far side, and this verdict exists
                   -- for the rows where there is none to read.
                   WHEN f.verdict = 'mortgage_transfer' THEN 'mortgage'
                   -- The four crossings whose far side the product does
                   -- not hold. The direction is the ROW's: a withdrawal
                   -- placed `retirement_transfer` is a contribution and a
                   -- deposit placed the same is a distribution.
                   WHEN f.verdict IN ('retirement_transfer', 'education_transfer',
                                      'health_transfer', 'trust_transfer',
                                      'deposit_transfer')
                        THEN 'vehicle_delta'
                   -- Debt at a lender the product does not track, in
                   -- either direction.
                   WHEN f.verdict IN ('loan_proceeds', 'debt_repayment') THEN 'loans'
                   -- Capital deployed to, or returned from, a
                   -- destination the product does not track.
                   WHEN f.verdict IN ('investment', 'capital_return') THEN 'investing'
                   -- A private fund's distribution is returned capital
                   -- until something proves otherwise — the income
                   -- floor says so — and a rule or a pin promoting it to
                   -- an income type moves it to operating in, as the
                   -- holder's word should. Read off the dimension's own
                   -- primary rather than off the value's spelling.
                   WHEN f.kind = 'distribution' THEN
                        CASE WHEN vc.spend_primary = 'INCOME' THEN 'in'
                             WHEN f.asset_class = 'cash'      THEN 'internal'
                             ELSE 'investing' END
                   -- Trades and private capital. An instrument whose
                   -- class is cash — a money-market fund, a time deposit
                   -- — is cash becoming cash, so the trade is
                   -- pool-internal rather than a movement anyone wants
                   -- drawn.
                   WHEN f.kind IN ('buy', 'sell', 'contribution') THEN
                        CASE WHEN f.asset_class = 'cash' THEN 'internal' ELSE 'investing' END
                   -- The inflow kinds.
                   WHEN f.kind IN ('dividend', 'coupon', 'staking', 'capital_gain',
                                   'reward', 'deposit')
                     OR (f.kind = 'interest' AND f.net_amount > 0) THEN 'in'
                   -- The outflow kinds. `refund` is here with the rest:
                   -- a merchant credit nets inside its own category, and
                   -- the class's sign is what decides which side of the
                   -- diagram it is drawn on.
                   WHEN f.kind IN ('purchase', 'refund', 'fee', 'tax', 'withdrawal')
                     OR (f.kind = 'interest' AND f.net_amount < 0) THEN 'out'
                   -- Everything left: the FX family and corporate
                   -- actions, which gold pins no canonical sign for, so
                   -- a section that reads direction cannot admit them;
                   -- an unpaired card bill or in-kind transfer leg,
                   -- which is in neither family's population so no tier
                   -- could ever have placed it; and the two catch-all
                   -- kinds. Excluded, and COUNTED — a counter nobody
                   -- reads is how a silent hole starts.
                   ELSE 'excluded'
               END AS place
          FROM far f
          LEFT JOIN spend_categories vc ON vc.spend_detailed = f.verdict
    ),
    nodes AS (
        SELECT p.*,
               CASE p.place WHEN 'internal' THEN 'internal'
                            WHEN 'excluded' THEN 'excluded'
                            ELSE 'line' END AS disposition,
               CASE p.place
                   WHEN 'in'                THEN 'operating_in'
                   WHEN 'giving_in'         THEN 'operating_in'
                   WHEN 'out'               THEN 'operating_out'
                   WHEN 'giving_out'        THEN 'operating_out'
                   WHEN 'investing'         THEN 'investing'
                   WHEN 'mortgage'          THEN 'financing'
                   WHEN 'loans'             THEN 'financing'
                   WHEN 'vehicle_far'       THEN 'vehicles'
                   WHEN 'vehicle_delta'     THEN 'vehicles'
                   WHEN 'vehicle_untracked' THEN 'vehicles'
               END AS section,
               CASE p.place
                   WHEN 'in' THEN
                        CASE
                            WHEN p.verdict IS NULL THEN '(uncategorized)'
                            WHEN p.verdict IN ('INCOME_WAGES', 'INCOME_SELF_EMPLOYMENT')
                                 THEN 'earnings'
                            -- Money the household's assets produce
                            -- without its labour: rent and royalties
                            -- belong here beside the dividends.
                            WHEN p.verdict IN ('INCOME_DIVIDENDS', 'INCOME_INTEREST_EARNED',
                                               'INCOME_DISTRIBUTIONS', 'INCOME_STAKING',
                                               'INCOME_RENT', 'INCOME_ROYALTIES')
                                 THEN 'yield'
                            WHEN p.verdict IN ('INCOME_RETIREMENT_PENSION', 'INCOME_GOVERNMENT_BENEFITS',
                                               'INCOME_UNEMPLOYMENT', 'INCOME_ALIMONY_AND_CHILD_SUPPORT')
                                 THEN 'benefits'
                            -- A card credit or a referral bonus sits
                            -- with the receipts rather than the yield:
                            -- it is not income from wealth.
                            ELSE 'other_receipts'
                        END
                   WHEN 'giving_in' THEN 'other_receipts'
                   WHEN 'out' THEN
                        CASE
                            WHEN p.verdict IS NULL THEN '(uncategorized)'
                            WHEN p.verdict_primary = 'BANK_FEES' THEN 'fees'
                            WHEN p.verdict IN ('GOVERNMENT_AND_NON_PROFIT_TAX_PAYMENT',
                                               'GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX')
                                 THEN 'taxes'
                            WHEN p.verdict IN ('GOVERNMENT_AND_NON_PROFIT_DONATIONS', 'gift')
                                 THEN 'giving'
                            ELSE 'consumption'
                        END
                   WHEN 'giving_out' THEN 'giving'
                   -- `elsewhere` is Untracked investments: capital
                   -- the household deployed somewhere the product does
                   -- not hold, which is what the `investment` and
                   -- `capital_return` verdicts place and which names no
                   -- instrument. A row that DOES name one is tracked —
                   -- a missing dimension row or an unset class is a gap
                   -- in the instrument, not an untracked destination —
                   -- so it falls to `other` and is visible as the gap
                   -- it is.
                   WHEN 'investing' THEN
                        CASE WHEN p.asset_class IS NOT NULL THEN p.asset_class
                             WHEN p.instrument_external_id IS NOT NULL THEN 'other'
                             ELSE 'elsewhere' END
                   WHEN 'mortgage' THEN 'mortgage'
                   WHEN 'loans'    THEN 'loans'
                   WHEN 'vehicle_far' THEN p.far_wrapper_class
                   WHEN 'vehicle_delta' THEN
                        CASE p.verdict
                            WHEN 'retirement_transfer' THEN 'retirement'
                            WHEN 'education_transfer'  THEN 'education'
                            WHEN 'health_transfer'     THEN 'health'
                            WHEN 'trust_transfer'      THEN 'trusts'
                            WHEN 'deposit_transfer'    THEN 'deposits'
                        END
                   WHEN 'vehicle_untracked' THEN 'untracked'
               END AS class
          FROM placed p
    ),
    grouped AS (
        SELECT n.*,
               CASE n.place
                   -- The inflow leaves are the income family's own
                   -- values. Income has ONE vendored primary, so a
                   -- primary-level leaf would fold every earned and
                   -- yielded type into `INCOME` and say nothing.
                   WHEN 'in'  THEN COALESCE(n.verdict, '(uncategorized)')
                   -- The outflow leaves are the spending family's
                   -- PRIMARIES, which is where the two families differ:
                   -- that vocabulary has ninety detailed values, and a
                   -- diagram with ninety leaves is not a diagram.
                   --
                   -- Taxes, fees and giving keep the detailed value,
                   -- because those three classes were lifted out of
                   -- spending precisely for the distinction inside them
                   -- — a tax assessed against one withheld at source, a
                   -- fee for banking against a fee for investing, a
                   -- donation against a gift given — and a class whose
                   -- one leaf repeats its own name is a self-edge.
                   --
                   -- A delta is primary-level, so `card_spend`,
                   -- `cash_withdrawal` and `other` are their own leaves
                   -- either way.
                   --
                   -- One spending PRIMARY is a catch-all rather than a
                   -- category, and a detailed value filed under it can
                   -- be a bigger line than most primaries: school fees
                   -- are `GENERAL_SERVICES_EDUCATION`, which folds a
                   -- household's tuition into the same leaf as its
                   -- subscriptions and its dry cleaning. Promoting the
                   -- value gives it an edge of its own beside
                   -- `HOME_IMPROVEMENT`, which is what it is comparable
                   -- to. The test is the vocabulary's, not this
                   -- deployment's: promote a value whose PRIMARY does
                   -- not describe it.
                   WHEN 'out' THEN
                        CASE WHEN n.verdict IS NULL THEN '(uncategorized)'
                             WHEN n.class IN ('taxes', 'fees', 'giving') THEN n.verdict
                             WHEN n.verdict IN ('GENERAL_SERVICES_EDUCATION')
                                  THEN n.verdict
                             ELSE COALESCE(n.verdict_primary, n.verdict) END
                   -- A crossing to or from a giving vehicle has no
                   -- family value behind it: the row's verdict is
                   -- `internal_transfer`, which names a movement rather
                   -- than a kind of giving or a kind of receipt. Two ids
                   -- of cashflow's own, so the leaf reads as vocabulary
                   -- and not as an account.
                   WHEN 'giving_out' THEN 'vehicle_giving'
                   WHEN 'giving_in'  THEN 'vehicle_receipt'
                   -- Investing leaves: what the money DID, since the
                   -- class already says what it was in.
                   WHEN 'investing' THEN
                        CASE WHEN n.kind IN ('buy', 'sell') THEN 'trades'
                             WHEN n.kind IN ('contribution', 'distribution') THEN 'private_capital'
                             WHEN n.class = 'elsewhere' THEN n.verdict
                             ELSE 'private_capital' END
                   WHEN 'loans' THEN n.verdict
                   -- Financing's mortgage, the four pools, the untracked
                   -- accounts: the class is the leaf. Nothing finer
                   -- exists to say, and the diagram draws these
                   -- attached to the hub rather than through a leaf
                   -- stage (docs/CASHFLOW.md §5).
                   ELSE n.class
               END AS grp
          FROM nodes n
    )
    SELECT g.silver_source_id, g.transaction_external_id, g.occurred_at,
           g.account_external_id, g.account_kind, g.display_name,
           g.nickname, g.account_category,
           g.kind, g.currency, g.net_amount, g.description, g.counterparty,
           g.instrument_external_id, g.asset_class,
           g.disposition, g.place, g.section, g.class, g.grp,
           cashflow_class_label(g.class) AS class_label,
           COALESCE(
               CASE g.grp
                   WHEN 'trades'          THEN 'Trades'
                   WHEN 'private_capital' THEN 'Private capital'
                   WHEN 'vehicle_giving'  THEN 'To giving vehicles'
                   WHEN 'vehicle_receipt' THEN 'From giving vehicles'
                   WHEN '(uncategorized)' THEN 'Uncategorised'
               END,
               gc.label, gp.primary_label,
               cashflow_class_label(g.grp)) AS group_label,
           -- The instrument on an investing line, the payer on a
           -- receipt, the merchant on an outflow. A financing, vehicle
           -- or cash line names nothing: its counterparty is an account,
           -- and an account is never something this feature prints as a
           -- name.
           CASE g.section
               WHEN 'investing'     THEN g.instrument_name
               WHEN 'operating_in'  THEN g.payer_name
               WHEN 'operating_out' THEN g.merchant_name
           END AS name,
           g.verdict, g.spend_detailed, g.income_detailed,
           COALESCE(g.income_provenance, g.spend_provenance) AS provenance,
           g.far_silver_source_id, g.far_account_external_id,
           g.far_side, g.far_place, g.far_known
      FROM grouped g
      -- A leaf is a detailed value, a primary, or a word of cashflow's
      -- own, and the dimension keys the first two differently. A delta
      -- matches both, its primary being its detailed value, and the two
      -- labels agree.
      LEFT JOIN spend_categories gc ON gc.spend_detailed = g.grp
      LEFT JOIN (SELECT DISTINCT spend_primary, primary_label FROM spend_categories) gp
             ON gp.spend_primary = g.grp
);

-- A delta is added to the taxonomy and to the two lists that keep it
-- off the family bases TOGETHER, or the vocabulary and the reports
-- disagree — 0094's lesson, learned when `deposit_transfer` was added
-- to one and not the other and both reports counted it. An instalment
-- to a servicer is the household's own money reducing its own
-- liability: not something it bought, not something it earned.
--
-- Carried forward from 0094; the one added line in each is the only
-- change of meaning, and `deposit_transfer`'s line is re-indented to
-- sit with its siblings.

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
       AND c.spend_detailed IS DISTINCT FROM 'mortgage_transfer'
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
       AND c.income_detailed IS DISTINCT FROM 'mortgage_transfer'
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (101, CAST(epoch(now()) AS BIGINT));
