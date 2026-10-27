-- ============================================================
-- gold schema, migration 0162 — a dated wrapper override.
--
-- The household boundary is drawn by tax wrapper, and a wrapper was one
-- column with no validity range: a grantor trust that became
-- non-grantor moved every year to the other side at once. A wrapper
-- override (config account_overrides[].tax_wrapper) may now carry a day,
-- `tax_wrapper_from`. From it the account has the override's wrapper;
-- before it, the wrapper its adapter states.
--
-- account_wrapper_history holds that earlier wrapper, one row per
-- account with a dated override: `tax_wrapper` (NULL where the adapter
-- states none) until `until_at`. The load writes it. accounts.tax_wrapper
-- stays the current wrapper, so every reader of the dimension is
-- unchanged.
--
-- account_wrapper_spans() is each account's wrapper over time: one span
-- from the beginning of time, or two around a dated override's day.
-- cashflow_pool_spans() is those spans the pool admits, by the same two
-- tests cashflow_pool_accounts() applies.
--
-- The two decisions the boundary makes per row read the wrapper as of
-- the row's day: whether the row's own account is in the pool
-- (cashflow_lines_base) and where a move to a far account lands
-- (cashflow_txn_nodes). Both are carried forward whole, from 0144 and
-- 0161, with only those joins changed. An account with no dated
-- override has one span, so they answer as before. The balance-side
-- readers (the reconciliation memo, coverage, the projection) read the
-- current wrapper.
--
-- Replayable: IF NOT EXISTS and OR REPLACE throughout.
-- ============================================================

CREATE TABLE IF NOT EXISTS account_wrapper_history (
    silver_source_id    TEXT   NOT NULL,
    account_external_id TEXT   NOT NULL,
    tax_wrapper         TEXT,
    until_at            BIGINT NOT NULL,
    PRIMARY KEY (silver_source_id, account_external_id)
);

CREATE OR REPLACE MACRO account_wrapper_spans() AS TABLE (
    SELECT a.silver_source_id, a.account_external_id, h.tax_wrapper,
           CAST(-9223372036854775808 AS BIGINT) AS from_at, h.until_at
      FROM accounts a
      JOIN account_wrapper_history h
        ON h.silver_source_id    = a.silver_source_id
       AND h.account_external_id = a.account_external_id
    UNION ALL
    SELECT a.silver_source_id, a.account_external_id, a.tax_wrapper,
           COALESCE(h.until_at, CAST(-9223372036854775808 AS BIGINT)) AS from_at,
           CAST(9223372036854775807 AS BIGINT) AS until_at
      FROM accounts a
      LEFT JOIN account_wrapper_history h
             ON h.silver_source_id    = a.silver_source_id
            AND h.account_external_id = a.account_external_id
);

CREATE OR REPLACE MACRO cashflow_pool_spans() AS TABLE (
    SELECT w.silver_source_id, w.account_external_id, w.from_at, w.until_at
      FROM account_wrapper_spans() w
      LEFT JOIN cashflow_account_scope s
             ON s.silver_source_id    = w.silver_source_id
            AND s.account_external_id = w.account_external_id
      LEFT JOIN cashflow_wrapper_sides ws
             ON ws.tax_wrapper = w.tax_wrapper
     WHERE COALESCE(s.mode, 'include') = 'include'
       AND COALESCE(ws.side, 'household') = 'household'
);

CREATE OR REPLACE MACRO cashflow_txn_nodes(p_from, p_to) AS TABLE (
    WITH joined AS (
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
          -- The far account's wrapper and pool membership as of the
          -- row's day (0162): a dated wrapper override applies from its
          -- day on.
          LEFT JOIN account_wrapper_spans() fws
                 ON fws.silver_source_id    = e.far_silver_source_id
                AND fws.account_external_id = e.far_account_external_id
                AND t.occurred_at >= fws.from_at AND t.occurred_at < fws.until_at
          LEFT JOIN cashflow_wrapper_sides fw ON fw.tax_wrapper = fws.tax_wrapper
          LEFT JOIN cashflow_pool_spans() fp
                 ON fp.silver_source_id    = e.far_silver_source_id
                AND fp.account_external_id = e.far_account_external_id
                AND t.occurred_at >= fp.from_at AND t.occurred_at < fp.until_at
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

CREATE OR REPLACE MACRO cashflow_lines_base(p_from, p_to) AS TABLE (
    -- Every mortgage instalment there has ever been, not merely the
    -- ones the window asks about: an interval's principal is shared out
    -- among the payments INSIDE THE INTERVAL, and which of those the
    -- reader happens to be looking at cannot change the answer.
    WITH pay AS (
        SELECT n.silver_source_id, n.transaction_external_id, n.occurred_at,
               n.far_silver_source_id AS msrc, n.far_account_external_id AS macct,
               -n.net_amount AS paid
          FROM cashflow_txn_nodes(0, 9223372036854775807) n
         WHERE n.class = 'mortgage'
           AND n.net_amount < 0
           AND n.far_silver_source_id    IS NOT NULL
           AND n.far_account_external_id IS NOT NULL
    ),
    bal AS (
        SELECT p.silver_source_id AS msrc, p.account_external_id AS macct,
               p.snapshot_at, SUM(p.market_value) AS mv
          FROM positions p
          JOIN accounts a ON a.silver_source_id    = p.silver_source_id
                         AND a.account_external_id = p.account_external_id
         WHERE a.account_kind = 'mortgage'
         GROUP BY 1, 2, 3
    ),
    obs AS (
        SELECT b.msrc, b.macct,
               b.snapshot_at // 86400 AS from_day,
               COALESCE(LEAD(b.snapshot_at) OVER w // 86400, 99999999) AS to_day,
               b.mv, LEAD(b.mv) OVER w AS next_mv, s.until
          FROM bal b
          JOIN pos_key_spans() s ON s.src = b.msrc AND s.acct = b.macct AND s.snap = b.snapshot_at
        WINDOW w AS (PARTITION BY b.msrc, b.macct ORDER BY b.snapshot_at)
    ),
    -- A mortgage's last observation retires what was left where it
    -- ends, by the ending every reader takes (pos_key_spans): a later
    -- run re-covers its company and its source runs more than one and a
    -- half cadences past it, or the source's clock passes the horizon.
    -- Short of that it stands, and its last interval retires nothing.
    retired AS (
        SELECT msrc, macct, from_day, to_day,
               CASE WHEN next_mv IS NOT NULL THEN next_mv - mv
                    WHEN until IS NOT NULL THEN -mv
                    ELSE 0 END AS retired
          FROM obs
    ),
    ranked AS (
        SELECT p.silver_source_id, p.transaction_external_id, p.paid,
               r.msrc, r.macct, r.from_day, r.retired,
               ROW_NUMBER() OVER (PARTITION BY r.msrc, r.macct, r.from_day
                                  ORDER BY p.paid DESC, p.transaction_external_id) AS rn
          FROM pay p
          JOIN retired r
            ON r.msrc = p.msrc AND r.macct = p.macct
           AND p.occurred_at // 86400 >= r.from_day
           AND p.occurred_at // 86400 <  r.to_day
    ),
    alloc AS (
        SELECT silver_source_id, transaction_external_id,
               CAST(greatest(0, least(paid, retired - COALESCE(
                   SUM(paid) OVER (PARTITION BY msrc, macct, from_day ORDER BY rn
                                   ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),
                   0))) AS DECIMAL(28,4)) AS principal
          FROM ranked
    ),
    line AS (
        SELECT n.*,
               CASE WHEN n.class = 'mortgage' AND n.net_amount < 0
                    THEN COALESCE(a.principal, 0) ELSE 0 END AS principal,
               -- A line splits only when it is a mortgage OUTFLOW that
               -- retired something and left something over.
               (n.class = 'mortgage' AND n.net_amount < 0) AS splits,
               -- And a mortgage line that does NOT split is money
               -- borrowed. Sign alone, so the class has no row left
               -- whose group repeats it: a zero-amount mortgage row
               -- draws no edge either way, and naming it here costs
               -- nothing and keeps the leaf rule structural.
               (n.class = 'mortgage' AND n.net_amount >= 0) AS borrowed
          FROM cashflow_txn_nodes(p_from, p_to) n
          -- In the pool on the row's own day (0162).
          JOIN cashflow_pool_spans() p
                 ON p.silver_source_id    = n.silver_source_id
                AND p.account_external_id = n.account_external_id
                AND n.occurred_at >= p.from_at AND n.occurred_at < p.until_at
          LEFT JOIN alloc a
                 ON a.silver_source_id        = n.silver_source_id
                AND a.transaction_external_id = n.transaction_external_id
         WHERE n.disposition = 'line'
    )
    SELECT l.* EXCLUDE (principal, splits, borrowed) REPLACE (
               CAST(CASE
                        WHEN NOT l.splits                                    THEN l.net_amount
                        -- The residue is under half a cent: one line
                        -- carrying the whole row, so the shares still
                        -- sum to it.
                        WHEN -l.net_amount - l.principal <= 0.005            THEN l.net_amount
                        WHEN sh.share = 'principal'                          THEN -l.principal
                        ELSE l.net_amount + l.principal
                    END AS DECIMAL(28,4)) AS net_amount,
               CASE WHEN l.borrowed            THEN 'mortgage_drawdown'
                    WHEN NOT l.splits          THEN l.grp
                    WHEN sh.share = 'principal' THEN 'mortgage_amortization'
                    ELSE 'mortgage_interest' END AS grp,
               CASE WHEN l.borrowed            THEN 'Mortgage drawdown'
                    WHEN NOT l.splits          THEN l.group_label
                    WHEN sh.share = 'principal' THEN 'Mortgage amortization'
                    ELSE 'Mortgage interest' END AS group_label)
      FROM line l
      JOIN (VALUES ('principal'), ('interest')) AS sh(share)
        ON (NOT l.splits AND sh.share = 'interest')
        OR (l.splits AND sh.share = 'principal' AND l.principal > 0)
        -- The remainder, or the whole row where no principal line
        -- covers it: a sub-cent outflow that retired nothing is still
        -- a line, and dropping it is the one way these shares could
        -- fail to sum to their row.
        OR (l.splits AND sh.share = 'interest'
            AND (-l.net_amount - l.principal > 0.005 OR l.principal <= 0))
     ORDER BY l.occurred_at, l.silver_source_id, l.transaction_external_id,
              CASE WHEN l.splits AND sh.share = 'principal' THEN 0 ELSE 1 END
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (162, CAST(epoch(now()) AS BIGINT));
