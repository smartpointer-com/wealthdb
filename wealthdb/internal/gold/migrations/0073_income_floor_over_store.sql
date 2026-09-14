-- On the income side the kind floor outranks the payer store.
--
-- The two families ask a model different questions, and the ordering
-- follows from which question it is.
--
-- On the OUTFLOW side the model names a merchant and picks that
-- merchant's category. It is never allowed to decide that a card
-- purchase was actually an investment: the nature of the row is the
-- data's, and what the model adds is a finer reading of who was paid.
-- A merchant verdict is therefore finer than any kind floor, and 0066
-- put the floor under the store for exactly that reason.
--
-- On the INFLOW side the model is asked what KIND OF INCOME a receipt
-- is, which is a claim about the transaction itself rather than about
-- who sent it. Wherever the data already makes that claim — a
-- `dividend` is dividend income, a `staking` credit is staking income,
-- a `coupon` is interest earned — the model has nothing to add and can
-- only be wrong. So the floor is read OVER the store here, and the
-- model has a say on the one kind that has no floor at all: `deposit`.
--
-- What this fixes, concretely. A payer signature is shared across every
-- row that folds to it, so an employer whose shares are also held would
-- carry one verdict onto both: the salary deposit's INCOME_WAGES would
-- re-type that employer's dividends. Under this ordering the dividend
-- keeps INCOME_DIVIDENDS with provenance `kind` and the deposit keeps
-- the store's verdict with provenance `model`. The same ordering is
-- what makes `categorize income --all` safe to run: re-asking every
-- deposit signature can no longer reach a floor-placed row. (The
-- candidate query is restricted to `deposit` as well, so such a row is
-- never asked about in the first place — two independent guards, as the
-- fence and the context level are.)
--
-- Unchanged by the re-order: EVERY TIER THE PASS WRITES still outranks
-- the floor — pin, matcher, built-in rule, config rule and provider map
-- alike — because they all write income_txn_enrichment and
-- e.income_detailed stays the head of the COALESCE. Only the model
-- moved. Decision 4 is therefore untouched: a `distribution` floors to
-- `capital_return` and a tier that writes the overlay promotes it,
-- which in practice means a rule or a pin, no provider map claiming one
-- today.
--
-- Also carried in: NULLIF on the instrument's name and symbol. Three
-- adapters pass a name pointer unconditionally and the writer stores
-- `''`, which COALESCE reads as an answer; an instrument named by the
-- empty string would have blanked the payer instead of falling through
-- to the store or the signature. Gold holds no such instrument today.
--
-- The rest of the body is 0070's verbatim. A re-issue replaces the
-- whole macro (0066's discipline), so the payer's four steps, the
-- delta arm, the floor's kinds and the provider columns all have to be
-- carried forward.
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
-- meeting the list here is not left to infer which half is meant. 0067
-- carries the same guard on the same kind from the other side, and the
-- two should read alike.
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
-- The payer's steps are NOT re-ordered. A verdict's NAME is still the
-- model's to give: naming who paid is the same job on both sides, and
-- nothing in the data names an instrument-free payer. Only the TYPE
-- moved.
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

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (73, CAST(epoch(now()) AS BIGINT));
