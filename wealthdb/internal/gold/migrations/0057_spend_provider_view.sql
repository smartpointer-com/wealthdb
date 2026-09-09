-- The issuer's own classification, kept beside ours.
--
-- `transactions.provider_category` already stores what the card
-- provider called a row, verbatim (migration 0038). What was missing is
-- the MAPPED value: the pair our provider vocabulary translates that
-- string to. Without it the issuer's view can only be recovered by
-- re-running a Go map over a text column, and a tier that overrules the
-- issuer erases the comparison instead of recording it.
--
-- The column is the faithful record and never a verdict of ours:
--
--   * it is written for every row the vocabulary translates, whether or
--     not the provider tier claimed the row, and whether or not a tier
--     above overruled it;
--   * NULL means the issuer published nothing this build translates —
--     which is NOT the same as the issuer saying "Shopping". A mapped
--     catch-all records a real, if uninformative, issuer opinion; NULL
--     records no opinion at all. Collapsing the two would erase exactly
--     the distinction the decline rule turns on;
--   * it holds only what the issuer's own filing says. That CAN be a
--     delta: a provider's own word sometimes names the movement rather
--     than a line of business — an ATM withdrawal, a payment to a card,
--     an FX conversion between the holder's own accounts — and the
--     delta is then the faithful translation. What it never holds is a
--     delta OUR tiers decided from structure the issuer could not see.
--
-- Reports show ours by default and this one on request. Nothing may sum
-- across the two: they disagree on roughly a third of the rows the
-- issuer classifies, which is the point of keeping both.
ALTER TABLE spend_txn_enrichment ADD COLUMN IF NOT EXISTS provider_spend_detailed TEXT;

-- spend_txn_categories gains the issuer pair beside ours. Both resolve
-- their primary through the same dimension, so a caller grouping by
-- either gets the same roll-up rule. The rest of the body is migration
-- 0054's verbatim — a re-issue replaces the whole macro, so every
-- refinement since 0042 (the delta merchant label, the model
-- provenance, the signature fallback) has to be carried forward.
CREATE OR REPLACE MACRO spend_txn_categories() AS TABLE (
    SELECT e.silver_source_id, e.transaction_external_id,
           e.merchant_signature,
           CASE WHEN c.spend_primary = c.spend_detailed THEN e.merchant_label
                ELSE COALESCE(m.merchant_name, NULLIF(TRIM(e.merchant_signature), ''))
           END AS merchant_name,
           COALESCE(e.spend_detailed, m.spend_detailed) AS spend_detailed,
           c.spend_primary,
           CASE WHEN e.spend_detailed IS NULL AND m.spend_detailed IS NOT NULL
                THEN 'model' ELSE e.provenance END AS provenance,
           e.provider_spend_detailed,
           pc.spend_primary AS provider_spend_primary
      FROM spend_txn_enrichment e
      LEFT JOIN spend_merchant_categories m
             ON m.merchant_signature = e.merchant_signature
      LEFT JOIN spend_categories c
             ON c.spend_detailed = COALESCE(e.spend_detailed, m.spend_detailed)
      LEFT JOIN spend_categories pc
             ON pc.spend_detailed = e.provider_spend_detailed
);


INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (57, CAST(epoch(now()) AS BIGINT));
