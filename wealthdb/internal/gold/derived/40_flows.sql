-- Flows: the spending, income and cash flow statements, from their
-- populations through their categories to their reports.

-- ============================================================
-- The roles of the kinds and the values, as predicates.
--
-- Each is a scalar macro, so it expands in place to the test it holds
-- and a caller's plan is the one the test would give. Go states
-- every role once (internal/canonical: the kind tables and
-- CategoryRoles), the derived set stamps it into txn_kind_roles and
-- spend_categories, and a test holds each predicate here to it
-- (TestFlowPredicatesHoldTheRoles). A macro that reads a role on a
-- join it already has reads the stamped column instead.
-- ============================================================

-- kind_is_spending: a row of the kind is in the spending population
-- (txn_kind_roles.spending_side). Interest is charged when negative.
CREATE MACRO kind_is_spending(p_kind, p_amount) AS (
    p_kind IN ('purchase', 'refund', 'withdrawal', 'fee', 'tax')
    OR (p_kind = 'interest' AND p_amount < 0)
);

-- kind_is_income: a row of the kind is in the income population
-- (txn_kind_roles.income_side). Interest is earned when positive.
CREATE MACRO kind_is_income(p_kind, p_amount) AS (
    p_kind IN ('dividend', 'coupon', 'staking', 'capital_gain',
               'reward', 'distribution', 'deposit')
    OR (p_kind = 'interest' AND p_amount > 0)
);

-- kind_in_matcher_pool: the internal-transfer matcher may pair a row of
-- the kind (txn_kind_roles.matcher_pool).
CREATE MACRO kind_in_matcher_pool(p_kind) AS (
    p_kind IN ('deposit', 'withdrawal', 'card_payment', 'transfer_in', 'transfer_out')
);

-- kind_is_unsigned: gold pins the kind no sign, and cash flow excludes
-- it for want of one (txn_kind_roles.unsigned).
CREATE MACRO kind_is_unsigned(p_kind) AS (
    p_kind IN ('fx', 'fx_forward', 'fx_swap', 'corporate_action', 'journal', 'other')
);

-- kind_is_trade: a trade, or private capital committed or returned by
-- a sale (txn_kind_roles.trade).
CREATE MACRO kind_is_trade(p_kind) AS (
    p_kind IN ('buy', 'sell', 'contribution')
);

-- income_kind_floor: the income type a row of the kind reads where no
-- tier placed it (txn_kind_roles.income_floor), on its income side.
CREATE MACRO income_kind_floor(p_kind, p_amount) AS (
    CASE p_kind
        WHEN 'dividend'     THEN 'INCOME_DIVIDENDS'
        WHEN 'coupon'       THEN 'INCOME_INTEREST_EARNED'
        WHEN 'staking'      THEN 'INCOME_STAKING'
        WHEN 'capital_gain' THEN 'INCOME_DISTRIBUTIONS'
        WHEN 'reward'       THEN 'INCOME_REWARDS'
        WHEN 'distribution' THEN 'capital_return'
        WHEN 'interest'     THEN
            CASE WHEN p_amount > 0
                 THEN 'INCOME_INTEREST_EARNED' END
    END
);

-- spend_kind_floor: the same for spending (txn_kind_roles.spend_floor).
-- A fee's floor is the one that reads the account: a card's, and a
-- `cash` account's in no portfolio, is a bank fee; every other is the
-- cost of holding investments (spend_txn_categories says why).
CREATE MACRO spend_kind_floor(p_kind, p_amount, p_account_kind, p_portfolio) AS (
    CASE p_kind
        WHEN 'fee' THEN
            CASE WHEN p_account_kind = 'card'
                      OR (p_account_kind = 'cash'
                          AND p_portfolio IS NULL)
                 THEN 'BANK_FEES_OTHER_BANK_FEES'
                 ELSE 'BANK_FEES_INVESTMENT_FEES' END
        WHEN 'tax' THEN 'GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX'
        WHEN 'interest' THEN
            CASE WHEN p_amount < 0
                 THEN 'BANK_FEES_INTEREST_CHARGE' END
    END
);

-- detailed_stays_in_spending: a line of the value stays in the
-- spending base (NOT spend_categories.leaves_spending). The values that
-- leave are an own-account move, capital deployed, a liability reduced
-- and the crossings. A line with no value stays. One test per value
-- rather than an IN-list, which the planner turns into a join.
CREATE MACRO detailed_stays_in_spending(p_detailed) AS (
    p_detailed IS DISTINCT FROM 'internal_transfer'
    AND p_detailed IS DISTINCT FROM 'investment'
    AND p_detailed IS DISTINCT FROM 'debt_repayment'
    AND p_detailed IS DISTINCT FROM 'retirement_transfer'
    AND p_detailed IS DISTINCT FROM 'education_transfer'
    AND p_detailed IS DISTINCT FROM 'health_transfer'
    AND p_detailed IS DISTINCT FROM 'trust_transfer'
    AND p_detailed IS DISTINCT FROM 'deposit_transfer'
    AND p_detailed IS DISTINCT FROM 'mortgage_transfer'
);

-- detailed_stays_in_income: the same for the income base (NOT
-- spend_categories.leaves_income). The values that leave name money
-- that arrived without being earned.
CREATE MACRO detailed_stays_in_income(p_detailed) AS (
    p_detailed IS DISTINCT FROM 'internal_transfer'
    AND p_detailed IS DISTINCT FROM 'capital_return'
    AND p_detailed IS DISTINCT FROM 'loan_proceeds'
    AND p_detailed IS DISTINCT FROM 'reimbursement'
    AND p_detailed IS DISTINCT FROM 'retirement_transfer'
    AND p_detailed IS DISTINCT FROM 'education_transfer'
    AND p_detailed IS DISTINCT FROM 'health_transfer'
    AND p_detailed IS DISTINCT FROM 'trust_transfer'
    AND p_detailed IS DISTINCT FROM 'deposit_transfer'
    AND p_detailed IS DISTINCT FROM 'mortgage_transfer'
);

-- ============================================================
-- Period bucketing and the valued line bases
-- ============================================================

-- spend_period_bucket: the UTC-midnight epoch second that opens the
-- bucket `p_at` falls in, or NULL for the single `total` bucket.
-- `total` is no date_trunc part, and DuckDB binds the call even on the
-- branch a CASE discards, so a bare `CASE WHEN p_period = 'total' THEN
-- NULL ELSE date_trunc(p_period, …) END` raises. The macro therefore
-- passes a valid part INSIDE the call and nulls the result outside it.
-- The timestamp comes from epoch_ms, not to_timestamp, which yields a
-- TIMESTAMP WITH TIME ZONE and would truncate in the session's zone
-- rather than UTC. Any other part is passed through to date_trunc, so
-- an unrecognised one raises rather than bucketing wrongly; validating
-- the choice belongs to the caller.
CREATE MACRO spend_period_bucket(p_period, p_at) AS (
    CASE WHEN p_period = 'total' THEN NULL
         ELSE CAST(epoch(date_trunc(
                  CASE WHEN p_period = 'total' THEN 'month' ELSE p_period END,
                  epoch_ms(p_at * 1000))) AS BIGINT)
    END
);

-- spend_matcher_pool: the legs the internal-transfer matcher may pair,
-- over EVERY account in gold, not just the accounts spending is scoped
-- to.
--
-- The internal-transfer matcher exists to recognise that money leaving
-- one tracked account arrived in another. It can only pair legs it can
-- SEE. A pool drawn from the spending accounts alone would hold one leg
-- of a movement into an investment account and not the other:
-- unpairable by construction. An unpaired outgoing leg is
-- indistinguishable from spending, so cash moved from a deposit account
-- into an investment account would count as spend, at whatever size the
-- movement was — an unbounded error, not a marginal one.
--
-- What the pool is, and what it deliberately is not:
--
--   * The pool is `transactions` restricted to the window and the
--     transfer-eligible kinds, with NO account join at all. An
--     account's kind says what the account is FOR; it says nothing
--     about whether a credit landing on it is the other half of a
--     debit elsewhere.
--   * spend_account_scope is not consulted here either. An account
--     fenced out of spending is still an account the product tracks,
--     so its legs are still legitimate counter-legs — and marking one
--     of its own rows `internal_transfer` cannot move a report,
--     because every spending surface reads the enrichment population,
--     which the scope still gates.
--   * spend_scoped_accounts(), spend_enrichment_population() and
--     spending_lines_base() keep their own scope. The pool adds
--     candidates for PAIRING; it adds nothing to what a report charts.
--   * The eligible kinds are the matcher_pool role (txn_kind_roles,
--     kind_in_matcher_pool): the kinds that mean "cash moved into or
--     out of an account" AND carry a pinned canonical sign
--     (internal/canonical/sign.go), so every leg can be oriented as
--     debit or credit. `journal` and `other` are catch-alls whose sign
--     is whatever the source supplied and whose meaning is "a
--     bookkeeping entry"; `fx*` and `corporate_action` are unsigned for
--     the same reason; `contribution` and `distribution` are booked
--     from the funding account's own perspective, so neither can ever
--     be the RECEIVING half of a movement out of it. Admitting any of them buys false
--     pairs, and a false pair does not surface as a wrong category —
--     it silently deletes a real spending line.
--
-- One consequence outside spending: report_transactions LEFT JOINs
-- spend_txn_categories() onto every transaction, so a paired leg on an
-- out-of-scope account reads `internal_transfer` there instead of
-- NULL. That is the label the pass has evidence for.
CREATE MACRO spend_matcher_pool(p_from, p_to) AS TABLE (
    SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
           t.account_external_id, t.kind, t.currency, t.net_amount,
           t.description, t.counterparty, t.provider_category
      FROM transactions t
     WHERE t.occurred_at BETWEEN p_from AND p_to
       AND kind_in_matcher_pool(t.kind)
);

-- txn_evidence: the evidence a source states in a transaction's payload
-- about where its movement went, one typed column per key
-- (internal/canonical/payloadkeys.go names each key and its writers).
-- One row per transaction. A key the payload lacks reads NULL, and so
-- does a counter_amount that is not a number.
CREATE MACRO txn_evidence() AS TABLE (
    SELECT t.silver_source_id, t.transaction_external_id, t.account_external_id,
           json_extract_string(t.payload, '$.bank_ref') AS bank_ref,
           json_extract_string(t.payload, '$.counter_account') AS counter_account,
           json_extract_string(t.payload, '$.counter_currency') AS counter_currency,
           TRY_CAST(json_extract_string(t.payload, '$.counter_amount') AS DOUBLE) AS counter_amount,
           json_extract_string(t.payload, '$.returns_flow') AS returns_flow,
           json_extract_string(t.payload, '$.source_kind') AS source_kind,
           json_extract_string(t.payload, '$.fee_for') AS fee_for,
           json_extract_string(t.payload, '$.equity_transfer_ledger') AS equity_transfer_ledger
      FROM transactions t
);

-- spend_scoped_accounts: every account can spend, so there is no kind
-- gate.
--
-- A list of account kinds is wrong in the same way each time it is
-- widened: a managed account pays its own fees whatever its kind, and
-- each new entry only postpones the next miss.
--
-- What the kinds would gate is nothing. An account is a place
-- money sits; whether it can be charged is not a property of its
-- type. Safekeeping depots carry custody prices, private-market
-- custody accounts carry execution fees, loan accounts carry
-- interest, and a cash account attached to a mandate carries that
-- mandate's fees whatever the product is called. None of those kinds
-- is `cash`, `card` or `brokerage`, and a scope built from a list of
-- kinds cannot see any of them.
--
-- So the default is `include` and `spend_account_scope` is the ONLY
-- thing that takes an account out. One mechanism, and the exception is
-- written where exceptions are read rather than compiled into a list
-- nobody looks at.
--
-- This does NOT widen what counts as spending. The population
-- (spend_enrichment_population) selects on transaction KIND —
-- `purchase`, `refund`, `withdrawal`, `fee`, `tax`, and `interest`
-- only when charged — so a buy, a sell, a dividend, a journal and a
-- corporate action stay out whatever account they sit on. An account
-- in scope adds exactly its outflows.
--
-- One consequence is worth naming because it is the design working:
-- a withdrawal out of an account the household also owns the other
-- side of is an internal transfer, and the matcher — whose pool spans
-- every account rather than the scoped ones (spend_matcher_pool) —
-- nets the pair out before the base is read. A
-- withdrawal to somewhere gold does not carry stays as spend, which
-- is the honest answer: from the data's point of view the money left.
--
-- A `donor_advised_fund` account is the usual case for
-- `spend_account_scope`: a grant can double-count giving already
-- booked when the fund was contributed to. That argues about a
-- particular arrangement rather than about a type, so it belongs in
-- the scope table, where it can be read and argued with.
CREATE MACRO spend_scoped_accounts() AS TABLE (
    SELECT a.silver_source_id, a.account_external_id, a.account_kind,
           a.display_name, a.nickname, a.account_category,
           a.portfolio_external_id
      FROM accounts a
      LEFT JOIN spend_account_scope s
             ON s.silver_source_id    = a.silver_source_id
            AND s.account_external_id = a.account_external_id
     WHERE COALESCE(s.mode, 'include') = 'include'
);

-- ============================================================
-- The income populations, layered — three macros where spending has
-- four, the missing one being the matcher pool it shares.
--
--   income_scoped_accounts()             which accounts count at all
--   income_enrichment_population(f, t)   what the enrichment pass sees
--   income_lines_base(f, t)              what a view charts
--
-- The layering is the same argument as spending's: the pass is what
-- DECIDES which rows are own-account moves or returned capital, so it
-- cannot read a population that has already dropped them.
-- ============================================================

-- income_scoped_accounts: every account, until income_account_scope
-- takes one out. No kind gate — spend_scoped_accounts' argument for
-- the outflow side holds here too, and more plainly: whether an
-- account can RECEIVE is not a property of its type.
CREATE MACRO income_scoped_accounts() AS TABLE (
    SELECT a.silver_source_id, a.account_external_id, a.account_kind,
           a.display_name, a.nickname, a.account_category,
           a.portfolio_external_id
      FROM accounts a
      LEFT JOIN income_account_scope s
             ON s.silver_source_id    = a.silver_source_id
            AND s.account_external_id = a.account_external_id
     WHERE COALESCE(s.mode, 'include') = 'include'
);

-- income_enrichment_population: every transaction the enrichment pass
-- may write an income verdict for. No type resolution, no exclusion of
-- what turns out not to be income: the pass decides those.
--
-- Kind rules, each a decision rather than a detail:
--
--   * `dividend`, `coupon`, `staking`, `capital_gain`, `reward` and
--     `distribution` enter with BOTH signs, so that a negative row — a
--     dividend clawed back, a credit reversed, a distribution
--     corrected — nets against what it reverses inside its own type,
--     the way a refund nets inside a merchant's category. On
--     `capital_gain` and `staking` a negative row need not be a
--     reversal at all: a fund can pay out a realised LOSS and a
--     proof-of-stake chain can slash a stake, which is why
--     canonical/sign.go pins neither kind's sign. The same arithmetic
--     nets it inside the same type, which is the right answer either
--     way.
--     `deposit` is in that list for the same reason, though it takes
--     an argument to see. Its canonical sign is positive and
--     ApplyCanonicalSign forces it (canonical/sign.go), so the only
--     route to a negative `deposit` is an adapter that deliberately
--     bypassed that helper on a reversal marker — the protocol
--     sign.go documents and the ubs-web reader implements, mapping a
--     reversed counter deposit to `deposit` with the source's sign
--     intact. A negative `deposit` is therefore a reversal by
--     construction, not money leaving: money leaving has a kind of its
--     own, `withdrawal`. Excluding it would drop the correction and
--     leave the booking it corrects standing.
--   * `interest` is the ONE kind here with a sign guard, and the guard
--     is not a policy about reversals. One canonical kind carries two
--     opposite events — interest credited and interest charged — and
--     the sign is the only thing that tells them apart. The spending
--     population takes the negative half; income takes the
--     positive half. Without the guard the same row would be booked by
--     both families, in opposite directions. A zero-amount interest
--     row satisfies neither guard and is in neither population, which
--     is the honest answer for a booking that moved nothing.
--   * `distribution` is admitted although it floors to `capital_return`
--     and therefore leaves the base. The pass has to SEE it to record
--     its signature and to let a rule or a pin promote the exception —
--     a private debt fund's interest, a pass-through dividend — which
--     is the same reason the spending population admits the rows its
--     base drops as own-account moves.
--   * `sell`, `contribution`, `transfer_in`, `card_payment`,
--     `corporate_action`, `journal`, `other` and the FX kinds are
--     absent. Sale proceeds are the holder's own capital coming back,
--     and a `contribution` is that capital going out — the
--     private-market analogue of a buy — so a positive one is a
--     commitment refunded rather than anything earned; a securities
--     transfer, a card bill's own leg and a bookkeeping entry are not
--     receipts; an FX leg moves one currency into another rather than
--     bringing money in; a corporate action is a change to a HOLDING, and
--     the cash a source books for one arrives under `distribution` or
--     `capital_gain`, which are admitted; `other` is the catch-all the
--     UBS adapter demotes conduit legs to and carries no reliable
--     sign. `wealthdb status -v` counts rows carrying either catch-all
--     kind — `other` or `journal` — on an account in SPENDING's scope.
--     Where the two scopes agree, which is the default, that count
--     answers for both families; where they diverge it answers for
--     spending's.
--
-- instrument_external_id is projected here and nowhere on the spending
-- side, because the payer of a dividend, a coupon, a staking reward or
-- a fund distribution IS the instrument (see income_txn_categories
-- below) and those rows carry no narrative at all.
--
-- net_amount keeps its canonical sign; currency conversion belongs to
-- the report macros that wrap these.
CREATE MACRO income_enrichment_population(p_from, p_to) AS TABLE (
    SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
           t.account_external_id, sa.account_kind, sa.display_name,
           sa.nickname, sa.account_category, sa.portfolio_external_id,
           t.kind, t.currency, t.net_amount, t.description,
           t.counterparty, t.provider_category, t.instrument_external_id
      FROM transactions t
      JOIN income_scoped_accounts() sa
             ON sa.silver_source_id    = t.silver_source_id
            AND sa.account_external_id = t.account_external_id
     WHERE t.occurred_at BETWEEN p_from AND p_to
       AND kind_is_income(t.kind, t.net_amount)
);

-- spend_enrichment_population: the rows the spending pass sees. The
-- kind `reward` is not among them: a rewards credit is income, and is
-- never netted against card spend.
--
-- Cashback, a statement credit, an account-opening bonus and a referral
-- bonus all arrive as `reward`, and only the first has any relationship
-- to spending at all. Netting them here would also contradict the rule
-- on the outflow side — a statement paydown is an own-account move, and
-- there is no such thing as a rebate on one. So the kind is income
-- whole: INCOME_REWARDS at the income floor (income_txn_categories),
-- and out of the population here.
--
-- A card adapter kinds a cashback credit `refund` — a statement credit
-- is indistinguishable from a merchant credit once the issuer's own
-- descriptor is gone — so such a credit nets inside the merchant's
-- category on the spending side. An adapter that tells the two apart
-- and emits `reward` moves it to income.
--
-- spending_lines_base selects from this macro, so it follows.
CREATE MACRO spend_enrichment_population(p_from, p_to) AS TABLE (
    SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
           t.account_external_id, sa.account_kind, sa.display_name,
           sa.nickname, sa.account_category, sa.portfolio_external_id,
           t.kind, t.currency, t.net_amount, t.description,
           t.counterparty, t.provider_category
      FROM transactions t
      JOIN spend_scoped_accounts() sa
             ON sa.silver_source_id    = t.silver_source_id
            AND sa.account_external_id = t.account_external_id
     WHERE t.occurred_at BETWEEN p_from AND p_to
       AND kind_is_spending(t.kind, t.net_amount)
);

-- ============================================================
-- The withholding memo.
--
-- Tax deducted at source, bucketed and converted exactly like the
-- lines it sits beside, so the two numbers are comparable. It reads
-- `transactions` directly rather than any income macro: withholding is
-- an OUTFLOW and is in no income population, which is the whole point
-- of showing it as a memo rather than as part of the arithmetic.
--
-- Scoped to income's accounts, since that is the set the income beside
-- it was summed over. Positive magnitude, like every other figure in
-- these reports.
-- ============================================================
CREATE MACRO income_withheld_outccy(p_from, p_to, p_ccy, p_period) AS TABLE (
    SELECT spend_period_bucket(p_period, t.occurred_at) AS period_start,
           CAST(SUM(-COALESCE(CASE WHEN t.currency = p_ccy THEN t.net_amount::DOUBLE END,
               t.net_amount::DOUBLE * d.rate, t.net_amount::DOUBLE * c1.rate * c2.rate,
               t.net_amount::DOUBLE * u1.rate * u2.rate)) AS DECIMAL(28,4)) AS withheld
      FROM transactions t
      JOIN income_scoped_accounts() sa
             ON sa.silver_source_id    = t.silver_source_id
            AND sa.account_external_id = t.account_external_id
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy  = t.currency AND d.to_ccy  = p_ccy AND d.day  <= (t.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = t.currency AND c1.to_ccy = 'CHF' AND c1.day <= (t.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= (t.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = t.currency AND u1.to_ccy = 'USD' AND u1.day <= (t.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= (t.occurred_at // 86400)
     WHERE t.occurred_at BETWEEN p_from AND p_to
       AND t.kind = 'tax' AND t.net_amount < 0
     GROUP BY 1
);

-- On the income side the kind floor outranks the payer store.
--
-- The two families ask a model different questions, and the ordering
-- follows from which question it is.
--
-- On the OUTFLOW side the model names a merchant and picks that
-- merchant's category. It is never allowed to decide that a card
-- purchase was actually an investment: the nature of the row is the
-- data's, and what the model adds is a finer reading of who was paid.
-- A merchant verdict is therefore finer than any kind floor, and
-- spend_txn_categories puts the floor under the store for exactly that
-- reason.
--
-- On the INFLOW side the model is asked what KIND OF INCOME a receipt
-- is, which is a claim about the transaction itself rather than about
-- who sent it. Wherever the data already makes that claim — a
-- `dividend` is dividend income, a `staking` credit is staking income,
-- a `coupon` is interest earned — the model has nothing to add and can
-- only be wrong. So the floor is read OVER the store here, and the
-- model has a say on the one kind that has no floor at all: `deposit`.
--
-- Why it matters, concretely. A payer signature is shared across every
-- row that folds to it, so an employer whose shares are also held would
-- carry one verdict onto both: the salary deposit's INCOME_WAGES would
-- re-type that employer's dividends. Under this ordering the dividend
-- keeps INCOME_DIVIDENDS with provenance `kind` and the deposit keeps
-- the store's verdict with provenance `model`. The same ordering is
-- what makes `categorize income --all` safe to run: re-asking every
-- deposit signature cannot reach a floor-placed row. (The
-- candidate query is restricted to `deposit` as well, so such a row is
-- never asked about in the first place — two independent guards, as the
-- fence and the context level are.)
--
-- EVERY TIER THE PASS WRITES outranks the floor — pin, matcher,
-- built-in rule, config rule and provider map alike — because they all
-- write income_txn_enrichment and e.income_detailed is the head of the
-- COALESCE. Only the model sits below the floor. So a `distribution`
-- floors to `capital_return`, and a tier that writes the overlay
-- promotes it, which in practice means a rule or a pin.
--
-- NULLIF on the instrument's name and symbol: some adapters pass a name
-- pointer unconditionally and the writer stores `''`, which COALESCE
-- reads as an answer. An instrument named by the empty string would
-- blank the payer instead of falling through to the store or the
-- signature.
--
-- THE KIND FLOOR is where the two families are least alike. On the
-- outflow side a narrative names a merchant and the floor is a backstop
-- for the few rows that have none. On the inflow side the floor answers
-- for every kind the population admits except one: a `dividend` row is
-- dividend income whatever its narrative says. The exception is
-- `deposit`, which has no floor because nothing but the narrative can
-- say what it was — so the narrative tiers, and the model, are there
-- for that one kind.
--
-- `distribution` floors to a DELTA, which no spending floor does. A
-- private fund's distribution returns contributed capital first, so
-- most of what it pays out is basis rather than income; it is therefore
-- excluded until proven otherwise, and a rule or a pin promotes the
-- exception. `capital_gain` is the near neighbour that stays income: a
-- public fund's payout of realised gains returns no basis at all, and
-- the holding is still held.
--
-- `interest` is floored only when POSITIVE, and the guard is kept
-- rather than justified. No state the pass can produce reaches it: a
-- pin writes its own value, which wins ahead of the floor; the matcher
-- pool excludes `interest`; and the population excludes the negative
-- half. It is a guard on a canonical kind that carries two opposite
-- events, written where the kinds are enumerated so that a reader
-- meeting the list here is not left to infer which half is meant.
-- spend_txn_categories carries the same guard on the same kind from the
-- other side, and the two should read alike.
--
-- THE PAYER resolves in four steps, the first two of which differ from
-- the merchant's:
--
--   1. a line whose resolved type is a DELTA has no payer. An
--      own-account move names the holder's own bank and a gift names a
--      relative; neither is a payer. Spending blanks the same lines
--      but has a card-issuer label to put there instead; income has
--      nothing to say, and says nothing.
--   2. a line that carries an INSTRUMENT is paid by that instrument —
--      the company, the issuer, the protocol, the fund — named by the
--      instrument's name, or by its symbol where the name is unknown.
--      It answers for every row carrying an instrument, which is every
--      dividend, coupon, staking reward and fund distribution.
--   3. otherwise the payer store's name for the line's signature.
--   4. otherwise the signature itself, which is the fold of whatever
--      narrative the source gave.
--
-- Steps 2 to 4 are one COALESCE because the rule is first that ANSWERS,
-- not first that applies: an instrument gold holds under neither a name
-- nor a symbol, or an instrument_external_id no instruments row
-- answers to, has not named a payer, and the line falls through to
-- whatever the narrative folded to rather than reading blank. Only the
-- delta arm is terminal, and deliberately so — there is no payer to
-- find, so there is nothing to fall through to.
--
-- The payer's steps keep the store above the floor. A verdict's NAME
-- is the model's to give: naming who paid is the same job on both
-- sides, and nothing in the data names an instrument-free payer. Only
-- the TYPE puts the floor first.
CREATE MACRO income_txn_categories() AS TABLE (
    WITH floored AS (
        SELECT e.*,
               t.instrument_external_id,
               income_kind_floor(t.kind, t.net_amount) AS kind_floor
          FROM income_txn_enrichment e
          LEFT JOIN transactions t
                 ON t.silver_source_id        = e.silver_source_id
                AND t.transaction_external_id = e.transaction_external_id
    )
    SELECT e.silver_source_id, e.transaction_external_id,
           e.payer_signature,
           CASE WHEN c.spend_primary = c.spend_detailed THEN NULL
                ELSE COALESCE(NULLIF(i.name, ''), NULLIF(i.symbol, ''),
                              m.payer_name,
                              NULLIF(TRIM(e.payer_signature), ''))
           END AS payer_name,
           COALESCE(e.income_detailed, e.kind_floor, m.income_detailed) AS income_detailed,
           c.spend_primary AS income_primary,
           c.label         AS income_label,
           c.primary_label AS income_primary_label,
           CASE WHEN e.income_detailed IS NOT NULL THEN e.provenance
                WHEN e.kind_floor      IS NOT NULL THEN 'kind'
                WHEN m.income_detailed IS NOT NULL THEN 'model'
                ELSE e.provenance END AS provenance,
           e.provider_income_detailed,
           pc.spend_primary  AS provider_income_primary,
           pc.label          AS provider_income_label,
           pc.primary_label  AS provider_income_primary_label
      FROM floored e
      LEFT JOIN income_payer_categories m
             ON m.payer_signature = e.payer_signature
      LEFT JOIN instruments i
             ON i.silver_source_id       = e.silver_source_id
            AND i.instrument_external_id = e.instrument_external_id
      LEFT JOIN spend_categories c
             ON c.spend_detailed = COALESCE(e.income_detailed, e.kind_floor,
                                            m.income_detailed)
      LEFT JOIN spend_categories pc
             ON pc.spend_detailed = e.provider_income_detailed
);

-- cashflow_pool_spans: the household's cash pool, dated — every span
-- of a scoped account's tax wrapper (account_wrapper_spans) whose
-- wrapper is on the household side. A dated wrapper override applies
-- from its day on, so an account can be in the pool on one day and out
-- of it on another. This is the one statement of "in the pool".
--
-- An UNSET wrapper reads as household, which is the render-time
-- default gold applies everywhere else and the safe direction: nothing
-- is silently removed from the statement. It is not free, and nothing
-- in the reconciliation can see the cost — an adapter that leaves the
-- column unset puts a retirement or health account inside the pool,
-- where its own trades become household investing and its
-- contributions become invisible, and the crossing is ABSENT rather
-- than wrong. `wealthdb status -v` counts pooled accounts with no
-- wrapper for exactly that reason.
--
-- The join is LEFT and the COALESCE is on the SIDE rather than on the
-- wrapper, so an account carrying a wrapper the stamped table does not
-- hold — a database migrated ahead of its binary — reads as household
-- too, by the same rule and not by accident.
CREATE MACRO cashflow_pool_spans() AS TABLE (
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

-- cashflow_pool_accounts: the pool today — every account whose span
-- that is current, the one with no end, is in the pool. That span
-- carries the account's own tax wrapper (account_wrapper_spans).
CREATE MACRO cashflow_pool_accounts() AS TABLE (
    SELECT a.silver_source_id, a.account_external_id, a.account_kind,
           a.display_name, a.nickname, a.account_category,
           a.portfolio_external_id, a.tax_wrapper
      FROM accounts a
      JOIN cashflow_pool_spans() p
        ON p.silver_source_id    = a.silver_source_id
       AND p.account_external_id = a.account_external_id
       AND p.until_at = CAST(9223372036854775807 AS BIGINT)
);

CREATE MACRO cashflow_section_rank(p_section) AS (
    CASE p_section
        WHEN 'operating_in'  THEN 1
        WHEN 'operating_out' THEN 2
        WHEN 'investing'     THEN 3
        WHEN 'financing'     THEN 4
        WHEN 'vehicles'      THEN 5
        WHEN 'cash'          THEN 6
    END
);

-- The reconciliation memo: the one check that can catch a wrong
-- POPULATION rather than wrong arithmetic.
--
-- The cash section is computed, never measured. That is what makes the
-- diagram balance by construction, and it is also its weakness: an
-- unpaired transfer, a distribution promoted wrongly, a bank that books
-- a wire under a catch-all kind — each lands in the residual and looks
-- like cash saved or spent. The two families' honesty surfaces cannot
-- catch that: their summary-equals-categories identity is structural,
-- and their canary counts only rows nothing placed.
--
-- Cashflow has something they lack: the pool's balances are OBSERVED.
--
--   cash_measured  the pool's CASH BALANCES at the bucket's end minus
--                  at its start, in the output currency at each
--                  boundary day's own rate. Balances, not the pool's
--                  whole cash: a money-market fund is in the pool by
--                  asset class and is a POSITION, so buying one leaves
--                  this spine. That is handled on the flow side rather
--                  than by a second spine whose snapshot series would
--                  diverge from this one.
--   fx_effect      the revaluation part of that change. TWO terms, and
--                  both are needed: the opening balances revalued from
--                  the opening rate to the closing one, AND every line
--                  in the bucket revalued from its own day's rate to
--                  the closing one. Without the second term, a salary
--                  received mid-year and held to the year's end — a
--                  flow counted at its own day's rate and a balance
--                  counted at the year's end — would masquerade as an
--                  error.
--   unexplained    cash_measured − fx_effect − net cash flow, a
--                  native-currency mismatch. Zero when every line is
--                  placed and every own-account move paired. The flow it
--                  subtracts is the statement's, over the accounts the
--                  memo can measure — see the restrictions below.
--
-- The arithmetic closes exactly in a world with no misfiling. Writing
-- B for a native opening balance and a for a native line amount, and
-- r(d) for the rate on day d:
--
--   cash_measured = Σ(B+Σa)·r(close) − Σ B·r(open)
--                 = Σ B·(r(close) − r(open))          [the first term]
--                 + Σ a·(r(close) − r(a's day))       [the second]
--                 + Σ a·r(a's day)                    [net_cash_flow]
--
-- NEAR ZERO, NOT ZERO, and this says so rather than promising an
-- identity it cannot keep. Balances are snapshot-grain and carried
-- forward, so a bucket boundary falling between two snapshots reads a
-- stale balance and the miss reverses next bucket. It is a diagnostic
-- with a stated tolerance, never an input: net_cash_flow does not read
-- it, and the columns are off by default.
--
-- FOUR RESTRICTIONS, each closing a way the memo would be wrong about
-- itself rather than about the statement:
--
--   * only POOLED accounts. A vehicle's balance change is not the
--     household's cash.
--   * only accounts that contribute TRANSACTIONS. An account loaded for
--     balances alone has a real balance change and no lines by
--     construction, and would read as a permanent hole.
--   * only accounts that HAVE a balance history, and the synthetic
--     per-portfolio `overlay` accounts never — one balance must not be
--     counted twice. An account with lines and no balances anywhere is
--     unmeasurable rather than unexplained, and is left out of both
--     sides of the subtraction so the diagnostic stays a diagnostic.
--   * the balance KIND is pinned to the same precedence `cash_chosen`
--     applies (current > closing > available > aggregated > opening >
--     initial > projected), but WITHOUT its non-zero filter: an account
--     whose balance went to zero has not stopped existing, and dropping
--     it would report the whole balance as a disappearance.
--
-- An account whose balance history has not started by a boundary is
-- not measured across it: it joins the memo at its first snapshot, and
-- the lines it contributed before that are outside the comparison. A
-- bucket in which nothing is measurable, or whose amounts no rate can
-- value, reports BLANK rather than a number computed from part of the
-- pool. So the memo covers the SLICE it can measure rather than the
-- whole statement, and `status -v` counts what it cannot reach.

-- ============================================================
-- cashflow_period_end: the last second of the bucket a period_start
-- opens, clamped to the window.
--
-- A macro parameter cannot be an INTERVAL unit any more than it can be
-- a date_trunc part, so the six periods are named one by one. `total`
-- has no next bucket: the window is the bucket.
-- ============================================================
CREATE MACRO cashflow_period_end(p_period, p_start, p_to) AS (
    CASE WHEN p_start IS NULL THEN p_to
         ELSE LEAST(p_to, CAST(epoch(
             CASE p_period
                 WHEN 'day'     THEN epoch_ms(p_start * 1000) + INTERVAL 1 DAY
                 WHEN 'week'    THEN epoch_ms(p_start * 1000) + INTERVAL 1 WEEK
                 WHEN 'month'   THEN epoch_ms(p_start * 1000) + INTERVAL 1 MONTH
                 WHEN 'quarter' THEN epoch_ms(p_start * 1000) + INTERVAL 3 MONTH
                 WHEN 'year'    THEN epoch_ms(p_start * 1000) + INTERVAL 1 YEAR
             END) AS BIGINT) - 1)
    END
);

-- income_lines_base: the income lines. in_pool says whether the line's
-- account is in the cash pool on the line's own day
-- (cashflow_pool_spans).
CREATE MACRO income_lines_base(p_from, p_to) AS TABLE (
    SELECT p.silver_source_id, p.transaction_external_id, p.occurred_at,
           p.account_external_id, p.account_kind, p.display_name,
           p.nickname, p.account_category,
           p.kind, p.currency, p.net_amount, p.description,
           p.counterparty, p.provider_category,
           c.payer_signature, c.payer_name, c.income_detailed,
           c.income_primary, c.income_label, c.income_primary_label,
           c.provenance,
           c.provider_income_detailed, c.provider_income_primary,
           c.provider_income_label, c.provider_income_primary_label,
           (ps.silver_source_id IS NOT NULL) AS in_pool
      FROM income_enrichment_population(p_from, p_to) p
      LEFT JOIN income_txn_categories() c
             ON c.silver_source_id        = p.silver_source_id
            AND c.transaction_external_id = p.transaction_external_id
      LEFT JOIN cashflow_pool_spans() ps
             ON ps.silver_source_id    = p.silver_source_id
            AND ps.account_external_id = p.account_external_id
            AND p.occurred_at >= ps.from_at AND p.occurred_at < ps.until_at
     WHERE detailed_stays_in_income(c.income_detailed)
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

-- The income reports, and the income columns on `wealthdb transactions`.
--
-- The spending reports read in the other direction: every macro below
-- is its spending twin with the income vocabulary, and where one
-- differs there is a reason in a comment. The FX blocks are duplicated
-- rather than shared because a DuckDB table macro cannot take a table
-- as a parameter — the same reason the spending pair duplicates them.
--
-- The vocabulary of the numbers is the mirror of spending's, and the
-- mirror is exact: spending sums debits as `spend` and credits as
-- `refunds` and subtracts; income sums credits as `income` and debits
-- as `reversals` and subtracts. Both report positive magnitudes, so a
-- reader comparing the two columns is comparing like with like, and
-- `net_income` is what the household actually received.
--
-- One column has no spending twin: `withheld`. It is the tax deducted
-- at source in the same window on the same accounts — shown beside the
-- income it was withheld from, off by default, and NEVER subtracted.
-- Income is gross as booked, so the withholding is already the
-- spending side's WITHHOLDING_TAX; netting it here would subtract it
-- twice from the household's arithmetic. It is a memo, and
-- `net_income` does not read it.

-- ============================================================
-- The FX pair: the base with its amounts converted.
-- ============================================================

CREATE MACRO income_lines_outccy(p_from, p_to, p_ccy) AS TABLE (
    SELECT b.*,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * d.rate, b.net_amount::DOUBLE * c1.rate * c2.rate,
               b.net_amount::DOUBLE * u1.rate * u2.rate) AS DECIMAL(28,4)) AS value_outccy
      FROM income_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy  = b.currency AND d.to_ccy  = p_ccy AND d.day  <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= (b.occurred_at // 86400)
);

-- ============================================================
-- The three reports.
-- ============================================================

-- report_income_summary: one row per bucket. `income` and `reversals`
-- are positive magnitudes and `net_income` is the difference, mirroring
-- report_spending_summary's spend/refunds/net_spend exactly.
--
-- A bucket where nothing converted reports NULL rather than zero — the
-- same n_converted guard the spending summary uses, so a missing FX
-- rate reads as "not known" instead of "nothing received".
--
-- `withheld` is FULL JOINed by bucket, so neither side can drop a
-- bucket the other has: a window with income and no withholding shows
-- the income and an empty memo, and a bucket whose only booking was a
-- tax row keeps its row rather than vanishing. The join itself carries
-- the reasoning.
CREATE MACRO report_income_summary(p_from, p_to, p_ccy, p_period) AS TABLE (
    WITH agg AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COUNT(*)            AS txn_count,
               COUNT(value_outccy) AS n_converted,
               SUM(CASE WHEN value_outccy > 0 THEN  value_outccy ELSE 0 END) AS income_raw,
               SUM(CASE WHEN value_outccy < 0 THEN -value_outccy ELSE 0 END) AS reversals_raw
          FROM income_lines_outccy(p_from, p_to, p_ccy)
         GROUP BY 1),
    vals AS (
        SELECT period_start, txn_count,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE income_raw    END AS DECIMAL(28,4)) AS income_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE reversals_raw END AS DECIMAL(28,4)) AS reversals_val
          FROM agg)
    -- A FULL join, not a LEFT one. The two sides are independent
    -- populations: a bucket can hold income and no withholding (the
    -- common case) and, on a quarter whose only booking was tax
    -- deducted at source, withholding and no income. A left join would
    -- drop that second bucket entirely and take the memo with it —
    -- the one row a reader went looking for.
    --
    -- IS NOT DISTINCT FROM, not `=`: `--period total` collapses the
    -- window into ONE bucket whose period_start is NULL on both sides,
    -- and NULL = NULL matches nothing. An equality join here would
    -- leave the memo empty on exactly the report most likely to be read
    -- for a tax year.
    SELECT COALESCE(v.period_start, w.period_start) AS period_start,
           COALESCE(v.txn_count, 0) AS txn_count,
           CAST(v.income_val    AS VARCHAR) AS income,
           CAST(v.reversals_val AS VARCHAR) AS reversals,
           CAST(v.income_val - v.reversals_val AS VARCHAR) AS net_income,
           CAST(w.withheld AS VARCHAR) AS withheld
      FROM vals v
      FULL JOIN income_withheld_outccy(p_from, p_to, p_ccy, p_period) w
             ON w.period_start IS NOT DISTINCT FROM v.period_start
     ORDER BY 1
);

-- report_income_types: a (bucket, type) pair with its share of the
-- bucket. report_spending_categories' body with the income vocabulary;
-- `share` is over net-income magnitudes, as spending's is over net
-- spend.
--
-- No `withheld` here, and that is a correction to the first draft of
-- the design rather than an omission: a tax row names the security it
-- was withheld from, not the income TYPE, so there is no honest way to
-- attribute it to one row of this report. The memo lives on the summary,
-- where the window is the only grain it needs.
CREATE MACRO report_income_types(p_from, p_to, p_ccy, p_period, p_level) AS TABLE (
    WITH labelled AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COALESCE(CASE WHEN p_level = 'primary' THEN income_primary ELSE income_detailed END,
                        '(uncategorized)') AS type,
               COALESCE(CASE WHEN p_level = 'primary' THEN income_primary_label ELSE income_label END,
                        '(uncategorized)') AS type_label,
               value_outccy
          FROM income_lines_outccy(p_from, p_to, p_ccy)),
    agg AS (
        SELECT period_start, type, type_label,
               COUNT(*)            AS txn_count,
               COUNT(value_outccy) AS n_converted,
               SUM(CASE WHEN value_outccy > 0 THEN  value_outccy ELSE 0 END) AS income_raw,
               SUM(CASE WHEN value_outccy < 0 THEN -value_outccy ELSE 0 END) AS reversals_raw
          FROM labelled
         GROUP BY 1, 2, 3),
    vals AS (
        SELECT period_start, type, type_label, txn_count,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE income_raw    END AS DECIMAL(28,4)) AS income_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE reversals_raw END AS DECIMAL(28,4)) AS reversals_val
          FROM agg),
    net AS (
        SELECT period_start, type, type_label, txn_count, income_val, reversals_val,
               CAST(income_val - reversals_val AS DECIMAL(28,4)) AS net_val
          FROM vals)
    SELECT period_start, type, type_label, txn_count,
           CAST(income_val    AS VARCHAR) AS income,
           CAST(reversals_val AS VARCHAR) AS reversals,
           CAST(net_val       AS VARCHAR) AS net_income,
           ABS(net_val)::DOUBLE
               / NULLIF(SUM(ABS(net_val)) OVER (PARTITION BY period_start), 0) AS share
      FROM net
     ORDER BY period_start, net_val DESC, type
);

-- report_income_transactions: an income line, oldest first. The
-- merchant columns of its spending twin become the payer ones; nothing
-- else moves.
CREATE MACRO report_income_transactions(p_from, p_to, p_ccy) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, payer_signature, payer_name, income_primary, income_detailed,
           income_label, income_primary_label,
           provenance, provider_income_detailed, provider_income_label, currency,
           CAST(net_amount AS VARCHAR) AS net_amount,
           description, counterparty,
           CAST(value_outccy AS VARCHAR) AS value_outccy
      FROM income_lines_outccy(p_from, p_to, p_ccy)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

CREATE MACRO cashflow_class_label(p_class) AS (
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
        WHEN 'unpaired'   THEN 'Unpaired transfers'
        WHEN 'cash'       THEN 'Cash savings'
        ELSE p_class
    END
);

CREATE MACRO cashflow_class_rank(p_class) AS (
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
        WHEN p_class = 'unpaired'        THEN 6
        WHEN p_class = 'cash'            THEN 1
        ELSE 40
    END
);

-- spend_txn_categories: a card's unplaced fee is a bank fee.
--
-- An unplaced `fee` row floors to BANK_FEES_OTHER_BANK_FEES on a card
-- and on a `cash` account in no portfolio, and to
-- BANK_FEES_INVESTMENT_FEES on every other account. An annual, late or
-- cash-advance fee that no tier placed is not the cost of holding
-- investments: a card holds none, and its fees are banking costs, as a
-- bank account's are. That holds for a card in a portfolio too: a card
-- is never the cash side of a mandate. A provider that files such a fee
-- under its own bank-fee catch-all declines to claim it, so the floor
-- is often what places it.
CREATE MACRO spend_txn_categories() AS TABLE (
    WITH floored AS (
        SELECT e.*,
               spend_kind_floor(t.kind, t.net_amount, a.account_kind,
                                a.portfolio_external_id) AS kind_floor
          FROM spend_txn_enrichment e
          LEFT JOIN transactions t
                 ON t.silver_source_id        = e.silver_source_id
                AND t.transaction_external_id = e.transaction_external_id
          LEFT JOIN accounts a
                 ON a.silver_source_id        = t.silver_source_id
                AND a.account_external_id     = t.account_external_id
    )
    SELECT e.silver_source_id, e.transaction_external_id,
           e.merchant_signature,
           CASE WHEN c.spend_primary = c.spend_detailed THEN e.merchant_label
                ELSE COALESCE(m.merchant_name, NULLIF(TRIM(e.merchant_signature), ''))
           END AS merchant_name,
           COALESCE(e.spend_detailed, m.spend_detailed, e.kind_floor) AS spend_detailed,
           c.spend_primary,
           c.label         AS spend_label,
           c.primary_label AS spend_primary_label,
           CASE WHEN e.spend_detailed IS NULL AND m.spend_detailed IS NOT NULL
                     THEN 'model'
                WHEN e.spend_detailed IS NULL AND m.spend_detailed IS NULL
                     AND e.kind_floor IS NOT NULL THEN 'kind'
                ELSE e.provenance END AS provenance,
           e.provider_spend_detailed,
           pc.spend_primary  AS provider_spend_primary,
           pc.label          AS provider_spend_label,
           pc.primary_label  AS provider_spend_primary_label
      FROM floored e
      LEFT JOIN spend_merchant_categories m
             ON m.merchant_signature = e.merchant_signature
      LEFT JOIN spend_categories c
             ON c.spend_detailed = COALESCE(e.spend_detailed, m.spend_detailed,
                                            e.kind_floor)
      LEFT JOIN spend_categories pc
             ON pc.spend_detailed = e.provider_spend_detailed
);

CREATE MACRO report_transactions_multi(p_from, p_to) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.account_kind, a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description,
               sc.merchant_name, sc.spend_primary, sc.spend_detailed
          FROM transactions t
          LEFT JOIN accounts a ON t.silver_source_id = a.silver_source_id AND t.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON t.silver_source_id = i.silver_source_id AND t.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = t.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = t.instrument_external_id
          LEFT JOIN symbol_resolutions srn ON srn.silver_source_id = t.silver_source_id
               AND srn.lookup_kind = 'name' AND srn.lookup_value = t.description
          LEFT JOIN spend_txn_categories() sc ON sc.silver_source_id = t.silver_source_id
               AND sc.transaction_external_id = t.transaction_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.account_kind, b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS DECIMAL(28,4)) AS gross_amount, CAST(b.net_amount AS DECIMAL(28,4)) AS net_amount,
           CAST(b.quantity AS DECIMAL(28,8)) AS quantity, CAST(b.price AS DECIMAL(28,8)) AS price, b.description,
           b.merchant_name, b.spend_primary, b.spend_detailed,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS value_usd,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS value_chf,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS value_eur,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS value_gbp
      FROM base b
      ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = b.currency AND fxl.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (b.occurred_at // 86400)
     ORDER BY b.occurred_at, b.silver_source_id, b.transaction_external_id
);

CREATE MACRO income_lines_multi(p_from, p_to) AS TABLE (
    SELECT b.*,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS value_usd,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS value_chf,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS value_eur,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS value_gbp
      FROM income_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = b.currency AND fxl.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (b.occurred_at // 86400)
);

-- income_projected_types: the projection's income types an income line
-- of detailed type `detailed` can be (gold's incomeTypeSQL): interest a
-- bond pays is a coupon, a dividend a fund pays a distribution. NULL for
-- a type the projection does not project (canonical's ProjectedIncome).
CREATE MACRO income_projected_types(detailed) AS
    CASE detailed WHEN 'INCOME_INTEREST_EARNED' THEN ['interest', 'coupons']
                  WHEN 'INCOME_DIVIDENDS' THEN ['dividends', 'distributions']
                  WHEN 'INCOME_DISTRIBUTIONS' THEN ['distributions']
                  WHEN 'INCOME_RENTAL' THEN ['rent']
                  WHEN 'INCOME_RETIREMENT_PENSION' THEN ['pension'] END;

-- income_line_class: the operating-in class of an income line of
-- detailed type `detailed` that the projection does not project, as the
-- cash flow statement classes a receipt (canonical's FlowClassIn, which
-- cashflow_txn_nodes reads from spend_categories): a line of no type is
-- the unresolved class, one of a type no class names other receipts.
-- NULL for a projected type, whose lines are the holdings' income. The
-- cash flow projection keeps its other income by it (gold.CashflowPlanIncome).
CREATE MACRO income_line_class(detailed) AS
    CASE WHEN detailed IS NULL THEN '(uncategorized)'
         WHEN income_projected_types(detailed) IS NOT NULL THEN NULL
         WHEN detailed IN ('INCOME_SALARY', 'INCOME_GIG_ECONOMY', 'INCOME_CONTRACTOR') THEN 'earnings'
         WHEN detailed IN ('INCOME_STAKING', 'INCOME_ROYALTIES', 'INCOME_ENERGY_FEED_IN') THEN 'yield'
         WHEN detailed IN ('INCOME_GOVERNMENT_BENEFITS', 'INCOME_UNEMPLOYMENT', 'INCOME_LONG_TERM_DISABILITY',
                           'INCOME_MILITARY', 'INCOME_CHILD_SUPPORT', 'INCOME_ALIMONY') THEN 'benefits'
         ELSE 'other_receipts' END;

CREATE MACRO report_income_transactions_multi(p_from, p_to) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, payer_signature, payer_name, income_primary, income_detailed,
           income_label, income_primary_label,
           provenance, provider_income_detailed, provider_income_label, currency,
           CAST(net_amount AS DECIMAL(28,4)) AS net_amount,
           description, counterparty,
           value_usd, value_chf, value_eur, value_gbp
      FROM income_lines_multi(p_from, p_to)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

CREATE MACRO spending_lines_own(p_from, p_to) AS TABLE (
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
     WHERE detailed_stays_in_spending(c.spend_detailed)
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

-- spend_reimbursements: each linked repayment, the outflow it answers
-- where a road names one, the category it nets in and the road
-- (`pair`, `signature`, `store`).
CREATE MACRO spend_reimbursements() AS TABLE (
    WITH repaid AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
               t.currency, t.net_amount, ic.payer_signature
          FROM income_txn_categories() ic
          JOIN transactions t
            ON t.silver_source_id        = ic.silver_source_id
           AND t.transaction_external_id = ic.transaction_external_id
         WHERE ic.income_detailed = 'reimbursement'
           AND t.net_amount > 0
    ),
    -- The spending lines the roads read: scoped, and with every delta
    -- and crossing already left out, so a repayment can only net
    -- against something spending counts.
    own AS (
        SELECT * FROM spending_lines_own(0, 9223372036854775807)
         WHERE spend_detailed IS NOT NULL
    ),
    paired AS (
        SELECT r.silver_source_id, r.transaction_external_id,
               o.silver_source_id AS out_silver_source_id,
               o.transaction_external_id AS out_transaction_external_id,
               o.spend_detailed
          FROM repaid r
          JOIN spend_reimbursement_pairs p
            ON p.silver_source_id        = r.silver_source_id
           AND p.transaction_external_id = r.transaction_external_id
          JOIN own o
            ON o.silver_source_id        = p.out_silver_source_id
           AND o.transaction_external_id = p.out_transaction_external_id
    ),
    signed AS (
        SELECT r.silver_source_id, r.transaction_external_id,
               o.silver_source_id AS out_silver_source_id,
               o.transaction_external_id AS out_transaction_external_id,
               o.spend_detailed
          FROM repaid r
          JOIN own o
            ON o.merchant_signature = r.payer_signature
           AND o.currency           = r.currency
           AND o.net_amount        <= -r.net_amount
           AND o.occurred_at       <= r.occurred_at
           AND o.occurred_at       >= r.occurred_at - 365 * 86400
       QUALIFY ROW_NUMBER() OVER (
                   PARTITION BY r.silver_source_id, r.transaction_external_id
                   ORDER BY o.occurred_at DESC, o.silver_source_id, o.transaction_external_id) = 1
    ),
    -- The store may hold a delta or a crossing for a payee (an own
    -- account's bank, a lender); a refund never nets against one.
    stored AS (
        SELECT r.silver_source_id, r.transaction_external_id, m.spend_detailed
          FROM repaid r
          JOIN spend_merchant_categories m ON m.merchant_signature = r.payer_signature
         WHERE detailed_stays_in_spending(m.spend_detailed)
    )
    SELECT r.silver_source_id, r.transaction_external_id,
           COALESCE(p.out_silver_source_id, s.out_silver_source_id) AS out_silver_source_id,
           COALESCE(p.out_transaction_external_id, s.out_transaction_external_id) AS out_transaction_external_id,
           COALESCE(p.spend_detailed, s.spend_detailed, m.spend_detailed) AS spend_detailed,
           CASE WHEN p.spend_detailed IS NOT NULL THEN 'pair'
                WHEN s.spend_detailed IS NOT NULL THEN 'signature'
                ELSE 'store' END AS road
      FROM repaid r
      LEFT JOIN paired p USING (silver_source_id, transaction_external_id)
      LEFT JOIN signed s USING (silver_source_id, transaction_external_id)
      LEFT JOIN stored m USING (silver_source_id, transaction_external_id)
     WHERE COALESCE(p.spend_detailed, s.spend_detailed, m.spend_detailed) IS NOT NULL
);

-- spending_lines_base: the spending lines and, on a spending-scoped
-- account, every linked repayment as a refund in its outflow's
-- category.
CREATE MACRO spending_lines_base(p_from, p_to) AS TABLE (
    SELECT * FROM spending_lines_own(p_from, p_to)
    UNION ALL
    SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
           t.account_external_id, sa.account_kind, sa.display_name,
           sa.nickname, sa.account_category,
           t.kind, t.currency, t.net_amount, t.description,
           t.counterparty, t.provider_category,
           ic.payer_signature AS merchant_signature, ic.payer_name AS merchant_name,
           rb.spend_detailed, sc.spend_primary, sc.label AS spend_label,
           sc.primary_label AS spend_primary_label,
           'reimbursement' AS provenance,
           NULL AS provider_spend_detailed, NULL AS provider_spend_primary,
           NULL AS provider_spend_label, NULL AS provider_spend_primary_label
      FROM spend_reimbursements() rb
      JOIN transactions t
        ON t.silver_source_id        = rb.silver_source_id
       AND t.transaction_external_id = rb.transaction_external_id
      JOIN spend_scoped_accounts() sa
        ON sa.silver_source_id    = t.silver_source_id
       AND sa.account_external_id = t.account_external_id
      JOIN income_txn_categories() ic
        ON ic.silver_source_id        = t.silver_source_id
       AND ic.transaction_external_id = t.transaction_external_id
      LEFT JOIN spend_categories sc ON sc.spend_detailed = rb.spend_detailed
     WHERE t.occurred_at BETWEEN p_from AND p_to
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

-- spending_lines_outccy: the spending base with each line's native
-- net_amount converted to p_ccy at its own occurred_at day. Same
-- COALESCE/ASOF shape as every other single-currency report macro
-- (identity, direct, via-CHF, via-USD); NULL when no path resolves.
CREATE MACRO spending_lines_outccy(p_from, p_to, p_ccy) AS TABLE (
    SELECT b.*,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * d.rate, b.net_amount::DOUBLE * c1.rate * c2.rate,
               b.net_amount::DOUBLE * u1.rate * u2.rate) AS DECIMAL(28,4)) AS value_outccy
      FROM spending_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy  = b.currency AND d.to_ccy  = p_ccy AND d.day  <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= (b.occurred_at // 86400)
);

-- ============================================================
-- report_spending_summary
-- ============================================================

-- One row per period bucket: how many spending lines it holds, the
-- gross outflow, what came back, and the difference.
--
-- NULL-vs-0 follows the other aggregate reports: a bucket only exists
-- because it has lines, so a bucket whose lines all failed to convert
-- reads NULL (no FX path), while a bucket that simply has no refunds
-- reads 0. An unconvertible line inside an otherwise convertible
-- bucket contributes nothing rather than voiding the bucket.
CREATE MACRO report_spending_summary(p_from, p_to, p_ccy, p_period) AS TABLE (
    WITH agg AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COUNT(*)                AS txn_count,
               COUNT(value_outccy)     AS n_converted,
               SUM(CASE WHEN value_outccy < 0 THEN -value_outccy ELSE 0 END) AS spend_raw,
               SUM(CASE WHEN value_outccy > 0 THEN  value_outccy ELSE 0 END) AS refunds_raw
          FROM spending_lines_outccy(p_from, p_to, p_ccy)
         GROUP BY 1),
    vals AS (
        SELECT period_start, txn_count,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE spend_raw   END AS DECIMAL(28,4)) AS spend_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE refunds_raw END AS DECIMAL(28,4)) AS refunds_val
          FROM agg)
    SELECT period_start, txn_count,
           CAST(spend_val   AS VARCHAR) AS spend,
           CAST(refunds_val AS VARCHAR) AS refunds,
           CAST(spend_val - refunds_val AS VARCHAR) AS net_spend
      FROM vals
     ORDER BY period_start
);

-- The category reports gain `category_label` beside `category`. The
-- key keeps its old spelling and its old name, so a caller that groups
-- or joins on it is untouched; the label is what a reader sees. A line
-- no tier placed reads "(uncategorized)" in both, as it did.
CREATE MACRO report_spending_categories(p_from, p_to, p_ccy, p_period, p_level) AS TABLE (
    WITH labelled AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COALESCE(CASE WHEN p_level = 'primary' THEN spend_primary ELSE spend_detailed END,
                        '(uncategorized)') AS category,
               COALESCE(CASE WHEN p_level = 'primary' THEN spend_primary_label ELSE spend_label END,
                        '(uncategorized)') AS category_label,
               value_outccy
          FROM spending_lines_outccy(p_from, p_to, p_ccy)),
    agg AS (
        SELECT period_start, category, category_label,
               COUNT(*)            AS txn_count,
               COUNT(value_outccy) AS n_converted,
               SUM(CASE WHEN value_outccy < 0 THEN -value_outccy ELSE 0 END) AS spend_raw,
               SUM(CASE WHEN value_outccy > 0 THEN  value_outccy ELSE 0 END) AS refunds_raw
          FROM labelled
         GROUP BY 1, 2, 3),
    vals AS (
        SELECT period_start, category, category_label, txn_count,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE spend_raw   END AS DECIMAL(28,4)) AS spend_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE refunds_raw END AS DECIMAL(28,4)) AS refunds_val
          FROM agg),
    net AS (
        SELECT period_start, category, category_label, txn_count, spend_val, refunds_val,
               CAST(spend_val - refunds_val AS DECIMAL(28,4)) AS net_val
          FROM vals)
    SELECT period_start, category, category_label, txn_count,
           CAST(spend_val   AS VARCHAR) AS spend,
           CAST(refunds_val AS VARCHAR) AS refunds,
           CAST(net_val     AS VARCHAR) AS net_spend,
           ABS(net_val)::DOUBLE
               / NULLIF(SUM(ABS(net_val)) OVER (PARTITION BY period_start), 0) AS share
      FROM net
     ORDER BY period_start, net_val DESC, category
);

-- The transaction reports carry both classifications, ours first.
CREATE MACRO report_spending_transactions(p_from, p_to, p_ccy) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, merchant_signature, merchant_name, spend_primary, spend_detailed,
           spend_label, spend_primary_label,
           provenance, provider_spend_detailed, provider_spend_label, currency,
           CAST(net_amount AS VARCHAR) AS net_amount,
           description, counterparty,
           CAST(value_outccy AS VARCHAR) AS value_outccy
      FROM spending_lines_outccy(p_from, p_to, p_ccy)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

CREATE MACRO spending_lines_multi(p_from, p_to) AS TABLE (
    SELECT b.*,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS value_usd,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS value_chf,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS value_eur,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS value_gbp
      FROM spending_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = b.currency AND fxl.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (b.occurred_at // 86400)
);

CREATE MACRO report_spending_transactions_multi(p_from, p_to) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, merchant_signature, merchant_name, spend_primary, spend_detailed,
           spend_label, spend_primary_label,
           provenance, provider_spend_detailed, provider_spend_label, currency,
           CAST(net_amount AS DECIMAL(28,4)) AS net_amount,
           description, counterparty,
           value_usd, value_chf, value_eur, value_gbp
      FROM spending_lines_multi(p_from, p_to)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

CREATE MACRO cashflow_txn_nodes(p_from, p_to) AS TABLE (
    WITH joined AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
               t.account_external_id, t.kind, t.currency, t.net_amount,
               t.description, t.counterparty, t.instrument_external_id,
               a.account_kind, a.display_name, a.nickname, a.account_category,
               -- What was TRADED, the trade's own word first. A feed
               -- that cannot name the instrument can still name the
               -- exposure — a precious-metal sale, a capital call, a
               -- subscription right — and transactions.asset_class is
               -- where it says so. Coalescing HERE rather than at each use is
               -- what makes every rule below read the same answer,
               -- the cash-is-internal test included.
               COALESCE(t.asset_class, i.asset_class) AS asset_class,
               COALESCE(i.name, i.symbol) AS instrument_name,
               sc.spend_detailed, sc.merchant_name, sc.provenance AS spend_provenance,
               ic.income_detailed, ic.payer_name, ic.provenance AS income_provenance,
               -- Where the money went: the far account the pass linked.
               fl.far_silver_source_id, fl.far_account_external_id,
               -- The holder's word for what the capital went INTO, one
               -- column per family. Read off the raw overlay, not its
               -- resolution macro: it describes a verdict rather than
               -- belonging to either family's vocabulary, and no
               -- report of theirs reads it. Safe against a
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
               -- row's source states it signed (cash_signed_kinds).
               (ck.kind IS NOT NULL
                AND kind_is_unsigned(t.kind)
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
          LEFT JOIN spend_txn_enrichment e
                 ON e.silver_source_id        = t.silver_source_id
                AND e.transaction_external_id = t.transaction_external_id
          -- The far side is a raw fact about a move rather than part of
          -- either family's vocabulary.
          LEFT JOIN transfer_links fl
                 ON fl.silver_source_id        = t.silver_source_id
                AND fl.transaction_external_id = t.transaction_external_id
          LEFT JOIN income_txn_enrichment ie
                 ON ie.silver_source_id        = t.silver_source_id
                AND ie.transaction_external_id = t.transaction_external_id
          LEFT JOIN silver_sources ss ON ss.silver_source_id = t.silver_source_id
          LEFT JOIN cash_signed_kinds ck
                 ON ck.silver_kind = ss.silver_kind AND ck.kind = t.kind
          -- A repayment linked to the outflow it answers.
          LEFT JOIN spend_reimbursements() rb
                 ON rb.silver_source_id        = t.silver_source_id
                AND rb.transaction_external_id = t.transaction_external_id
          LEFT JOIN accounts fa
                 ON fa.silver_source_id    = fl.far_silver_source_id
                AND fa.account_external_id = fl.far_account_external_id
          -- The far account's wrapper and pool membership as of the
          -- row's day: a dated wrapper override applies from its
          -- day on.
          LEFT JOIN account_wrapper_spans() fws
                 ON fws.silver_source_id    = fl.far_silver_source_id
                AND fws.account_external_id = fl.far_account_external_id
                AND t.occurred_at >= fws.from_at AND t.occurred_at < fws.until_at
          LEFT JOIN cashflow_wrapper_sides fw ON fw.tax_wrapper = fws.tax_wrapper
          LEFT JOIN cashflow_pool_spans() fp
                 ON fp.silver_source_id    = fl.far_silver_source_id
                AND fp.account_external_id = fl.far_account_external_id
                AND t.occurred_at >= fp.from_at AND t.occurred_at < fp.until_at
         WHERE t.occurred_at BETWEEN p_from AND p_to
    ),
    verdicted AS (
        SELECT j.*,
               -- A linked repayment takes the category of the outflow it
               -- answers: it nets there, as a merchant's refund does.
               CASE WHEN j.reimbursed_detailed IS NOT NULL THEN j.reimbursed_detailed
                    WHEN kind_is_income(j.kind, j.net_amount)
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
               CASE WHEN kind_is_income(j.kind, j.net_amount)
                    THEN COALESCE(j.income_stated_class, j.spend_stated_class)
                    ELSE COALESCE(j.spend_stated_class, j.income_stated_class)
               END AS stated_asset_class
          FROM joined j
    ),
    -- THE ORDERED FAR-ACCOUNT TEST (docs/CASHFLOW.md §4). First match
    -- wins, and the WRAPPER is asked before the account KIND: any
    -- account kind can sit inside any wrapper, and the wrapper is what
    -- says whose money moved.
    --
    -- A far account whose wrapper is unset falls past the wrapper arms
    -- and, unless it is a mortgage, reaches the pool test, which admits
    -- it — the same render-time default the pool macro applies, reached
    -- by the same rule rather than by accident.
    --
    -- The last two arms are where a move lands that no wrapper and no
    -- pool membership could place. A far account gold HOLDS, on the
    -- household's side, that the `cashflow.accounts` scope fenced out of
    -- the pool — a setting, and named as one. Last, no far account at
    -- all: a rule or a pin placed the verdict and nothing paired it,
    -- which under the closed world the statement assumes is a finding —
    -- a counter-leg gold should hold, or a declaration not yet written.
    -- Drawn as invisible that money would vanish into the residual
    -- forever; drawn as a node it is visible as what it is, and the
    -- remedy — collect the account, declare it, or find the missing leg
    -- — is obvious.
    far AS (
        SELECT v.*,
               CASE
                   WHEN v.far_side = 'vehicle'                     THEN 'vehicle'
                   WHEN v.far_side = 'giving' AND v.net_amount < 0  THEN 'giving_out'
                   WHEN v.far_side = 'giving'                       THEN 'giving_in'
                   WHEN v.far_account_kind = 'mortgage'             THEN 'mortgage'
                   WHEN v.far_pooled                                THEN 'internal'
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
               -- its primary and its roles say where it draws without
               -- restating the taxonomy here.
               vc.spend_primary AS verdict_primary,
               vc.flow_place AS verdict_place,
               vc.flow_class_in AS verdict_class_in,
               vc.flow_class_out AS verdict_class_out,
               vc.flow_leaf_detailed AS verdict_leaf_detailed,
               CASE
                   -- THE KINDS NO VERDICT MAY PLACE, tested ahead of
                   -- every verdict arm. gold pins no canonical sign for
                   -- these kinds (txn_kind_roles.unsigned) — the adapter
                   -- passes the source's through — so their direction is
                   -- a per-source convention, and a section that reads
                   -- direction cannot admit them. A pin or a rule may
                   -- call such a row an own-account move; it still
                   -- cannot make an unsigned amount readable. `fx` is
                   -- cash becoming cash besides, and the two catch-alls
                   -- are the exclusion both families already make.
                   --
                   -- The one exception is a cash leg its source states
                   -- signed (cash_signed_kinds): cash in lieu, a
                   -- return of capital, an unissued right's cash. It is
                   -- investing, as the trade it settles is.
                   WHEN f.cash_signed THEN
                        CASE WHEN f.asset_class = 'cash' THEN 'internal' ELSE 'investing' END
                   WHEN kind_is_unsigned(f.kind) THEN 'excluded'
                   -- A repayment linked to its outflow is that outflow's
                   -- money coming back: operating out, netted inside the
                   -- outflow's class (spend_reimbursements).
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
                   -- A verdict whose value places it, needing no far
                   -- account (spend_categories.flow_place):
                   --   mortgage       a mortgage servicer the product
                   --                  holds no account for. The two roads
                   --                  to `financing · Mortgage` both read
                   --                  the far side, and this verdict
                   --                  exists for the rows where there is
                   --                  none to read.
                   --   vehicle_delta  a crossing whose far side the
                   --                  product does not hold. The direction
                   --                  is the ROW's: a withdrawal placed
                   --                  `retirement_transfer` is a
                   --                  contribution and a deposit placed
                   --                  the same is a distribution.
                   --   loans          debt at a lender the product does
                   --                  not track, in either direction.
                   --   investing      capital deployed to, or returned
                   --                  from, a destination the product
                   --                  does not track.
                   WHEN vc.flow_place IS NOT NULL THEN vc.flow_place
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
                   WHEN kind_is_trade(f.kind) THEN
                        CASE WHEN f.asset_class = 'cash' THEN 'internal' ELSE 'investing' END
                   -- The inflow kinds, a distribution placed above.
                   WHEN kind_is_income(f.kind, f.net_amount) THEN 'in'
                   -- The outflow kinds. `refund` is here with the rest:
                   -- a merchant credit nets inside its own category, and
                   -- the class's sign is what decides which side of the
                   -- diagram it is drawn on.
                   WHEN kind_is_spending(f.kind, f.net_amount) THEN 'out'
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
                            -- Earnings, yield or benefits by the value
                            -- (spend_categories.flow_class_in). A card
                            -- credit or a referral bonus sits with the
                            -- receipts rather than the yield: it is not
                            -- income from wealth.
                            ELSE COALESCE(p.verdict_class_in, 'other_receipts')
                        END
                   WHEN 'giving_in' THEN 'other_receipts'
                   WHEN 'out' THEN
                        CASE
                            WHEN p.verdict IS NULL THEN '(uncategorized)'
                            -- Fees, taxes or giving by the value
                            -- (spend_categories.flow_class_out).
                            ELSE COALESCE(p.verdict_class_out, 'consumption')
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
                   -- transactions.asset_class means: what the FEED said.
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
                   -- The pool a crossing's value names.
                   WHEN 'vehicle_delta' THEN p.verdict_class_out
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
                   -- because those three classes stand apart from
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
                             WHEN n.verdict_leaf_detailed THEN n.verdict
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
                   -- `place` ladder already follows. A group read off
                   -- `n.class = 'elsewhere'` would fall to the ELSE the
                   -- moment a stated exposure moved a row off
                   -- `elsewhere`, and a house purchase would read
                   -- `Real estate · Private capital`. A row reaches
                   -- `investing` on another kind only through a value
                   -- that places it there, or as a cash leg its source
                   -- signs (cash_signed). A corporate action's is its
                   -- own group; every other is private capital.
                   --
                   -- The class is what the money went INTO; the group is
                   -- what the money DID. A house bought and a REIT traded
                   -- share a class node and differ at group grain.
                   WHEN 'investing' THEN
                        CASE WHEN n.verdict_place = 'investing'
                                  THEN n.verdict
                             WHEN n.kind IN ('buy', 'sell') THEN 'trades'
                             WHEN n.kind = 'corporate_action' THEN 'corporate_actions'
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

-- cashflow_pool_nodes: the nodes of the rows whose own account is in
-- the pool on the row's own day (cashflow_pool_spans), so a row on a
-- dated wrapper override's day reads the override's wrapper. What the
-- statement's lines, the projection's look-back and `status` count.
CREATE MACRO cashflow_pool_nodes(p_from, p_to) AS TABLE (
    SELECT n.*
      FROM cashflow_txn_nodes(p_from, p_to) n
      JOIN cashflow_pool_spans() p
             ON p.silver_source_id    = n.silver_source_id
            AND p.account_external_id = n.account_external_id
            AND n.occurred_at >= p.from_at AND n.occurred_at < p.until_at
);

-- ============================================================
-- report_transactions: the ledger, and what a trade was looked up BY.
--
-- transactions.instrument_hint holds the token an adapter resolved an
-- instrument from when that resolution failed. This macro puts it on
-- the one surface a person reads a transaction from, behind `-C`, so
-- the set still to close is a query rather than a per-feed payload
-- excavation:
--
--     wealthdb transactions - today -C +instrument_hint -f json
--
-- and the entries that close it go in `transaction_instruments`, keyed
-- by exactly this value.
--
-- The hint sits after `check_number` and before the cashflow trio,
-- because the Go reader scans this macro POSITIONALLY — the projection,
-- the row struct and the scan list move together or not at all.
-- ============================================================

CREATE MACRO report_transactions(p_from, p_to, p_ccy) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.account_kind, a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description,
               sc.merchant_name, sc.spend_primary, sc.spend_detailed,
               ic.payer_name, ic.income_primary, ic.income_detailed,
               t.check_number, t.instrument_hint,
               cf.section AS cashflow_section, cf.class AS cashflow_class, cf.grp AS cashflow_group
          FROM transactions t
          LEFT JOIN accounts a ON t.silver_source_id = a.silver_source_id AND t.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON t.silver_source_id = i.silver_source_id AND t.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = t.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = t.instrument_external_id
          LEFT JOIN symbol_resolutions srn ON srn.silver_source_id = t.silver_source_id
               AND srn.lookup_kind = 'name' AND srn.lookup_value = t.description
          LEFT JOIN spend_txn_categories() sc ON sc.silver_source_id = t.silver_source_id
               AND sc.transaction_external_id = t.transaction_external_id
          LEFT JOIN income_txn_categories() ic ON ic.silver_source_id = t.silver_source_id
               AND ic.transaction_external_id = t.transaction_external_id
          LEFT JOIN cashflow_txn_nodes(p_from, p_to) cf ON cf.silver_source_id = t.silver_source_id
               AND cf.transaction_external_id = t.transaction_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.account_kind, b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS VARCHAR) AS gross_amount, CAST(b.net_amount AS VARCHAR) AS net_amount,
           CAST(b.quantity AS VARCHAR) AS quantity, CAST(b.price AS VARCHAR) AS price, b.description,
           b.merchant_name, b.spend_primary, b.spend_detailed,
           b.payer_name, b.income_primary, b.income_detailed,
           b.check_number, b.instrument_hint,
           b.cashflow_section, b.cashflow_class, b.cashflow_group,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * d.rate, b.net_amount::DOUBLE * c1.rate * c2.rate,
               b.net_amount::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy
      FROM base b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy = b.currency AND d.to_ccy = p_ccy AND d.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = p_ccy AND c2.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = p_ccy AND u2.day <= (b.occurred_at // 86400)
     ORDER BY b.occurred_at, b.silver_source_id, b.transaction_external_id
);

CREATE MACRO cashflow_lines_base(p_from, p_to) AS TABLE (
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
          FROM cashflow_pool_nodes(p_from, p_to) n
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

-- The four cash flow reports, and the three cashflow columns on
-- `wealthdb transactions`.
--
-- They follow the spending and income reports: same FX pair, same
-- period bucketing, same n_converted guard, same positive-magnitude
-- idiom. What is their own is the netting — a node is a net, the level
-- decides what nets, and the sign decides which side of the diagram it
-- is drawn on — and the edge list that netting makes possible.
--
-- The FX blocks are duplicated rather than shared because a DuckDB
-- table macro cannot take a table as a parameter, which is the same
-- reason the spending pair duplicates them.
--
-- THE SIGN RULE, once, for every report below: positive is cash
-- arriving in the pool and negative is cash leaving it. Magnitude
-- columns (`income`, `spending`, `inflow`, `outflow`) are positive, as
-- the families' spend and refund columns are; `net` and the section
-- columns are signed.

-- ============================================================
-- The FX pair: the base with its amounts converted.
-- ============================================================

CREATE MACRO cashflow_lines_outccy(p_from, p_to, p_ccy) AS TABLE (
    SELECT b.*,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * d.rate, b.net_amount::DOUBLE * c1.rate * c2.rate,
               b.net_amount::DOUBLE * u1.rate * u2.rate) AS DECIMAL(28,4)) AS value_outccy
      FROM cashflow_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy  = b.currency AND d.to_ccy  = p_ccy AND d.day  <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= (b.occurred_at // 86400)
);

-- ============================================================
-- cashflow_nodes_outccy: the lines, converted, with the investing
-- grain applied.
--
-- The one place `--investing` is honoured. Under `whole`, the default,
-- every asset class folds into one node and the groups fold with it:
-- a class on the other side cannot be drawn as a child of a node on
-- this one, so "as a whole" means as a whole. Under `class` the fold is
-- a no-op and the section opens into one node per asset class, which is
-- the step in — it shows the reallocation between classes that the
-- whole nets away.
--
-- Shared by the flows view and the diagram, which differ in how they
-- aggregate it and not in what they aggregate.
-- ============================================================
CREATE MACRO cashflow_nodes_outccy(p_from, p_to, p_ccy, p_investing) AS TABLE (
    WITH folded AS (
        SELECT b.*,
               CASE WHEN b.section = 'investing' AND p_investing = 'whole'
                    THEN 'investments' ELSE b.class END AS node_class,
               CASE WHEN b.section = 'investing' AND p_investing = 'whole'
                    THEN 'investments' ELSE b.grp END AS node_group
          FROM cashflow_lines_outccy(p_from, p_to, p_ccy) b
    )
    SELECT f.* EXCLUDE (class_label, group_label),
           cashflow_class_label(f.node_class) AS node_class_label,
           CASE WHEN f.node_group = f.grp THEN f.group_label
                ELSE cashflow_class_label(f.node_group) END AS node_group_label
      FROM folded f
);

-- ============================================================
-- report_cashflow_transactions: a cashflow line, oldest first.
--
-- The family transactions views' shape with the node resolved. `name`
-- is the instrument on an investing line and the payer or merchant on
-- an operating line; a financing, vehicle or cash line names nothing,
-- because its counterparty is an ACCOUNT and no surface of this feature
-- prints an account as a name.
--
-- The projection order is a contract: gold.CashflowTransactionRow scans
-- this macro POSITIONALLY, so the macro, the row struct and the scan
-- list move together in one commit.
-- ============================================================
CREATE MACRO report_cashflow_transactions(p_from, p_to, p_ccy) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, section, class, class_label, grp, group_label, name,
           verdict, spend_detailed, income_detailed, provenance, currency,
           CAST(net_amount AS VARCHAR) AS net_amount,
           description, counterparty,
           CAST(value_outccy AS VARCHAR) AS value_outccy
      FROM cashflow_lines_outccy(p_from, p_to, p_ccy)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

-- THE LABEL HAS ONE HOME. `cashflow_class_label` is the macro that
-- names a class, and these two reports ASK it for the residual's name
-- rather than spell it as a string literal. A copy would rename the node
-- on the statement and leave it unchanged on the diagram and the flows.

CREATE MACRO report_cashflow_sankey(p_from, p_to, p_ccy, p_level, p_investing) AS TABLE (
    WITH v AS (SELECT * FROM cashflow_nodes_outccy(p_from, p_to, p_ccy, p_investing)),
    -- The class column of every section, plus the residual.
    cls AS (
        SELECT section, node_class AS class, node_class_label AS label,
               CAST(SUM(value_outccy) AS DECIMAL(28,4)) AS net
          FROM v GROUP BY 1, 2, 3
        UNION ALL
        SELECT 'cash', 'cash', cashflow_class_label('cash'),
               CAST(-COALESCE(SUM(value_outccy), 0) AS DECIMAL(28,4)) FROM v),
    -- The self-edge rule is stated as itself (`node_group <>
    -- node_class`) rather than left to a list of sections to imply. A
    -- section list would do two jobs and the second silently: cash and
    -- the vehicles have nothing finer to say, so excluding them BY
    -- SECTION looks equivalent to excluding them for repeating their
    -- class — but it would also exclude financing, which HAS something
    -- finer to say, so a split mortgage would never draw. With the
    -- guards separate, a section gains its leaf stage the day its groups
    -- stop repeating its classes.
    --
    -- Investing stays out deliberately. Its groups do not repeat its
    -- classes, so the self-edge rule alone would give it a leaf stage —
    -- but what that stage would say (a trade against a capital call) is
    -- a different question from this one.
    --
    -- The leaves: the operating sections and financing, less the backlog. The
    -- backlog class IS its own leaf — nothing placed those rows, so
    -- there is nothing finer to say about them — and a class drawn into
    -- a leaf of the same name is a self-edge, which a Sankey renders as
    -- a node pointing at itself. It attaches to the hub directly
    -- instead, the way cash and the vehicles do.
    leaf AS (
        SELECT section, node_class AS class, node_group AS grp, node_group_label AS label,
               CAST(SUM(value_outccy) AS DECIMAL(28,4)) AS net
          FROM v
         WHERE section IN ('operating_in', 'operating_out', 'financing')
           AND node_class <> '(uncategorized)'
           AND node_group <> node_class
         GROUP BY 1, 2, 3, 4),
    -- A node's printable name, disambiguated where the same value
    -- reaches both operating sections.
    named_cls AS (
        SELECT c.*, CASE
                   WHEN c.section = 'operating_in'  AND (c.class = '(uncategorized)' OR sc.family = 'both')
                        THEN c.label || ' in'
                   WHEN c.section = 'operating_out' AND (c.class = '(uncategorized)' OR sc.family = 'both')
                        THEN c.label || ' out'
                   ELSE c.label END AS node_name,
               c.section || '.' || c.class AS node_id
          FROM cls c LEFT JOIN spend_categories sc ON sc.spend_detailed = c.class),
    named_leaf AS (
        SELECT l.*, CASE
                   WHEN l.section = 'operating_in'  AND (l.grp = '(uncategorized)' OR sg.family = 'both')
                        THEN l.label || ' in'
                   WHEN l.section = 'operating_out' AND (l.grp = '(uncategorized)' OR sg.family = 'both')
                        THEN l.label || ' out'
                   ELSE l.label END AS node_name,
               l.section || '.' || l.class || '.' || l.grp AS node_id
          FROM leaf l LEFT JOIN spend_categories sg ON sg.spend_detailed = l.grp),
    -- A leaf stays with its class when the two run the same way.
    attached AS (
        SELECT l.*, c.net AS class_net, c.node_name AS class_name, c.node_id AS class_id,
               ((l.net > 0 AND c.net > 0) OR (l.net < 0 AND c.net < 0)) AS stays
          FROM named_leaf l
          JOIN named_cls c ON c.section = l.section AND c.class = l.class),
    -- The atomic nodes: what the hub is summed over, and what attaches
    -- to it. At class grain every class is atomic; at group grain the
    -- operating classes give way to their leaves.
    atoms AS (
        -- A class enters the hub in its own right exactly when it has
        -- no leaves to enter through. A section list here would stop
        -- agreeing with the leaf rule the moment a section grows leaves:
        -- the class would enter BOTH here and as the sum of its leaves
        -- below, and the diagram would draw the same edge twice. Asking
        -- `attached` directly cannot drift from it.
        SELECT c.node_id, c.node_name, c.section, c.net FROM named_cls c
         WHERE p_level = 'class'
            OR NOT EXISTS (SELECT 1 FROM attached a
                            WHERE a.section = c.section AND a.class = c.class)
        UNION ALL
        SELECT node_id, node_name, section,
               CASE WHEN stays THEN CAST(0 AS DECIMAL(28,4)) ELSE net END
          FROM attached WHERE p_level = 'group'
        UNION ALL
        -- A class whose leaves stayed with it enters the hub as the sum
        -- of those leaves, which is its net widened by whatever left.
        SELECT class_id, class_name, section,
               CAST(SUM(CASE WHEN stays THEN net ELSE 0 END) AS DECIMAL(28,4))
          FROM attached WHERE p_level = 'group'
         GROUP BY 1, 2, 3),
    hub AS (SELECT SUM(CASE WHEN net > 0 THEN net ELSE 0 END) AS total FROM atoms),
    edges AS (
        -- Stages 2 and 3: every atomic node, to or from the household.
        SELECT CASE WHEN a.net > 0 THEN 2 ELSE 3 END AS stage,
               CASE WHEN a.net > 0 THEN a.node_name ELSE 'Household' END AS source,
               CASE WHEN a.net > 0 THEN 'Household' ELSE a.node_name END AS target,
               CASE WHEN a.net > 0 THEN a.node_id ELSE 'household' END AS source_id,
               CASE WHEN a.net > 0 THEN 'household' ELSE a.node_id END AS target_id,
               a.section, ABS(a.net) AS value
          FROM atoms a WHERE a.net <> 0
        UNION ALL
        -- Stages 1 and 4: the operating leaves that stayed with their
        -- class, drawn outside it on whichever side the class sits.
        SELECT CASE WHEN a.class_net > 0 THEN 1 ELSE 4 END,
               CASE WHEN a.class_net > 0 THEN a.node_name ELSE a.class_name END,
               CASE WHEN a.class_net > 0 THEN a.class_name ELSE a.node_name END,
               CASE WHEN a.class_net > 0 THEN a.node_id ELSE a.class_id END,
               CASE WHEN a.class_net > 0 THEN a.class_id ELSE a.node_id END,
               a.section, ABS(a.net)
          FROM attached a WHERE p_level = 'group' AND a.stays AND a.net <> 0)
    SELECT e.stage, e.source, e.target, e.source_id, e.target_id, e.section,
           CAST(CAST(e.value AS DECIMAL(28,4)) AS VARCHAR) AS value,
           e.value::DOUBLE / NULLIF(h.total::DOUBLE, 0) AS share
      FROM edges e CROSS JOIN hub h
     ORDER BY e.stage, e.value DESC, e.source, e.target
);

CREATE MACRO report_cashflow_flows(p_from, p_to, p_ccy, p_period, p_level, p_investing) AS TABLE (
    WITH v AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               section,
               CASE WHEN p_level = 'section' THEN NULL ELSE node_class END AS class,
               CASE WHEN p_level = 'section' THEN NULL ELSE node_class_label END AS class_label,
               CASE WHEN p_level = 'group' THEN node_group END AS grp,
               CASE WHEN p_level = 'group' THEN node_group_label END AS group_label,
               value_outccy
          FROM cashflow_nodes_outccy(p_from, p_to, p_ccy, p_investing)),
    agg AS (
        SELECT period_start, section, class, class_label, grp, group_label,
               COUNT(*)            AS txn_count,
               COUNT(value_outccy) AS n_converted,
               CAST(SUM(CASE WHEN value_outccy > 0 THEN  value_outccy ELSE 0 END) AS DECIMAL(28,4)) AS inflow_val,
               CAST(SUM(CASE WHEN value_outccy < 0 THEN -value_outccy ELSE 0 END) AS DECIMAL(28,4)) AS outflow_val,
               CAST(SUM(value_outccy) AS DECIMAL(28,4)) AS net_val
          FROM v
         GROUP BY 1, 2, 3, 4, 5, 6),
    -- One cash node per bucket, whatever the level: the residual has
    -- one class and one group as it has one node.
    cash AS (
        SELECT t.period_start, 'cash' AS section,
               CASE WHEN p_level = 'section' THEN NULL ELSE 'cash' END AS class,
               CASE WHEN p_level = 'section' THEN NULL
                    ELSE cashflow_class_label('cash') END AS class_label,
               CASE WHEN p_level = 'group' THEN 'cash' END AS grp,
               CASE WHEN p_level = 'group'
                    THEN cashflow_class_label('cash') END AS group_label,
               CAST(0 AS BIGINT) AS txn_count, CAST(1 AS BIGINT) AS n_converted,
               CAST(NULL AS DECIMAL(28,4)) AS inflow_val,
               CAST(NULL AS DECIMAL(28,4)) AS outflow_val,
               CAST(-t.total AS DECIMAL(28,4)) AS net_val
          FROM (SELECT period_start, SUM(net_val) AS total FROM agg GROUP BY 1) t),
    rows_all AS (SELECT * FROM agg UNION ALL SELECT * FROM cash)
    SELECT period_start, section, class, class_label, grp, group_label, txn_count,
           CAST(CASE WHEN n_converted = 0 THEN NULL ELSE inflow_val  END AS VARCHAR) AS inflow,
           CAST(CASE WHEN n_converted = 0 THEN NULL ELSE outflow_val END AS VARCHAR) AS outflow,
           CAST(CASE WHEN n_converted = 0 THEN NULL ELSE net_val     END AS VARCHAR) AS net,
           ABS(net_val)::DOUBLE
               / NULLIF(SUM(CASE WHEN net_val > 0 THEN net_val ELSE 0 END)
                            OVER (PARTITION BY period_start), 0) AS share
      FROM rows_all
     ORDER BY period_start,
              cashflow_section_rank(section),
              cashflow_class_rank(class), ABS(net_val) DESC, class, grp
);

CREATE MACRO cashflow_lines_multi(p_from, p_to) AS TABLE (
    SELECT b.*,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS value_usd,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS value_chf,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS value_eur,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS value_gbp
      FROM cashflow_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = b.currency AND fxl.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (b.occurred_at // 86400)
);

CREATE MACRO report_cashflow_transactions_multi(p_from, p_to) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, section, class, class_label, grp, group_label, name,
           verdict, spend_detailed, income_detailed, provenance, currency,
           CAST(net_amount AS DECIMAL(28,4)) AS net_amount,
           description, counterparty,
           value_usd, value_chf, value_eur, value_gbp
      FROM cashflow_lines_multi(p_from, p_to)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

-- ============================================================
-- cashflow_cash_balances: the memo sees a deposit stated as a holding.
--
-- The reconciliation memo and the coverage report measure an account
-- by its cash balances, so an account whose source states none is
-- invisible to both. A deposit family read from statements states the
-- account's balance as a holding: a `cash` position of vehicle
-- `demand_deposit`, which net worth already counts. Restating it as a
-- cash balance would count it twice.
--
-- cashflow_cash_balances() is cash_balances plus, for an account with
-- no cash balance at all, its demand-deposit holdings, summed per
-- snapshot and currency, as its `current` balance. The guard is
-- account-wide on purpose: an account with any cash balance states its
-- cash there, and a deposit sleeve beside it is a holding. A snapshot
-- with no deposit holding carries the last balance forward, as a
-- statement that lost its table does. The three balance readers of cash
-- flow read this macro rather than the table: cashflow_balance_pool,
-- report_cashflow_reconciliation and report_cashflow_coverage.
-- ============================================================

CREATE MACRO cashflow_cash_balances() AS TABLE (
    SELECT silver_source_id, snapshot_at, account_external_id, currency,
           balance_kind, amount
      FROM cash_balances
    UNION ALL
    SELECT p.silver_source_id, p.snapshot_at, p.account_external_id, p.currency,
           'current' AS balance_kind, SUM(p.market_value) AS amount
      FROM positions p
     WHERE p.asset_class = 'cash' AND p.vehicle = 'demand_deposit'
       AND p.market_value IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM cash_balances c
                        WHERE c.silver_source_id    = p.silver_source_id
                          AND c.account_external_id = p.account_external_id)
     GROUP BY 1, 2, 3, 4
);

CREATE MACRO cashflow_balance_pool() AS TABLE (
    SELECT p.silver_source_id, p.account_external_id
      FROM cashflow_pool_accounts() p
     WHERE p.account_kind IS DISTINCT FROM 'overlay'
       AND EXISTS (SELECT 1 FROM transactions t
                    WHERE t.silver_source_id    = p.silver_source_id
                      AND t.account_external_id = p.account_external_id)
       AND EXISTS (SELECT 1 FROM cashflow_cash_balances() c
                    WHERE c.silver_source_id    = p.silver_source_id
                      AND c.account_external_id = p.account_external_id)
);

CREATE MACRO report_cashflow_reconciliation(p_from, p_to, p_ccy, p_period) AS TABLE (
    WITH pool AS (SELECT * FROM cashflow_balance_pool()),
    -- The pinned balance, per (account, currency, snapshot). No
    -- non-zero filter: a balance that went to zero is a fact.
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
                  FROM cashflow_cash_balances() cb
                  JOIN pool p ON p.silver_source_id    = cb.silver_source_id
                             AND p.account_external_id = cb.account_external_id)
         WHERE rn = 1),
    first_obs AS (
        SELECT silver_source_id, account_external_id,
               MIN(snapshot_at) // 86400 AS first_day
          FROM chosen GROUP BY 1, 2),
    -- WHAT MOVES THE SPINE. The flow side is every row on a measured
    -- account that the resolution did NOT decline — its lines, and the
    -- movements it calls pool-internal.
    --
    -- The internal ones belong here even though the statement never
    -- draws them, and leaving them out is what a first reading gets
    -- wrong. The spine is narrower than the household: a wire to a
    -- pooled account whose balances are not collected leaves the
    -- MEASURED pool while staying inside the household's, and a
    -- money-market fund bought with idle cash leaves the cash balances
    -- for a position. Both move the spine and neither is a line. Where
    -- both ends of a move are measured, both legs are here and they net
    -- to zero, which is what makes one rule serve every case.
    --
    -- What is deliberately NOT here is the rows the resolution
    -- DECLINED: the FX family, corporate actions, an unpaired card bill
    -- or in-kind leg, the catch-all kinds. Each moves the spine with
    -- nothing to explain it, and showing up in `unexplained` is exactly
    -- what the memo is for.
    --
    -- The flow side is also restricted to the same accounts and the
    -- same slice as the balance side. A diagnostic whose two halves
    -- covered different populations would carry a structural term for
    -- every account it cannot measure, and would stop saying anything
    -- about whether the statement's account of the cash is complete.
    raw_lines AS (
        SELECT spend_period_bucket(p_period, n.occurred_at) AS period_start,
               n.silver_source_id, n.account_external_id,
               n.occurred_at // 86400 AS day, n.currency, n.net_amount
          FROM cashflow_txn_nodes(p_from, p_to) n
          JOIN pool p ON p.silver_source_id    = n.silver_source_id
                     AND p.account_external_id = n.account_external_id
         WHERE n.disposition IN ('line', 'internal')),
    lines AS (
        SELECT r.*,
               CAST(COALESCE(CASE WHEN r.currency = p_ccy THEN r.net_amount::DOUBLE END,
                   r.net_amount::DOUBLE * d0.rate, r.net_amount::DOUBLE * c1.rate * c2.rate,
                   r.net_amount::DOUBLE * u1.rate * u2.rate) AS DECIMAL(28,4)) AS value_outccy
          FROM raw_lines r
          ASOF LEFT JOIN fx_daily d0 ON d0.from_ccy = r.currency AND d0.to_ccy = p_ccy AND d0.day <= r.day
          ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = r.currency AND c1.to_ccy = 'CHF' AND c1.day <= r.day
          ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= r.day
          ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = r.currency AND u1.to_ccy = 'USD' AND u1.day <= r.day
          ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= r.day),
    -- The buckets the STATEMENT reports, so the memo sits beside it
    -- rather than inventing a bucket of its own.
    buckets AS (
        SELECT DISTINCT spend_period_bucket(p_period, occurred_at) AS period_start
          FROM cashflow_lines_base(p_from, p_to)),
    bday AS (
        SELECT period_start,
               (GREATEST(COALESCE(period_start, p_from), p_from) - 1) // 86400 AS open_day,
               cashflow_period_end(p_period, period_start, p_to) // 86400 AS close_day
          FROM buckets),
    -- An account's own opening boundary: the later of the bucket's and
    -- its first observation. An account whose balances have not started
    -- by the bucket's end is not in it at all.
    aopen AS (
        SELECT b.period_start, f.silver_source_id, f.account_external_id,
               GREATEST(b.open_day, f.first_day) AS open_day, b.close_day
          FROM bday b JOIN first_obs f ON f.first_day <= b.close_day),
    acur AS (SELECT DISTINCT silver_source_id, account_external_id, currency FROM chosen),
    edges AS (
        SELECT a.period_start, 'open' AS side, a.open_day AS day,
               a.silver_source_id, a.account_external_id
          FROM aopen a
        UNION ALL
        SELECT a.period_start, 'close', a.close_day,
               a.silver_source_id, a.account_external_id
          FROM aopen a),
    -- One balance and one rate per (bucket, boundary, account,
    -- currency). The rate is triangulated the way every other report
    -- triangulates it.
    valued AS (
        SELECT g.period_start, g.side, g.silver_source_id, g.account_external_id, g.currency,
               COALESCE(c.amount, 0) AS amount,
               COALESCE(CASE WHEN g.currency = p_ccy THEN 1.0 END,
                        d0.rate, c1.rate * c2.rate, u1.rate * u2.rate) AS rate
          FROM (SELECT e.period_start, e.side, e.day,
                       e.silver_source_id, e.account_external_id, u.currency
                  FROM edges e
                  JOIN acur u ON u.silver_source_id    = e.silver_source_id
                             AND u.account_external_id = e.account_external_id) g
          ASOF LEFT JOIN chosen c
                 ON c.silver_source_id    = g.silver_source_id
                AND c.account_external_id = g.account_external_id
                AND c.currency            = g.currency
                AND c.snapshot_at        <= g.day * 86400 + 86399
          ASOF LEFT JOIN fx_daily d0 ON d0.from_ccy = g.currency AND d0.to_ccy = p_ccy AND d0.day <= g.day
          ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = g.currency AND c1.to_ccy = 'CHF' AND c1.day <= g.day
          ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= g.day
          ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = g.currency AND u1.to_ccy = 'USD' AND u1.day <= g.day
          ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= g.day),
    paired AS (
        SELECT period_start, silver_source_id, account_external_id, currency,
               MAX(CASE WHEN side = 'open'  THEN amount END) AS open_amount,
               MAX(CASE WHEN side = 'close' THEN amount END) AS close_amount,
               MAX(CASE WHEN side = 'open'  THEN rate   END) AS open_rate,
               MAX(CASE WHEN side = 'close' THEN rate   END) AS close_rate
          FROM valued GROUP BY 1, 2, 3, 4),
    balance_terms AS (
        SELECT period_start,
               SUM(close_amount::DOUBLE * close_rate) - SUM(open_amount::DOUBLE * open_rate) AS cash_measured,
               SUM(open_amount::DOUBLE * (close_rate - open_rate)) AS fx_opening,
               COUNT(*) FILTER (WHERE open_rate IS NULL OR close_rate IS NULL) AS unrated
          FROM paired GROUP BY 1),
    -- The bucket's closing rate per currency: the day is the bucket's
    -- for every account, so one rate serves them all.
    close_rates AS (
        SELECT g.period_start, g.currency,
               COALESCE(CASE WHEN g.currency = p_ccy THEN 1.0 END,
                        d0.rate, c1.rate * c2.rate, u1.rate * u2.rate) AS rate
          FROM (SELECT b.period_start, b.close_day AS day, c.currency
                  FROM bday b
                  CROSS JOIN (SELECT DISTINCT currency FROM lines
                              UNION SELECT DISTINCT currency FROM chosen) c) g
          ASOF LEFT JOIN fx_daily d0 ON d0.from_ccy = g.currency AND d0.to_ccy = p_ccy AND d0.day <= g.day
          ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = g.currency AND c1.to_ccy = 'CHF' AND c1.day <= g.day
          ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= g.day
          ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = g.currency AND u1.to_ccy = 'USD' AND u1.day <= g.day
          ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= g.day),
    -- The lines inside each account's own measured slice, revalued from
    -- their own day's rate to the bucket's closing one.
    flow_terms AS (
        SELECT l.period_start,
               SUM(l.net_amount::DOUBLE * r.rate) - SUM(l.value_outccy::DOUBLE) AS fx_lines,
               SUM(l.value_outccy::DOUBLE) AS net_flow,
               COUNT(*) FILTER (WHERE l.value_outccy IS NULL OR r.rate IS NULL) AS unrated
          FROM lines l
          JOIN aopen a ON a.period_start IS NOT DISTINCT FROM l.period_start
                      AND a.silver_source_id    = l.silver_source_id
                      AND a.account_external_id = l.account_external_id
                      AND l.day > a.open_day
          JOIN close_rates r ON r.period_start IS NOT DISTINCT FROM l.period_start
                            AND r.currency = l.currency
         GROUP BY 1),
    memo AS (
        SELECT b.period_start,
               b.cash_measured,
               b.fx_opening + COALESCE(f.fx_lines, 0) AS fx_effect,
               -- The flow term the residual is taken against, published
               -- rather than left implicit: it is the flow over the
               -- MEASURED slice, internal moves included, and so is not
               -- the statement's net_cash_flow. Without it the four
               -- printed columns do not close and a reader doing the
               -- arithmetic finds a residual that is neither zero nor
               -- `unexplained` (docs/CASHFLOW.md §8).
               COALESCE(f.net_flow, 0) AS cash_flow_measured,
               b.cash_measured - (b.fx_opening + COALESCE(f.fx_lines, 0))
                   - COALESCE(f.net_flow, 0) AS unexplained,
               -- Blank rather than print a figure no rate could value.
               (b.unrated = 0 AND COALESCE(f.unrated, 0) = 0) AS usable
          FROM balance_terms b
          LEFT JOIN flow_terms f ON f.period_start IS NOT DISTINCT FROM b.period_start)
    SELECT period_start,
           CAST(CASE WHEN usable THEN CAST(cash_measured      AS DECIMAL(28,4)) END AS VARCHAR) AS cash_measured,
           CAST(CASE WHEN usable THEN CAST(fx_effect          AS DECIMAL(28,4)) END AS VARCHAR) AS fx_effect,
           CAST(CASE WHEN usable THEN CAST(cash_flow_measured AS DECIMAL(28,4)) END AS VARCHAR) AS cash_flow_measured,
           CAST(CASE WHEN usable THEN CAST(unexplained        AS DECIMAL(28,4)) END AS VARCHAR) AS unexplained
      FROM memo
     ORDER BY 1
);

-- ============================================================
-- report_cashflow_summary, with the memo trio.
--
-- The memo's three columns are joined on the bucket and come after
-- `savings_rate`. The projection order is a contract:
-- gold.CashflowSummaryRow scans this macro POSITIONALLY.
--
-- A LEFT join, not a FULL one, and the difference from income's
-- withholding memo is the point: the memo is computed FOR the buckets
-- the statement reports and has no population of its own, so there is
-- no bucket it could contribute that the statement does not already
-- have. IS NOT DISTINCT FROM, not `=`, because `--period total`
-- collapses the window into one bucket whose period_start is NULL on
-- both sides.
-- ============================================================
CREATE MACRO report_cashflow_summary(p_from, p_to, p_ccy, p_period) AS TABLE (
    WITH agg AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COUNT(*)            AS txn_count,
               COUNT(value_outccy) AS n_converted,
               SUM(CASE WHEN section = 'operating_in'  THEN value_outccy ELSE 0 END) AS income_raw,
               SUM(CASE WHEN section = 'operating_out' THEN value_outccy ELSE 0 END) AS spending_raw,
               SUM(CASE WHEN section = 'investing'     THEN value_outccy ELSE 0 END) AS investing_raw,
               SUM(CASE WHEN section = 'financing'     THEN value_outccy ELSE 0 END) AS financing_raw,
               SUM(CASE WHEN section = 'vehicles'      THEN value_outccy ELSE 0 END) AS vehicles_raw,
               SUM(CASE WHEN class   = 'yield'  THEN value_outccy ELSE 0 END) AS yield_raw,
               SUM(CASE WHEN class   = 'taxes'  THEN value_outccy ELSE 0 END) AS taxes_raw,
               SUM(CASE WHEN class   = 'fees'   THEN value_outccy ELSE 0 END) AS fees_raw,
               SUM(CASE WHEN class   = 'giving' THEN value_outccy ELSE 0 END) AS giving_raw
          FROM cashflow_lines_outccy(p_from, p_to, p_ccy)
         GROUP BY 1),
    vals AS (
        SELECT period_start, txn_count,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE  income_raw    END AS DECIMAL(28,4)) AS income_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE -spending_raw  END AS DECIMAL(28,4)) AS spending_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE  investing_raw END AS DECIMAL(28,4)) AS investing_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE  financing_raw END AS DECIMAL(28,4)) AS financing_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE  vehicles_raw  END AS DECIMAL(28,4)) AS vehicles_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE  yield_raw     END AS DECIMAL(28,4)) AS yield_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE -taxes_raw     END AS DECIMAL(28,4)) AS taxes_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE -fees_raw      END AS DECIMAL(28,4)) AS fees_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE -giving_raw    END AS DECIMAL(28,4)) AS giving_val
          FROM agg)
    SELECT v.period_start, v.txn_count,
           CAST(v.income_val    AS VARCHAR) AS operating_in,
           CAST(v.spending_val  AS VARCHAR) AS operating_out,
           CAST(v.income_val - v.spending_val AS VARCHAR) AS operating,
           CAST(v.investing_val AS VARCHAR) AS investing,
           CAST(v.financing_val AS VARCHAR) AS financing,
           CAST(v.vehicles_val  AS VARCHAR) AS vehicles,
           CAST(v.income_val - v.spending_val + v.investing_val + v.financing_val + v.vehicles_val
                AS VARCHAR) AS net_cash_flow,
           CAST(v.yield_val  AS VARCHAR) AS yield,
           CAST(v.taxes_val  AS VARCHAR) AS taxes,
           CAST(v.fees_val   AS VARCHAR) AS fees,
           CAST(v.giving_val AS VARCHAR) AS giving,
           -- Blank rather than infinite where nothing came in: a rate
           -- over zero income says nothing a reader can act on.
           (v.income_val - v.spending_val)::DOUBLE / NULLIF(v.income_val::DOUBLE, 0) AS savings_rate,
           m.cash_measured, m.fx_effect, m.cash_flow_measured, m.unexplained
      FROM vals v
      LEFT JOIN report_cashflow_reconciliation(p_from, p_to, p_ccy, p_period) m
             ON m.period_start IS NOT DISTINCT FROM v.period_start
     ORDER BY 1
);

CREATE MACRO report_cashflow_coverage(p_from, p_to, p_period) AS TABLE (
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
                  FROM cashflow_cash_balances() cb
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
    -- (cash_signed_kinds) moves cash and is in the ledger.
    unsigned AS (
        SELECT t.*,
               (kind_is_unsigned(t.kind)
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

-- fee_group: a BANK_FEES detailed value's group, the fee view's split.
CREATE MACRO fee_group(p_detailed) AS
    CASE WHEN p_detailed = 'BANK_FEES_INVESTMENT_FEES' THEN 'investment'
         WHEN p_detailed = 'BANK_FEES_ATM_FEES' THEN 'atm'
         WHEN p_detailed IN ('BANK_FEES_OTHER_BANK_FEES', 'BANK_FEES_OVERDRAFT_FEES',
                             'BANK_FEES_INSUFFICIENT_FUNDS') THEN 'bank'
         ELSE 'other' END;
