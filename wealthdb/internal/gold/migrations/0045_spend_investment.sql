-- The fourth delta, `investment`, and its exclusion from the spending
-- base.
--
-- The vendored vocabulary is consumption through and through, and a
-- cash account also funds things that are not consumed: a securities
-- subscription, a deposit into a wallet, each booked by the bank as a
-- plain withdrawal. The policy the value encodes is that capital
-- deployed is not spending. It is an `internal_transfer` when the
-- destination is an account the product tracks — the matcher or a rule
-- can see that — and an `investment` when it is not. Before this
-- migration such a row had no honest home: `internal_transfer` would
-- be false, `other` is excluded from nothing and explains nothing, and
-- every vendored category says it was consumed.
--
-- Two statements, and nothing else changes:
--
--   * spend_categories gains the row, seeded from
--     internal/canonical/spendtaxonomy.go exactly as migration 0040
--     seeds the vendored vocabulary; TestSpendCategoriesMatchGoTable
--     pins the dimension to the Go table.
--   * spending_lines_base is re-issued with `investment` excluded
--     EXACTLY as `internal_transfer` is: one more IS DISTINCT FROM on
--     the same column, every other predicate byte-identical to
--     migration 0042's issue. A NULL category still passes both, so the
--     backlog stays in. The reports and the web views read the base by
--     name through spend_txn_categories() and need no re-issue.
--
-- It is a delta, so the model tier may never emit it: the gauntlet
-- validates against canonical.VendoredSpendDetailed, which is derived
-- from the vendored rows alone, and refuses the new value with no
-- change to the command. Rules (`spending.rules`) and pins
-- (`spending.pins`) are what place it.
--
-- INSERT OR REPLACE / CREATE OR REPLACE keep this replayable for the
-- DDL-rerun test (see gold.Migrate's REPLAY note).
INSERT OR REPLACE INTO spend_categories (spend_primary, spend_detailed, description) VALUES
    ('investment', 'investment', 'Capital deployed from a cash account — a securities subscription, a deposit into a wallet — whose destination the product does not track; not consumed, and not an own-account move');

CREATE OR REPLACE MACRO spending_lines_base(p_from, p_to) AS TABLE (
    SELECT p.silver_source_id, p.transaction_external_id, p.occurred_at,
           p.account_external_id, p.account_kind, p.display_name,
           p.nickname, p.account_category,
           p.kind, p.currency, p.net_amount, p.description,
           p.counterparty, p.provider_category,
           c.merchant_signature, c.merchant_name, c.spend_detailed,
           c.spend_primary, c.provenance
      FROM spend_enrichment_population(p_from, p_to) p
      LEFT JOIN spend_txn_categories() c
             ON c.silver_source_id        = p.silver_source_id
            AND c.transaction_external_id = p.transaction_external_id
     WHERE c.spend_detailed IS DISTINCT FROM 'internal_transfer'
       AND c.spend_detailed IS DISTINCT FROM 'investment'
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (45, CAST(epoch(now()) AS BIGINT));
