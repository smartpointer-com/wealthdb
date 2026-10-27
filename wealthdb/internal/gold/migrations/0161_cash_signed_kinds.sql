-- ============================================================
-- gold schema, migration 0161 — a sign pin for the unsigned kinds.
--
-- Six kinds carry no canonical sign: `fx`, `fx_forward`, `fx_swap`,
-- `corporate_action`, `journal` and `other`. Their direction is a
-- per-source convention, so cash flow excludes them and the coverage
-- report counts them as volume no sign can be read from. For some
-- sources the convention is known. Their adapters pass the statement's
-- or the API's signed cash amount through, and a corporate action's
-- cash leg (cash in lieu, a return of capital, an unissued right's
-- cash) is then as readable as a sale.
--
-- cash_signed_kinds holds those pins: a silver kind and a kind, one of
-- the six. The
-- pins live beside the canonical signs (canonical.CashSignedKinds),
-- and the enrichment pass stamps the table from them at every load. A
-- pin reaches only a row that moves no quantity and carries an amount:
-- a row that moves shares may value them rather than pay for them.
--
-- Two macros are re-issued whole: cashflow_txn_nodes from 0160 and
-- report_cashflow_coverage from 0150. In the first a pinned row is
-- investing, grouped `corporate_actions`, ahead of the exclusion (a
-- `cash`-class instrument's leg is pool-internal, and one that names no
-- instrument is class `other`); in the
-- second it joins the ledger rather than the unsigned volume.
--
-- Replayable: IF NOT EXISTS and OR REPLACE throughout.
-- ============================================================

CREATE TABLE IF NOT EXISTS cash_signed_kinds (
    silver_kind TEXT NOT NULL,
    kind        TEXT NOT NULL,
    PRIMARY KEY (silver_kind, kind)
);

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
               -- The holder's word for what the capital went INTO, one
               -- column per family (0102). Read off the raw overlay for
               -- the reason the far account is: it describes a verdict
               -- rather than belonging to either family's vocabulary,
               -- and no report of theirs reads it. Safe against a
               -- replaced verdict, because the store and the kind floor
               -- that `spend_txn_categories()` coalesces over only ever
               -- fill a NULL — where the overlay wrote a verdict, that
               -- verdict is the resolved one.
               e.stated_asset_class  AS spend_stated_class,
               ie.stated_asset_class AS income_stated_class,
               fw.side  AS far_side,
               fw.class AS far_wrapper_class,
               fa.account_kind AS far_account_kind,
               rb.spend_detailed AS reimbursed_detailed,
               -- A cash leg of a kind gold pins no sign for, where the
               -- row's source states it signed (0161).
               (ck.kind IS NOT NULL
                AND t.kind IN ('fx', 'fx_forward', 'fx_swap', 'corporate_action', 'journal', 'other')
                AND COALESCE(t.quantity, 0) = 0
                AND t.net_amount <> 0) AS cash_signed,
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
          LEFT JOIN income_txn_enrichment ie
                 ON ie.silver_source_id        = t.silver_source_id
                AND ie.transaction_external_id = t.transaction_external_id
          LEFT JOIN silver_sources ss ON ss.silver_source_id = t.silver_source_id
          LEFT JOIN cash_signed_kinds ck
                 ON ck.silver_kind = ss.silver_kind AND ck.kind = t.kind
          -- A repayment linked to the outflow it answers (0160).
          LEFT JOIN spend_reimbursements() rb
                 ON rb.silver_source_id        = t.silver_source_id
                AND rb.transaction_external_id = t.transaction_external_id
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
               -- A linked repayment takes the category of the outflow it
               -- answers: it nets there, as a merchant's refund does.
               CASE WHEN j.reimbursed_detailed IS NOT NULL THEN j.reimbursed_detailed
                    WHEN j.kind IN ('dividend', 'coupon', 'staking', 'capital_gain',
                                    'reward', 'distribution', 'deposit')
                      OR (j.kind = 'interest' AND j.net_amount > 0)
                    THEN COALESCE(j.income_detailed, j.spend_detailed)
                    ELSE COALESCE(j.spend_detailed, j.income_detailed)
               END AS verdict,
               -- The SAME kind test, so the exposure is read from the
               -- family whose verdict won. A fixed spending-then-income
               -- order would be right only by luck: rows carry a verdict
               -- on both overlays, and the column is only ever written
               -- beside a verdict, so a NULL verdict implies a NULL
               -- exposure and the two CASEs select one family by
               -- construction.
               CASE WHEN j.kind IN ('dividend', 'coupon', 'staking', 'capital_gain',
                                    'reward', 'distribution', 'deposit')
                      OR (j.kind = 'interest' AND j.net_amount > 0)
                    THEN COALESCE(j.income_stated_class, j.spend_stated_class)
                    ELSE COALESCE(j.spend_stated_class, j.income_stated_class)
               END AS stated_asset_class
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
    -- The last three arms are where a move lands that no wrapper and no
    -- pool membership could place. The mortgage rule's own word first.
    -- Then a far account gold HOLDS, on the household's side, that the
    -- `cashflow.accounts` scope fenced out of the pool — a setting, and
    -- named as one. Last, no far account at all: a rule or a pin placed
    -- the verdict and nothing paired it, which under the closed world
    -- the statement assumes is a finding — a counter-leg gold should
    -- hold, or a declaration not yet written. Drawn as invisible that
    -- money would vanish into the residual forever; drawn as a node it
    -- is visible as what it is, and the remedy — collect the account,
    -- declare it, or find the missing leg — is obvious.
    far AS (
        SELECT v.*,
               CASE
                   WHEN v.far_side = 'vehicle'                     THEN 'vehicle'
                   WHEN v.far_side = 'giving' AND v.net_amount < 0  THEN 'giving_out'
                   WHEN v.far_side = 'giving'                       THEN 'giving_in'
                   WHEN v.far_account_kind = 'mortgage'             THEN 'mortgage'
                   WHEN v.far_pooled                                THEN 'internal'
                   WHEN v.far_class = 'mortgage'                    THEN 'mortgage'
                   WHEN v.far_known                                 THEN 'out_of_pool'
                   ELSE 'unnamed'
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
                   --
                   -- The one exception is a cash leg its source states
                   -- signed (cash_signed_kinds, 0161): cash in lieu, a
                   -- return of capital, an unissued right's cash. It is
                   -- investing, as the trade it settles is.
                   WHEN f.cash_signed THEN
                        CASE WHEN f.asset_class = 'cash' THEN 'internal' ELSE 'investing' END
                   WHEN f.kind IN ('fx', 'fx_forward', 'fx_swap',
                                   'corporate_action', 'journal', 'other')
                        THEN 'excluded'
                   -- A repayment linked to its outflow is that outflow's
                   -- money coming back: operating out, netted inside the
                   -- outflow's class (0160).
                   WHEN f.reimbursed_detailed IS NOT NULL THEN 'out'
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
                            ELSE 'vehicle_unpaired'
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
                   -- actions (but the cash legs a sign pin admits, above),
                   -- which gold pins no canonical sign for, so
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
                   WHEN 'vehicle_unpaired'  THEN 'vehicles'
               END AS section,
               CASE p.place
                   WHEN 'in' THEN
                        CASE
                            WHEN p.verdict IS NULL THEN '(uncategorized)'
                            -- Money from labour, whoever pays for it:
                            -- an employer, a platform or a client.
                            WHEN p.verdict IN ('INCOME_SALARY', 'INCOME_GIG_ECONOMY',
                                               'INCOME_CONTRACTOR')
                                 THEN 'earnings'
                            -- Money the household's assets produce
                            -- without its labour: rent and royalties
                            -- belong here beside the dividends.
                            WHEN p.verdict IN ('INCOME_DIVIDENDS', 'INCOME_INTEREST_EARNED',
                                               'INCOME_DISTRIBUTIONS', 'INCOME_STAKING',
                                               'INCOME_RENTAL', 'INCOME_ROYALTIES',
                                               'INCOME_ENERGY_FEED_IN')
                                 THEN 'yield'
                            -- Entitlements: a pension, a benefit, and
                            -- maintenance from a former partner or
                            -- for a child.
                            WHEN p.verdict IN ('INCOME_RETIREMENT_PENSION', 'INCOME_GOVERNMENT_BENEFITS',
                                               'INCOME_UNEMPLOYMENT', 'INCOME_LONG_TERM_DISABILITY',
                                               'INCOME_MILITARY', 'INCOME_CHILD_SUPPORT',
                                               'INCOME_ALIMONY')
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
                   --
                   -- The holder's word is read HERE and not in `joined`,
                   -- and it comes last on purpose. `instruments.asset_class`
                   -- is NOT NULL, so a null on the joined row means the
                   -- dimension ROW is absent — a term in `joined` would
                   -- jump ahead of the `other` gap marker above and hide
                   -- a real dimension hole behind a correct-looking
                   -- class. Reading it here is the only placement that
                   -- makes "the feed first, the holder last" true, and
                   -- it keeps the exported `asset_class` meaning what
                   -- 0097 says it means: what the FEED said.
                   --
                   -- The `cash` guard makes the config refusal structural
                   -- rather than a promise kept only at parse time:
                   -- `cash` is labelled "Cash savings", which is the
                   -- Sankey's own residual node, and node names must be
                   -- unique across the edge list.
                   WHEN 'investing' THEN
                        CASE WHEN p.asset_class IS NOT NULL THEN p.asset_class
                             -- A pinned cash leg that names no instrument
                             -- settles a holding rather than deploying
                             -- capital somewhere untracked: `other`.
                             WHEN p.cash_signed THEN 'other'
                             WHEN p.instrument_external_id IS NOT NULL THEN 'other'
                             WHEN p.stated_asset_class IS NOT NULL
                              AND p.stated_asset_class <> 'cash'
                                  THEN p.stated_asset_class
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
                   WHEN 'vehicle_unpaired'  THEN 'unpaired'
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
                   -- that vocabulary has close to a hundred detailed
                   -- values, and a diagram with that many leaves is not
                   -- a diagram.
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
                   --
                   -- The verdict outranks the kind, which is the rule the
                   -- `place` ladder already follows. Until 0102 the
                   -- verdict reached the group only through
                   -- `WHEN n.class = 'elsewhere'`, so the moment a stated
                   -- exposure moved a row off `elsewhere` its group fell
                   -- to the ELSE and a house purchase read
                   -- `Real estate · Private capital`. That arm is dropped
                   -- rather than carried: no row reaches `investing` on a
                   -- kind other than buy/sell/contribution/distribution
                   -- except through those two verdicts, so the first arm
                   -- subsumes it.
                   --
                   -- The class is what the money went INTO; the group is
                   -- what the money DID. A house bought and a REIT traded
                   -- share a class node and differ at group grain.
                   WHEN 'investing' THEN
                        CASE WHEN n.verdict IN ('investment', 'capital_return')
                                  THEN n.verdict
                             WHEN n.kind IN ('buy', 'sell') THEN 'trades'
                             WHEN n.kind = 'corporate_action' THEN 'corporate_actions'
                             WHEN n.kind IN ('contribution', 'distribution') THEN 'private_capital'
                             ELSE 'private_capital' END
                   WHEN 'loans' THEN n.verdict
                   -- The unpaired transfers split by what is missing:
                   -- no far account at all, or a far account the pool
                   -- fenced out. The first is a finding to work down,
                   -- the second a setting, and a reader should not have
                   -- to open the config to tell them apart.
                   WHEN 'vehicle_unpaired' THEN n.far_place
                   -- Financing's mortgage and the five pools: the class
                   -- is the leaf. Nothing finer exists to say, and the
                   -- diagram draws these attached to the hub rather than
                   -- through a leaf stage (docs/CASHFLOW.md §5).
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
                   WHEN 'corporate_actions' THEN 'Corporate actions'
                   WHEN 'private_capital' THEN 'Private capital'
                   WHEN 'vehicle_giving'  THEN 'To giving vehicles'
                   WHEN 'vehicle_receipt' THEN 'From giving vehicles'
                   WHEN 'unnamed'         THEN 'No far account'
                   WHEN 'out_of_pool'     THEN 'Fenced-out account'
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

CREATE OR REPLACE MACRO report_cashflow_coverage(p_from, p_to, p_period) AS TABLE (
    WITH pool AS (
        SELECT silver_source_id, account_external_id, account_kind, nickname,
               COALESCE(nickname, display_name, account_external_id) AS account
          FROM cashflow_pool_accounts()
         WHERE account_kind IS DISTINCT FROM 'overlay'),
    chosen AS (
        SELECT silver_source_id, account_external_id, currency, snapshot_at, amount
          FROM (SELECT cb.silver_source_id, cb.account_external_id, cb.currency,
                       cb.snapshot_at, cb.amount,
                       ROW_NUMBER() OVER (
                           PARTITION BY cb.silver_source_id, cb.snapshot_at,
                                        cb.account_external_id, cb.currency
                           ORDER BY CASE cb.balance_kind
                               WHEN 'current' THEN 1 WHEN 'closing' THEN 2
                               WHEN 'available' THEN 3 WHEN 'aggregated' THEN 4
                               WHEN 'opening' THEN 5 WHEN 'initial' THEN 6
                               WHEN 'projected' THEN 7 ELSE 99 END, cb.balance_kind) AS rn
                  FROM cash_balances cb
                  JOIN pool p ON p.silver_source_id    = cb.silver_source_id
                             AND p.account_external_id = cb.account_external_id)
         WHERE rn = 1),
    buckets AS (
        SELECT DISTINCT spend_period_bucket(p_period, t.occurred_at) AS period_start
          FROM transactions t JOIN pool p ON p.silver_source_id = t.silver_source_id
                                         AND p.account_external_id = t.account_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to),
    bday AS (
        SELECT period_start,
               (GREATEST(COALESCE(period_start, p_from), p_from) - 1) // 86400 AS open_day,
               cashflow_period_end(p_period, period_start, p_to) // 86400 AS close_day
          FROM buckets),
    series AS (
        SELECT DISTINCT silver_source_id, account_external_id, currency FROM chosen
        UNION
        SELECT DISTINCT t.silver_source_id, t.account_external_id, t.currency
          FROM transactions t JOIN pool p ON p.silver_source_id = t.silver_source_id
                                         AND p.account_external_id = t.account_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to AND t.net_amount IS NOT NULL),
    -- A row moves no cash when its kind pins no sign, or when it is an
    -- in-kind transfer: securities delivered in or out, valued but paid
    -- for by nobody. A cash leg its source states signed
    -- (cash_signed_kinds, 0161) moves cash and is in the ledger.
    unsigned AS (
        SELECT t.*,
               (t.kind IN ('fx','fx_forward','fx_swap','corporate_action','journal','other')
                AND NOT (ck.kind IS NOT NULL AND COALESCE(t.quantity, 0) = 0
                         AND t.net_amount <> 0))
               OR (t.kind IN ('transfer_in','transfer_out')
                   AND t.instrument_external_id IS NOT NULL) AS no_cash
          FROM transactions t JOIN pool p ON p.silver_source_id = t.silver_source_id
                                         AND p.account_external_id = t.account_external_id
          LEFT JOIN silver_sources ss ON ss.silver_source_id = t.silver_source_id
          LEFT JOIN cash_signed_kinds ck
                 ON ck.silver_kind = ss.silver_kind AND ck.kind = t.kind
         WHERE t.occurred_at BETWEEN p_from AND p_to AND t.net_amount IS NOT NULL),
    led AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               silver_source_id, account_external_id, currency,
               SUM(CASE WHEN no_cash THEN 0 ELSE net_amount END) AS ledger,
               SUM(CASE WHEN no_cash THEN abs(net_amount) ELSE 0 END) AS unsigned_volume,
               COUNT(*) AS txn_count
          FROM unsigned
         GROUP BY 1,2,3,4),
    -- Where each balance series stops. A series that stopped before a
    -- period carries its last balance across it, which measures nothing.
    last_seen AS (
        SELECT silver_source_id, account_external_id, currency,
               MAX(snapshot_at) AS last_at
          FROM chosen
         GROUP BY 1,2,3),
    grid AS (SELECT b.period_start, b.open_day, b.close_day, s.*
               FROM bday b CROSS JOIN series s),
    bal AS (
        SELECT g.period_start, g.silver_source_id, g.account_external_id, g.currency,
               o.amount AS open_amt, c.amount AS close_amt,
               ls.last_at <= g.open_day * 86400 + 86399 AS ended
          FROM grid g
          LEFT JOIN last_seen ls
                 ON ls.silver_source_id    = g.silver_source_id
                AND ls.account_external_id = g.account_external_id
                AND ls.currency            = g.currency
          ASOF LEFT JOIN chosen o
                ON o.silver_source_id    = g.silver_source_id
               AND o.account_external_id = g.account_external_id
               AND o.currency            = g.currency
               AND o.snapshot_at        <= g.open_day * 86400 + 86399
          ASOF LEFT JOIN chosen c
                ON c.silver_source_id    = g.silver_source_id
               AND c.account_external_id = g.account_external_id
               AND c.currency            = g.currency
               AND c.snapshot_at        <= g.close_day * 86400 + 86399)
    SELECT b.period_start, p.silver_source_id, p.account, p.nickname, p.account_kind, b.currency,
           COALESCE(l.txn_count, 0) AS txn_count,
           CAST(CAST(l.ledger          AS DECIMAL(28,4)) AS VARCHAR) AS ledger,
           CAST(CAST(l.unsigned_volume AS DECIMAL(28,4)) AS VARCHAR) AS unsigned_volume,
           CAST(CAST(CASE WHEN b.open_amt IS NOT NULL AND b.close_amt IS NOT NULL
                               AND NOT b.ended
                THEN b.close_amt - b.open_amt END AS DECIMAL(28,4)) AS VARCHAR) AS measured,
           CAST(CAST(CASE WHEN b.open_amt IS NOT NULL AND b.close_amt IS NOT NULL
                               AND NOT b.ended
                THEN COALESCE(l.ledger, 0) - (b.close_amt - b.open_amt)
                END AS DECIMAL(28,4)) AS VARCHAR) AS gap,
           CASE WHEN b.close_amt IS NULL THEN 'unmeasurable'
                WHEN b.ended            THEN 'ended'
                WHEN b.open_amt  IS NULL THEN 'opening'
                WHEN COALESCE(l.unsigned_volume, 0) > 0
                     AND abs(COALESCE(l.ledger, 0) - (b.close_amt - b.open_amt))
                         <= l.unsigned_volume THEN 'obscured'
                ELSE 'measured' END AS status
      FROM bal b
      JOIN pool p ON p.silver_source_id    = b.silver_source_id
                 AND p.account_external_id = b.account_external_id
      LEFT JOIN led l ON l.period_start IS NOT DISTINCT FROM b.period_start
                     AND l.silver_source_id    = b.silver_source_id
                     AND l.account_external_id = b.account_external_id
                     AND l.currency            = b.currency
     WHERE COALESCE(l.txn_count, 0) > 0 OR b.close_amt IS NOT NULL
     ORDER BY b.period_start, abs(COALESCE(COALESCE(l.ledger,0) - (b.close_amt - b.open_amt), 0)) DESC,
              b.silver_source_id, p.account, b.account_external_id, b.currency
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (161, CAST(epoch(now()) AS BIGINT));
