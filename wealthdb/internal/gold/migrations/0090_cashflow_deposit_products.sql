-- ============================================================
-- gold schema, migration 0090 —
--   a bank's own deposit product is not an untracked account.
--
-- A call deposit, a fixed-term deposit and a notice account are the
-- household's own cash, parked at the bank that already holds the
-- account funding them. The bank books every movement of one under the
-- FUNDING account and never lists the product beside it, so the
-- collector has no account to collect and the move reaches the
-- resolution with one leg.
--
-- One-legged, it fell to the last arm of the far-account test and drew
-- as `Untracked accounts` — the node for a destination nothing
-- identifies. Here the narrative identifies it outright, on every row,
-- in both of the bank's eras. Lumping it in with the genuinely
-- anonymous is what stops that node meaning anything: a reader cannot
-- tell the money whose destination is unknown from the money whose
-- destination is stated and merely uncollected.
--
-- It is placed as a fifth `*_transfer` delta rather than by a far
-- account, for the reason the other four are: there IS no far account
-- to write, and a verdict can be placed on either leg. A far account
-- lives on the spending overlay, which holds the outflow leg and not
-- the return — so that road would have named the money going in and
-- left the money coming back anonymous, which is worse than naming
-- neither.
--
-- What deliberately does NOT change:
--
--   * The move stays a LINE. The money left the measured pool, so the
--     statement has to say so; drawing it as internal netting would
--     make the statement stop tying to the balances it is drawn from.
--     `deposits` is `untracked` made specific, not made invisible.
--   * Interest the product pays is untouched. It is income, it arrives
--     on its own row, and it is the one part of a deposit that is not
--     a transfer.
--
-- 0081 issued these macros, 0085 and 0088 re-issued `cashflow_txn_nodes`
-- and 0087 re-issued `cashflow_class_label`; all are applied, so both
-- are carried forward whole rather than edited. The arms named above
-- are the only changes — every other line is byte-identical.
-- ============================================================

CREATE OR REPLACE MACRO cashflow_txn_nodes(p_from, p_to) AS TABLE (
    WITH pool AS (
        SELECT silver_source_id, account_external_id FROM cashflow_pool_accounts()
    ),
    joined AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
               t.account_external_id, t.kind, t.currency, t.net_amount,
               t.description, t.counterparty, t.instrument_external_id,
               a.account_kind, a.display_name, a.nickname, a.account_category,
               i.asset_class,
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
                        CASE WHEN p.instrument_external_id IS NULL THEN 'elsewhere'
                             WHEN p.asset_class IS NULL OR p.asset_class = 'cash'
                                  THEN 'other'
                             ELSE p.asset_class END
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
        WHEN 'deposits'   THEN 'Bank deposits'
        WHEN 'untracked'  THEN 'Untracked accounts'
        WHEN 'cash'       THEN 'Cash'
        ELSE p_class
    END
);

-- cashflow_class_rank, carried forward from 0081. `deposits` ranks
-- ahead of `untracked` inside the vehicles section: a named
-- destination reads before an anonymous one, and `untracked` stays
-- last there because it is the section's residual.
CREATE OR REPLACE MACRO cashflow_class_rank(p_class) AS (
    CASE
        WHEN p_class = 'earnings'        THEN 1
        WHEN p_class = 'yield'           THEN 2
        WHEN p_class = 'benefits'        THEN 3
        WHEN p_class = 'other_receipts'  THEN 4
        WHEN p_class = 'consumption'     THEN 1
        WHEN p_class = 'fees'            THEN 2
        WHEN p_class = 'taxes'           THEN 3
        WHEN p_class = 'giving'          THEN 4
        WHEN p_class = '(uncategorized)' THEN 90
        WHEN p_class = 'investments'     THEN 1
        WHEN p_class = 'elsewhere'       THEN 80
        WHEN p_class = 'mortgage'        THEN 1
        WHEN p_class = 'loans'           THEN 2
        WHEN p_class = 'retirement'      THEN 1
        WHEN p_class = 'education'       THEN 2
        WHEN p_class = 'health'          THEN 3
        WHEN p_class = 'trusts'          THEN 4
        WHEN p_class = 'deposits'        THEN 5
        WHEN p_class = 'untracked'       THEN 6
        WHEN p_class = 'cash'            THEN 1
        ELSE 40
    END
);

-- The delta itself, in the dimension the resolution joins. 0078
-- seeded the other five the same way; the INSERT is OR REPLACE and the
-- catch_all recompute is a function of the row's own spelling, so this
-- stays replayable for the DDL-rerun test.
--
-- TestSpendCategoriesMatchGoTable pins this row to canonical's
-- deltaCategories, which is what makes the two copies one vocabulary.
INSERT OR REPLACE INTO spend_categories
    (spend_primary, spend_detailed, description, label, primary_label, family)
VALUES
    ('deposit_transfer', 'deposit_transfer',
     'The same crossing for a bank''s own deposit product the collector does not list as an account — a call deposit, a fixed-term deposit, a notice account: money paid in, or the principal coming back. Earmarked for nothing and taxed like the funding account; it is here because the far leg does not exist in the product, not because the money went anywhere. Interest the product pays is NOT this — it is income, and it arrives on its own row',
     'Deposit transfer', 'Deposit transfer', 'both');

UPDATE spend_categories
   SET catch_all = (spend_primary <> spend_detailed
                    AND starts_with(substr(spend_detailed, length(spend_primary) + 2), 'OTHER_'))
 WHERE catch_all IS NULL;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (90, CAST(epoch(now()) AS BIGINT));
