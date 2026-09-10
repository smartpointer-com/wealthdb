-- What a row IS, when nothing could say what it was for.
--
-- A brokerage account books two things no narrative explains: a
-- security-level fee (an ADR depositary charge, a platform or custody
-- fee) and tax withheld at source. Their narrative is the SECURITY —
-- `EXAMPLE TREASURY FLOATING RATE ETF` — or, on some sources,
-- nothing at all. There is no payee in them, so no rule can key on
-- one: barely any such row carries a word a pattern could match.
--
-- But the row is not unknown. Its KIND says `fee` or `tax`, and that
-- is not a guess — each adapter derives it from whatever evidence its
-- source gives (schwab-web reads a statement line reading
-- `Dividend NRA Tax <security>` and books `tax`), which is exactly
-- where source-specific knowledge belongs. The category tier should
-- read that verdict rather than try to re-derive it from prose that
-- may not carry the word at all.
--
-- So: a row of kind `fee` that nothing placed is an investment fee,
-- and one of kind `tax` is withholding — the two extensions migration
-- 0065 adds. Both are the floor and nothing more
-- — the LAST arm of the COALESCE, under the merchant store.
--
-- Under it, not over it, and that ordering is the whole design. The
-- model tier files `Foreign Transaction Fee` as
-- BANK_FEES_FOREIGN_TRANSACTION_FEES, which is finer than this floor
-- can ever be; a floor written by the enrichment pass would sit in
-- `spend_txn_enrichment` and beat the merchant store, quietly
-- coarsening every such row. Resolving it here instead means it
-- applies only where the pass AND the model both declined.
--
-- `kind` therefore joins `model` as a provenance that exists only at
-- query time. Neither is stored, so the enrichment table's closed
-- CHECK is untouched, and a reader can still tell the two apart from
-- a verdict a tier actually wrote.
--
-- This also closes the categorization backlog on these rows, which
-- selects on this macro's `spend_detailed IS NULL`
-- (cmd_categorize.go). That matters beyond tidiness: the signature on
-- a withheld-tax row reads `NRA TAX <security>`, and a model asked
-- to place one answers with whatever trade that name suggests.
--
-- The rest of the body is migration 0059's verbatim — a re-issue
-- replaces the whole macro, so the label and issuer columns it added,
-- 0054's signature fallback and 0052's issuer label all have to be
-- carried forward.
CREATE OR REPLACE MACRO spend_txn_categories() AS TABLE (
    WITH floored AS (
        SELECT e.*,
               CASE t.kind
                   WHEN 'fee' THEN 'BANK_FEES_INVESTMENT_FEES'
                   WHEN 'tax' THEN 'GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX'
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
    VALUES (66, CAST(epoch(now()) AS BIGINT));
