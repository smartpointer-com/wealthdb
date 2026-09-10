-- Interest charged joins the kind floor.
--
-- Migration 0066 floored `fee` and `tax`. `interest` is the third kind
-- the population admits and the floor missed: a broker's margin
-- interest reaches gold as `Margin Interest INTEREST <period>`
-- or, on the older rows, a bare `INTEREST <dates>` — no payee, and a
-- rule keyed on the word would be keying on a coincidence of spelling
-- rather than on what the row is.
--
-- The kind says it. And here the sign says the rest: the population
-- admits an interest row ONLY when it is negative (migration 0041 —
-- credited interest is income, charged interest is spend), so within
-- the base every interest row is a charge. The floor still tests the
-- sign rather than relying on that, because this macro is read at
-- transaction grain too, where a credited interest row does reach it
-- and must not be labelled a fee.
--
-- BANK_FEES_INTEREST_CHARGE is vendored, not one of ours: "fees
-- incurred for interest on purchases, including not-paid-in-full or
-- interest on cash advances". Margin interest is the same animal —
-- the cost of money borrowed inside the account — so it needs no
-- extension.
--
-- The rest of the body is 0066's verbatim; a re-issue replaces the
-- whole macro.
CREATE OR REPLACE MACRO spend_txn_categories() AS TABLE (
    WITH floored AS (
        SELECT e.*,
               CASE t.kind
                   WHEN 'fee' THEN 'BANK_FEES_INVESTMENT_FEES'
                   WHEN 'tax' THEN 'GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX'
                   WHEN 'interest' THEN
                       CASE WHEN t.net_amount < 0
                            THEN 'BANK_FEES_INTEREST_CHARGE' END
               END AS kind_floor
          FROM spend_txn_enrichment e
          LEFT JOIN transactions t
                 ON t.silver_source_id        = e.silver_source_id
                AND t.transaction_external_id = e.transaction_external_id
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

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (67, CAST(epoch(now()) AS BIGINT));
