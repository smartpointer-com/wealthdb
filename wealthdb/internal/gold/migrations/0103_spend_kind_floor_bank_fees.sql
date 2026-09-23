-- A bank account's unplaced fee is a bank fee.
--
-- Migration 0066 floored every `fee` row nothing else placed to
-- BANK_FEES_INVESTMENT_FEES. It was written for brokerages, where such a
-- row is a security-level charge with no payee to key a rule on. A plain
-- bank account books fees too — account maintenance, card and payment
-- charges, a fee refunded by agreement — and there the floor misfiled them
-- as investment fees on accounts that hold no investments.
--
-- The account decides. A `cash` account that belongs to no portfolio is a
-- bank account, and its fee floors to BANK_FEES_OTHER_BANK_FEES ("other
-- miscellaneous bank fees", vendored). Every other account keeps the
-- investment-fee floor — including a `cash` account inside a portfolio,
-- which is the cash side of an investment mandate and whose fees (a
-- management fee debited from the mandate's cash, say) are what the floor
-- was built for. The account kind alone could not draw that line.
--
-- accounts is keyed by (silver_source_id, account_external_id), so the
-- join adds at most one row and never multiplies a transaction; a row
-- whose account is not in gold keeps the investment-fee floor, as before.
--
-- The rest of the body is 0067's verbatim; a re-issue replaces the whole
-- macro.
CREATE OR REPLACE MACRO spend_txn_categories() AS TABLE (
    WITH floored AS (
        SELECT e.*,
               CASE t.kind
                   WHEN 'fee' THEN
                       CASE WHEN a.account_kind = 'cash'
                                 AND a.portfolio_external_id IS NULL
                            THEN 'BANK_FEES_OTHER_BANK_FEES'
                            ELSE 'BANK_FEES_INVESTMENT_FEES' END
                   WHEN 'tax' THEN 'GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX'
                   WHEN 'interest' THEN
                       CASE WHEN t.net_amount < 0
                            THEN 'BANK_FEES_INTEREST_CHARGE' END
               END AS kind_floor
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

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (103, CAST(epoch(now()) AS BIGINT));
