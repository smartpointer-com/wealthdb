-- The income overlay: the tables that carry an income type, and the
-- layered macros that define what is income at all — and, at the end,
-- the one change here that moves an existing answer: the `reward` kind
-- leaves the spending population.
--
-- This is migration 0041 read in the other direction, and it is
-- deliberately written to diff against it: same three tables, same
-- layering, same resolution shape. Where the two differ there is a
-- reason, and the reason is in a comment here.
--
--   income_txn_enrichment    per-transaction, source-scoped, rewritten
--                            by every enrichment pass — the same pass
--                            that writes spend_txn_enrichment, in the
--                            same transaction. Holds the payer
--                            signature computed for the row and, when a
--                            tier could place it, the type. `reset
--                            <source>` clears it.
--   income_payer_categories  GLOBAL, keyed by payer signature alone. A
--                            payer is the same payer whichever account
--                            received from it, so a verdict paid for
--                            once is not bought again per source.
--                            Separate from spend_merchant_categories
--                            though the signature is the same key: one
--                            counterparty can be both a merchant and a
--                            payer — a shop that also refunds by
--                            transfer, a company that is both employer
--                            and supplier — and the two questions have
--                            two answers.
--   income_account_scope     which accounts income counts, as overrides
--                            of an include-everything default. Its own
--                            scope, not spending's: an account excluded
--                            from spending because its outflows
--                            double-count something is not thereby an
--                            account whose inflows are not income.
--
-- Two things the spending side has are deliberately absent, one
-- column and one macro. `merchant_label` is the card rule's alone — the issuer a card bill
-- was paid to — and has no income analogue. And there is no income
-- matcher pool: the internal-transfer matcher already admits `deposit`
-- on every account (migration 0044) precisely so a funding wire pairs,
-- and already writes `internal_transfer` on both legs. Income READS
-- those verdicts; it pairs nothing. Two matchers with two bandings
-- would call the same wire internal on one side and external on the
-- other.
--
-- `provider_income_detailed` is here from the start rather than added
-- later as 0057 added its spending twin: the provider tier is part of
-- the lattice on both sides, and a table created now has no reason to
-- arrive incomplete.
--
-- IF NOT EXISTS / OR REPLACE throughout keep this replayable for the
-- DDL-rerun test (see gold.Migrate's REPLAY note).

CREATE TABLE IF NOT EXISTS income_txn_enrichment (
    silver_source_id         TEXT    NOT NULL,
    transaction_external_id  TEXT    NOT NULL,
    payer_signature          TEXT,
    signature_version        INTEGER NOT NULL,
    income_detailed          TEXT,
    provenance               TEXT    NOT NULL CHECK (provenance IN (
        'matcher', 'rule', 'provider', 'signature-only', 'manual'
    )),
    assigned_at              BIGINT  NOT NULL,   -- Unix seconds UTC
    provider_income_detailed TEXT,
    PRIMARY KEY (silver_source_id, transaction_external_id)
);

CREATE TABLE IF NOT EXISTS income_payer_categories (
    payer_signature   TEXT    NOT NULL PRIMARY KEY,
    payer_name        TEXT    NOT NULL,
    income_detailed   TEXT    NOT NULL,
    signature_version INTEGER NOT NULL,
    assigned_at       BIGINT  NOT NULL,          -- Unix seconds UTC
    model_name        TEXT    NOT NULL           -- copy of cfg.model.name at verdict time
);

CREATE TABLE IF NOT EXISTS income_account_scope (
    silver_source_id    TEXT NOT NULL,
    account_external_id TEXT NOT NULL,
    mode                TEXT NOT NULL CHECK (mode IN ('include', 'exclude')),
    PRIMARY KEY (silver_source_id, account_external_id)
);

-- The signature is the join key to the global payer store, so it is
-- looked up far more often than the primary key it hangs off.
CREATE INDEX IF NOT EXISTS ix_income_txn_enrichment_signature
    ON income_txn_enrichment(payer_signature);

-- ============================================================
-- The income populations, layered — three macros where spending has
-- four, the missing one being the matcher pool it shares.
--
--   income_scoped_accounts()             which accounts count at all
--   income_enrichment_population(f, t)   what the enrichment pass sees
--   income_lines_base(f, t)              what a view charts
--
-- The layering is the same argument as 0041's: the pass is what
-- DECIDES which rows are own-account moves or returned capital, so it
-- cannot read a population that has already dropped them.
-- ============================================================

-- income_scoped_accounts: every account, until income_account_scope
-- takes one out. No kind gate — the lesson migration 0068 records for
-- the outflow side holds here too, and more plainly: whether an
-- account can RECEIVE is not a property of its type.
CREATE OR REPLACE MACRO income_scoped_accounts() AS TABLE (
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
--     population claimed the negative half in 0041; income takes the
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
CREATE OR REPLACE MACRO income_enrichment_population(p_from, p_to) AS TABLE (
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
       AND (t.kind IN ('dividend', 'coupon', 'staking', 'capital_gain',
                       'reward', 'distribution', 'deposit')
            OR (t.kind = 'interest' AND t.net_amount > 0))
);

-- income_txn_categories: the resolution, at transaction grain.
-- Migration 0067's shape with the income vocabulary, one step added to
-- the payer and one arm of the floor doing something no spending floor
-- does.
--
-- THE KIND FLOOR is where the two families are least alike. On the outflow side a narrative
-- names a merchant and the floor is a backstop for the few rows that
-- have none. On the inflow side the floor answers for every kind the
-- population admits except one: a `dividend` row is dividend income
-- whatever its narrative says. The exception is `deposit`, which has
-- no floor because nothing but the narrative can say what it was —
-- so the narrative tiers, and eventually the model, are there for that
-- one kind.
--
-- `distribution` floors to a DELTA, which no spending floor does. A
-- private fund's distribution returns contributed capital first, so
-- most of what it pays out is basis rather than income; it is
-- therefore excluded until proven otherwise, and a rule or a pin
-- promotes the exception. `capital_gain` is the near neighbour that
-- stays income: a public fund's payout of realised gains returns no
-- basis at all, and the holding is still held.
--
-- The floor is read UNDER the payer store, never over it, for 0066's
-- reason: a verdict that named the payer is finer than anything a kind
-- can say.
--
-- `interest` is floored only when POSITIVE. Inside the population the
-- guard is redundant, which is the reason to state it rather than to
-- drop it: this macro's driving table is income_txn_enrichment, keyed
-- by transaction alone, and a pin names a TRANSACTION, so a pinned row
-- reaches the overlay whether or not the population would admit it.
-- Read at transaction grain, a finance charge without this guard would
-- be labelled interest earned. 0067 carries the same guard on the same
-- kind from the other side, and the two should read alike.
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
CREATE OR REPLACE MACRO income_txn_categories() AS TABLE (
    WITH floored AS (
        SELECT e.*,
               t.instrument_external_id,
               CASE t.kind
                   WHEN 'dividend'     THEN 'INCOME_DIVIDENDS'
                   WHEN 'coupon'       THEN 'INCOME_INTEREST_EARNED'
                   WHEN 'staking'      THEN 'INCOME_STAKING'
                   WHEN 'capital_gain' THEN 'INCOME_DISTRIBUTIONS'
                   WHEN 'reward'       THEN 'INCOME_REWARDS'
                   WHEN 'distribution' THEN 'capital_return'
                   WHEN 'interest'     THEN
                       CASE WHEN t.net_amount > 0
                            THEN 'INCOME_INTEREST_EARNED' END
               END AS kind_floor
          FROM income_txn_enrichment e
          LEFT JOIN transactions t
                 ON t.silver_source_id        = e.silver_source_id
                AND t.transaction_external_id = e.transaction_external_id
    )
    SELECT e.silver_source_id, e.transaction_external_id,
           e.payer_signature,
           CASE WHEN c.spend_primary = c.spend_detailed THEN NULL
                ELSE COALESCE(i.name, i.symbol, m.payer_name,
                              NULLIF(TRIM(e.payer_signature), ''))
           END AS payer_name,
           COALESCE(e.income_detailed, m.income_detailed, e.kind_floor) AS income_detailed,
           c.spend_primary AS income_primary,
           c.label         AS income_label,
           c.primary_label AS income_primary_label,
           CASE WHEN e.income_detailed IS NULL AND m.income_detailed IS NOT NULL
                     THEN 'model'
                WHEN e.income_detailed IS NULL AND m.income_detailed IS NULL
                     AND e.kind_floor IS NOT NULL THEN 'kind'
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
             ON c.spend_detailed = COALESCE(e.income_detailed, m.income_detailed,
                                            e.kind_floor)
      LEFT JOIN spend_categories pc
             ON pc.spend_detailed = e.provider_income_detailed
);

-- income_lines_base: what a view charts — the population with its type
-- resolved and what is not income removed.
--
-- Four exclusions, named one by one rather than read off a flag, so
-- that adding a value to the taxonomy cannot silently remove rows from
-- the base:
--
--   internal_transfer  an own-account move; the matcher's verdict,
--                      written by the same pass on both legs
--   capital_return     the holder's own capital coming back
--   loan_proceeds      money borrowed; a liability incurred
--   reimbursement      money back for money spent
--
-- The other deltas stay: `gift`, `inheritance` and `cash_deposit` are
-- receipts in their own right, and `other` is what a line nothing could
-- place reads as. A row resolving to NULL stays too — that is the
-- backlog the model tier works from, and an unplaced inbound wire is
-- shown rather than guessed at.
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
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

-- The `reward` move: a rewards credit is income, and is never netted
-- against card spend.
--
-- It has been in the spending population since 0041, on the reading
-- that cashback offsets the spending that earned it. The income side
-- makes that reading untenable: cashback, a statement credit, an
-- account-opening bonus and a referral bonus all arrive as `reward`,
-- and only the first has any relationship to spending at all. Netting
-- them there would also contradict the decision of record on the
-- outflow side — a statement paydown is an own-account move, and there
-- is no such thing as a rebate on one. So the kind moves whole:
-- INCOME_REWARDS at the income floor above, and out of the population
-- here.
--
-- Nothing in gold changes today. No adapter emits the kind yet (the
-- chase and amex transaction mappers say so in as many words: an issuer
-- marks the rows that EARN cashback, not the credit itself), so the
-- move is a decision taken before the first row arrives rather than a
-- reclassification of history.
--
-- Which also means the decision does not yet BITE. A cashback credit
-- reaching gold today is kinded `refund` by every card adapter — a
-- statement credit is indistinguishable from a merchant credit once the
-- issuer's own descriptor is gone — so it still nets inside the
-- merchant's category on the spending side. What changes that is an
-- adapter learning to tell the two apart, not this migration.
--
-- 0055's body, carried forward whole, less `reward`.
-- spending_lines_base is NOT re-issued: it selects from this macro, so
-- it follows.
CREATE OR REPLACE MACRO spend_enrichment_population(p_from, p_to) AS TABLE (
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
       AND (t.kind IN ('purchase', 'refund', 'withdrawal', 'fee', 'tax')
            OR (t.kind = 'interest' AND t.net_amount < 0))
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (70, CAST(epoch(now()) AS BIGINT));
