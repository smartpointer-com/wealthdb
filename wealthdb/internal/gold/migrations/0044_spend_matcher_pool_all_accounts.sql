-- Re-issue spend_matcher_pool over EVERY account in gold, not just the
-- accounts spending is scoped to.
--
-- The internal-transfer matcher exists to recognise that money leaving
-- one tracked account arrived in another. It can only pair legs it can
-- SEE, and the pool it was given drew from spend_scoped_accounts() —
-- account kinds `cash` and `card`. Any movement whose receiving leg
-- lands on an account of another kind therefore had one leg in the
-- pool and one leg nowhere: unpairable by construction. An unpaired
-- outgoing leg is indistinguishable from spending, so cash moved from
-- a deposit account into an investment account was counted as spend,
-- at whatever size the movement was — an unbounded error, not a
-- marginal one.
--
-- What changes, and what deliberately does not:
--
--   * The pool is now `transactions` restricted to the window and the
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
--     spending_lines_base() are untouched. The spending BASE stays
--     cash and card. A wider pool adds candidates for PAIRING; it adds
--     nothing to what a report charts.
--   * The eligible KIND set is unchanged: `deposit`, `withdrawal`,
--     `card_payment`, `transfer_in`, `transfer_out`. Those are exactly
--     the kinds that mean "cash moved into or out of an account" AND
--     carry a pinned canonical sign (internal/canonical/sign.go), so
--     every leg can be oriented as debit or credit. `journal` and
--     `other` are catch-alls whose sign is whatever the source
--     supplied and whose meaning is "a bookkeeping entry"; `fx*` and
--     `corporate_action` are unsigned for the same reason;
--     `contribution` and `distribution` are booked from the funding
--     account's own perspective, so neither can ever be the RECEIVING
--     half of a movement out of it. Admitting any of them buys false
--     pairs, and a false pair does not surface as a wrong category —
--     it silently deletes a real spending line.
--
-- One consequence outside spending: report_transactions (migration
-- 0042) LEFT JOINs spend_txn_categories() onto every transaction, so a
-- paired leg on an out-of-scope account now reads `internal_transfer`
-- there instead of NULL. That is the label the pass has evidence for.
--
-- CREATE OR REPLACE keeps this replayable for the DDL-rerun test (see
-- gold.Migrate's REPLAY note).
CREATE OR REPLACE MACRO spend_matcher_pool(p_from, p_to) AS TABLE (
    SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
           t.account_external_id, t.kind, t.currency, t.net_amount,
           t.description, t.counterparty
      FROM transactions t
     WHERE t.occurred_at BETWEEN p_from AND p_to
       AND t.kind IN ('deposit', 'withdrawal', 'card_payment',
                      'transfer_in', 'transfer_out')
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (44, CAST(epoch(now()) AS BIGINT));
