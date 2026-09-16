-- THE RESOLUTION: which node of the cash flow statement each line is.
--
-- This is the whole of the feature's arithmetic. Everything above it —
-- the four reports, their multi-currency siblings, the serving view the
-- dashboard reads — is an aggregate over these two macros, in the shape
-- the spending and income report families already have.
--
-- It is SQL rather than Go for one reason: the dashboard needs the same
-- node assignment the CLI uses, computed AFTER its pickers apply. A
-- Go-side resolution would have to be re-implemented in the serving
-- view, and the two would drift. The returns engine is in Go because
-- its maths cannot be expressed in SQL; nothing here is like that.
--
-- ONE INVERSION worth naming up front: cashflow reads each family's
-- RESOLUTION macro — spend_txn_categories(), income_txn_categories() —
-- and never its `*_lines_base`. Four of the verdicts it wants are
-- exactly the ones a base excludes: `internal_transfer`,
-- `capital_return`, `loan_proceeds` and the four crossings all leave
-- their family's base and all of them are cashflow lines.
--
--   cashflow_txn_nodes(from, to)   every transaction in the window with
--                                  its node resolved, and a disposition
--                                  saying whether it is a line at all.
--   cashflow_lines_base(from, to)  those nodes, on the pool's accounts,
--                                  that are lines.
--
-- The layering is the families': the wider macro is what a canary
-- counts and what a test walks, and the narrower one is what a report
-- charts. `cashflow_txn_nodes` deliberately does NOT join the pool,
-- because the pool is the question a line answers rather than a filter
-- on the resolution.
--
-- IF NOT EXISTS / OR REPLACE throughout keep this replayable for the
-- DDL-rerun test.

-- ============================================================
-- cashflow_class_label: a class's display name.
--
-- Two vocabularies in one function, because a class IS two things: the
-- words cashflow invents for the statement's own structure, and — on
-- the investing section — the instrument's asset class, whose values
-- gold stores as bare enum spellings and labels nowhere else. Both are
-- read off the value, so a report and the serving view cannot disagree
-- about what a node is called.
--
-- A value the table does not know is returned unchanged rather than
-- guessed at, which is the rule canonical.SpendLabel follows for the
-- taxonomy: a new asset class shows up as its own spelling rather than
-- as a blank or as somebody else's name.
-- ============================================================
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
        WHEN 'consumption'    THEN 'Spending'
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
        WHEN 'retirement' THEN 'Retirement'
        WHEN 'education'  THEN 'Education'
        WHEN 'health'     THEN 'Health'
        WHEN 'trusts'     THEN 'Trusts'
        WHEN 'untracked'  THEN 'Untracked accounts'
        WHEN 'cash'       THEN 'Cash'
        ELSE p_class
    END
);

-- ============================================================
-- cashflow_txn_nodes: the resolution, one row per transaction.
--
-- THE SHAPE. A single ordered ladder decides `place`, a discriminant
-- naming what KIND of node the row is; three keyed lookups then read
-- the section, the class and the group off it. The split is deliberate:
-- the ordered part is written once, so a branch cannot be reordered in
-- one copy and not another, and the parts that follow are keyed by
-- value rather than by position and cannot drift at all.
--
-- THE SIGN. gold's canonical signs already mean "positive is cash into
-- the pool" for every kind admitted here, the card kinds included: a
-- card's balance is carried as negative cash, so a purchase on it drives
-- the pool down and a refund back up, exactly as they do on a deposit
-- account. Nothing below negates anything.
--
-- WHICH FAMILY'S VERDICT. A row can carry one in each overlay — a
-- deposit the matcher paired is `internal_transfer` in both — so the
-- verdict is read direction-first: the income overlay answers for the
-- kinds income books, the spending overlay for the rest, and each falls
-- through to the other. The fall-through is what covers an account one
-- family's scope excludes: cashflow's pool inherits neither scope, so a
-- row can have a verdict on one side only.
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
                                      'health_transfer', 'trust_transfer')
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
                   WHEN 'investing' THEN
                        CASE WHEN p.asset_class IS NULL OR p.asset_class = 'cash'
                             THEN 'elsewhere' ELSE p.asset_class END
                   WHEN 'mortgage' THEN 'mortgage'
                   WHEN 'loans'    THEN 'loans'
                   WHEN 'vehicle_far' THEN p.far_wrapper_class
                   WHEN 'vehicle_delta' THEN
                        CASE p.verdict
                            WHEN 'retirement_transfer' THEN 'retirement'
                            WHEN 'education_transfer'  THEN 'education'
                            WHEN 'health_transfer'     THEN 'health'
                            WHEN 'trust_transfer'      THEN 'trusts'
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
                   WHEN 'out' THEN
                        CASE WHEN n.verdict IS NULL THEN '(uncategorized)'
                             WHEN n.class IN ('taxes', 'fees', 'giving') THEN n.verdict
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

-- ============================================================
-- cashflow_lines_base: what a view charts.
--
-- The nodes, restricted to the household's cash pool and to the rows
-- that are lines at all. Both restrictions are the whole of what this
-- macro adds, and both are stated here rather than inside the
-- resolution so that a canary can count what the resolution declined.
--
-- The pool join is on the NEAR account: a vehicle's own dividends,
-- trades and fees are not the household's cash flow, which is what
-- makes the totals here smaller than the two families' reports by more
-- than the re-homed verdicts alone.
-- ============================================================
CREATE OR REPLACE MACRO cashflow_lines_base(p_from, p_to) AS TABLE (
    SELECT n.*
      FROM cashflow_txn_nodes(p_from, p_to) n
      JOIN cashflow_pool_accounts() p
             ON p.silver_source_id    = n.silver_source_id
            AND p.account_external_id = n.account_external_id
     WHERE n.disposition = 'line'
     ORDER BY n.occurred_at, n.silver_source_id, n.transaction_external_id
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (81, CAST(epoch(now()) AS BIGINT));
