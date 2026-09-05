-- No merchant on a delta line.
--
-- spend_txn_categories() (migration 0042) reaches the merchant store
-- through the enrichment row's signature and published the store's
-- name for that signature on every row, whatever tier placed the row
-- and whatever it resolved to. A row whose resolved category is a
-- delta is not a merchant transaction: a card-bill payment carries the
-- holder's own name, a cash gift the recipient's, a subscription the
-- company that received the capital. Where the fence let such a
-- narrative reach the model, the store holds a name for it, and the
-- transaction reports, `web_spending` and the dashboard's merchant
-- ranking showed that name in the merchant column — a person, or the
-- holder, ranked among shops.
--
-- One statement, and nothing else changes:
--
--   * spend_txn_categories is re-issued with the same column shape.
--     merchant_name is NULL whenever the resolved spend_detailed is a
--     delta and the store's name otherwise; every other column is
--     byte-identical to migration 0042's issue. The resolution itself
--     is untouched — a delta placed at transaction scope still
--     outranks the store's verdict — so a store row and its name stay
--     in the store, where `wealthdb categorizations` lists them, and
--     the merchant column is the only thing that moves.
--   * Delta-ness is read off the seeded dimension, not restated as a
--     list. A delta is primary-level — spend_primary = spend_detailed
--     — which is the marker migration 0040 seeds from
--     internal/canonical/spendtaxonomy.go; the taxonomy's tests pin
--     that marker to exactly the delta rows and the schema test pins
--     the seed to the Go table. A delta added there and seeded here is
--     blank without this macro changing. A row with no resolved
--     category has no dimension row to test and keeps the store's
--     name, which is NULL by construction: a store row is exactly what
--     would have resolved it.
--   * spending_lines_base, the spending reports, report_transactions,
--     the web views and both CLI views read merchant_name from this
--     macro by name and need no re-issue; a view binds its macros
--     lazily and its projection is unchanged (migration 0045's
--     precedent).
--
-- CREATE OR REPLACE keeps this replayable for the DDL-rerun test (see
-- gold.Migrate's REPLAY note).
CREATE OR REPLACE MACRO spend_txn_categories() AS TABLE (
    SELECT e.silver_source_id, e.transaction_external_id,
           e.merchant_signature,
           CASE WHEN c.spend_primary = c.spend_detailed THEN NULL
                ELSE m.merchant_name END AS merchant_name,
           COALESCE(e.spend_detailed, m.spend_detailed) AS spend_detailed,
           c.spend_primary, e.provenance
      FROM spend_txn_enrichment e
      LEFT JOIN spend_merchant_categories m
             ON m.merchant_signature = e.merchant_signature
      LEFT JOIN spend_categories c
             ON c.spend_detailed = COALESCE(e.spend_detailed, m.spend_detailed)
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (48, CAST(epoch(now()) AS BIGINT));
