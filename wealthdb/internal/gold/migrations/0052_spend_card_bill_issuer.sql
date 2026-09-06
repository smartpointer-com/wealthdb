-- A card bill names the issuer it was paid to.
--
-- Migration 0048 blanked the merchant column on every delta line: the
-- merchant store's name for a signature is not a merchant when the row
-- is an own-account move, a gift or capital deployed, and a person or
-- the holder's own bank ranked among shops. That rule stays. This adds
-- the one exception, with a source of truth of its own rather than the
-- store's.
--
-- A `card_spend` line is a bill for a card wealthdb does not itemise.
-- Its narrative names the issuer the bill was paid to and nothing
-- else — there are no purchases to name, which is the whole reason the
-- placeholder exists — so the issuer is the only handle there is on
-- WHICH card the money went to. Without it the card-spend line is one
-- undifferentiated bucket per period.
--
-- Two statements:
--
--   * spend_txn_enrichment gains merchant_label. The built-in
--     card-payment rule is its only writer, and only where the bill
--     matched a NAMED issuer descriptor (internal/spending/rules.go):
--     a bill recognised by a generic card-payment phrase or by a
--     masked card number names no issuer, and every other tier — the
--     model, the provider, a config rule, a pin, the matcher — leaves
--     it NULL. A tier that overrules the card rule clears it with the
--     verdict, so the label never outlives the card bill it describes.
--     The column is nullable and carries no CHECK: it is a display
--     name, not a vocabulary.
--
--   * spend_txn_categories() is re-issued, same column shape as
--     migration 0050's issue, one expression changed. merchant_name
--     resolves as: the store's name when the resolved category is NOT
--     a delta (0048, unchanged), otherwise the enrichment's
--     merchant_label, otherwise NULL — which is what the label's own
--     NULL gives on every delta line but a labelled card bill. A row
--     with no resolved category has no dimension row to test and takes
--     the store's name, which is NULL by construction (0048's note).
--
-- Nothing downstream is re-issued: spending_lines_base, the three
-- spending reports, report_transactions, the web views and both CLI
-- views read merchant_name from this macro by name, a view binds its
-- macros lazily, and no projection moves (0045's precedent, 0048's
-- note).
--
-- The dashboard's merchant ranking is the one consumer that must
-- change with this, and it changes outside gold: it filtered on a
-- non-NULL merchant as a proxy for "not a delta", which a labelled
-- card bill now passes. It excludes delta categories instead
-- (web/provision.py), so an issuer never ranks as a merchant.
--
-- ADD COLUMN IF NOT EXISTS / CREATE OR REPLACE keep this replayable for
-- the DDL-rerun test (see gold.Migrate's REPLAY note).
ALTER TABLE spend_txn_enrichment ADD COLUMN IF NOT EXISTS merchant_label TEXT;

CREATE OR REPLACE MACRO spend_txn_categories() AS TABLE (
    SELECT e.silver_source_id, e.transaction_external_id,
           e.merchant_signature,
           CASE WHEN c.spend_primary = c.spend_detailed THEN e.merchant_label
                ELSE m.merchant_name END AS merchant_name,
           COALESCE(e.spend_detailed, m.spend_detailed) AS spend_detailed,
           c.spend_primary,
           CASE WHEN e.spend_detailed IS NULL AND m.spend_detailed IS NOT NULL
                THEN 'model' ELSE e.provenance END AS provenance
      FROM spend_txn_enrichment e
      LEFT JOIN spend_merchant_categories m
             ON m.merchant_signature = e.merchant_signature
      LEFT JOIN spend_categories c
             ON c.spend_detailed = COALESCE(e.spend_detailed, m.spend_detailed)
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (52, CAST(epoch(now()) AS BIGINT));
