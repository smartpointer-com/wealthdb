-- Reports read our verdict, and read it in words.
--
-- Two changes, both presentation:
--
--   * every spending report carries the display label beside the value
--     it labels, so a column header and a category cell read as prose
--     rather than as an identifier. The value travels with it and stays
--     the key: a caller grouping by `spend_detailed` gets exactly what
--     it got before, and a label is never something to group by.
--   * the issuer's own classification travels too, as
--     `provider_spend_*`. It is off to the side, never summed with
--     ours, and present so a reader who wants the card provider's view
--     can have it without leaving the report.
--
-- Every macro below is re-issued whole, so each carries forward what
-- the migration that last defined it established: 0045's investment
-- exclusion in spending_lines_base, 0049's epoch_ms timestamps in
-- web_spending, 0057's issuer columns in spend_txn_categories.

-- spend_txn_categories: the labels come from the same dimension rows
-- the values already resolve their primary through, so there is one
-- definition of what a value reads as.
CREATE OR REPLACE MACRO spend_txn_categories() AS TABLE (
    SELECT e.silver_source_id, e.transaction_external_id,
           e.merchant_signature,
           CASE WHEN c.spend_primary = c.spend_detailed THEN e.merchant_label
                ELSE COALESCE(m.merchant_name, NULLIF(TRIM(e.merchant_signature), ''))
           END AS merchant_name,
           COALESCE(e.spend_detailed, m.spend_detailed) AS spend_detailed,
           c.spend_primary,
           c.label         AS spend_label,
           c.primary_label AS spend_primary_label,
           CASE WHEN e.spend_detailed IS NULL AND m.spend_detailed IS NOT NULL
                THEN 'model' ELSE e.provenance END AS provenance,
           e.provider_spend_detailed,
           pc.spend_primary  AS provider_spend_primary,
           pc.label          AS provider_spend_label,
           pc.primary_label  AS provider_spend_primary_label
      FROM spend_txn_enrichment e
      LEFT JOIN spend_merchant_categories m
             ON m.merchant_signature = e.merchant_signature
      LEFT JOIN spend_categories c
             ON c.spend_detailed = COALESCE(e.spend_detailed, m.spend_detailed)
      LEFT JOIN spend_categories pc
             ON pc.spend_detailed = e.provider_spend_detailed
);

-- spending_lines_base: 0045's body, projecting the six new columns.
-- spending_lines_outccy and spending_lines_multi select `b.*`, so they
-- carry them without being re-issued.
CREATE OR REPLACE MACRO spending_lines_base(p_from, p_to) AS TABLE (
    SELECT p.silver_source_id, p.transaction_external_id, p.occurred_at,
           p.account_external_id, p.account_kind, p.display_name,
           p.nickname, p.account_category,
           p.kind, p.currency, p.net_amount, p.description,
           p.counterparty, p.provider_category,
           c.merchant_signature, c.merchant_name, c.spend_detailed,
           c.spend_primary, c.spend_label, c.spend_primary_label,
           c.provenance,
           c.provider_spend_detailed, c.provider_spend_primary,
           c.provider_spend_label, c.provider_spend_primary_label
      FROM spend_enrichment_population(p_from, p_to) p
      LEFT JOIN spend_txn_categories() c
             ON c.silver_source_id        = p.silver_source_id
            AND c.transaction_external_id = p.transaction_external_id
     WHERE c.spend_detailed IS DISTINCT FROM 'internal_transfer'
       AND c.spend_detailed IS DISTINCT FROM 'investment'
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

-- The category reports gain `category_label` beside `category`. The
-- key keeps its old spelling and its old name, so a caller that groups
-- or joins on it is untouched; the label is what a reader sees. A line
-- no tier placed reads "(uncategorized)" in both, as it did.
CREATE OR REPLACE MACRO report_spending_categories(p_from, p_to, p_ccy, p_period, p_level) AS TABLE (
    WITH labelled AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COALESCE(CASE WHEN p_level = 'primary' THEN spend_primary ELSE spend_detailed END,
                        '(uncategorized)') AS category,
               COALESCE(CASE WHEN p_level = 'primary' THEN spend_primary_label ELSE spend_label END,
                        '(uncategorized)') AS category_label,
               value_outccy
          FROM spending_lines_outccy(p_from, p_to, p_ccy)),
    agg AS (
        SELECT period_start, category, category_label,
               COUNT(*)            AS txn_count,
               COUNT(value_outccy) AS n_converted,
               SUM(CASE WHEN value_outccy < 0 THEN -value_outccy ELSE 0 END) AS spend_raw,
               SUM(CASE WHEN value_outccy > 0 THEN  value_outccy ELSE 0 END) AS refunds_raw
          FROM labelled
         GROUP BY 1, 2, 3),
    vals AS (
        SELECT period_start, category, category_label, txn_count,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE spend_raw   END AS DECIMAL(28,4)) AS spend_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE refunds_raw END AS DECIMAL(28,4)) AS refunds_val
          FROM agg),
    net AS (
        SELECT period_start, category, category_label, txn_count, spend_val, refunds_val,
               CAST(spend_val - refunds_val AS DECIMAL(28,4)) AS net_val
          FROM vals)
    SELECT period_start, category, category_label, txn_count,
           CAST(spend_val   AS VARCHAR) AS spend,
           CAST(refunds_val AS VARCHAR) AS refunds,
           CAST(net_val     AS VARCHAR) AS net_spend,
           ABS(net_val)::DOUBLE
               / NULLIF(SUM(ABS(net_val)) OVER (PARTITION BY period_start), 0) AS share
      FROM net
     ORDER BY period_start, net_val DESC, category
);

CREATE OR REPLACE MACRO report_spending_categories_multi(p_from, p_to, p_period, p_level) AS TABLE (
    WITH labelled AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COALESCE(CASE WHEN p_level = 'primary' THEN spend_primary ELSE spend_detailed END,
                        '(uncategorized)') AS category,
               COALESCE(CASE WHEN p_level = 'primary' THEN spend_primary_label ELSE spend_label END,
                        '(uncategorized)') AS category_label,
               value_usd, value_chf, value_eur
          FROM spending_lines_multi(p_from, p_to)),
    agg AS (
        SELECT period_start, category, category_label,
               COUNT(*) AS txn_count,
               COUNT(value_usd) AS n_usd, COUNT(value_chf) AS n_chf, COUNT(value_eur) AS n_eur,
               SUM(CASE WHEN value_usd < 0 THEN -value_usd ELSE 0 END) AS spend_usd_raw,
               SUM(CASE WHEN value_chf < 0 THEN -value_chf ELSE 0 END) AS spend_chf_raw,
               SUM(CASE WHEN value_eur < 0 THEN -value_eur ELSE 0 END) AS spend_eur_raw,
               SUM(CASE WHEN value_usd > 0 THEN  value_usd ELSE 0 END) AS refunds_usd_raw,
               SUM(CASE WHEN value_chf > 0 THEN  value_chf ELSE 0 END) AS refunds_chf_raw,
               SUM(CASE WHEN value_eur > 0 THEN  value_eur ELSE 0 END) AS refunds_eur_raw
          FROM labelled
         GROUP BY 1, 2, 3),
    vals AS (
        SELECT period_start, category, category_label, txn_count,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE spend_usd_raw   END AS DECIMAL(28,4)) AS spend_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE spend_chf_raw   END AS DECIMAL(28,4)) AS spend_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE spend_eur_raw   END AS DECIMAL(28,4)) AS spend_eur,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE refunds_usd_raw END AS DECIMAL(28,4)) AS refunds_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE refunds_chf_raw END AS DECIMAL(28,4)) AS refunds_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE refunds_eur_raw END AS DECIMAL(28,4)) AS refunds_eur
          FROM agg),
    net AS (
        SELECT *,
               CAST(spend_usd - refunds_usd AS DECIMAL(28,4)) AS net_spend_usd,
               CAST(spend_chf - refunds_chf AS DECIMAL(28,4)) AS net_spend_chf,
               CAST(spend_eur - refunds_eur AS DECIMAL(28,4)) AS net_spend_eur
          FROM vals)
    SELECT period_start, category, category_label, txn_count,
           spend_usd, spend_chf, spend_eur,
           refunds_usd, refunds_chf, refunds_eur,
           net_spend_usd, net_spend_chf, net_spend_eur,
           ABS(net_spend_usd)::DOUBLE
               / NULLIF(SUM(ABS(net_spend_usd)) OVER (PARTITION BY period_start), 0) AS share_usd,
           ABS(net_spend_chf)::DOUBLE
               / NULLIF(SUM(ABS(net_spend_chf)) OVER (PARTITION BY period_start), 0) AS share_chf,
           ABS(net_spend_eur)::DOUBLE
               / NULLIF(SUM(ABS(net_spend_eur)) OVER (PARTITION BY period_start), 0) AS share_eur
      FROM net
     ORDER BY period_start, net_spend_usd DESC, category
);

-- The transaction reports carry both classifications, ours first.
CREATE OR REPLACE MACRO report_spending_transactions(p_from, p_to, p_ccy) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, merchant_signature, merchant_name, spend_primary, spend_detailed,
           spend_label, spend_primary_label,
           provenance, provider_spend_detailed, provider_spend_label, currency,
           CAST(net_amount AS VARCHAR) AS net_amount,
           description, counterparty,
           CAST(value_outccy AS VARCHAR) AS value_outccy
      FROM spending_lines_outccy(p_from, p_to, p_ccy)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

CREATE OR REPLACE MACRO report_spending_transactions_multi(p_from, p_to) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, merchant_signature, merchant_name, spend_primary, spend_detailed,
           spend_label, spend_primary_label,
           provenance, provider_spend_detailed, provider_spend_label, currency,
           CAST(net_amount AS DECIMAL(28,4)) AS net_amount,
           description, counterparty,
           value_usd, value_chf, value_eur
      FROM spending_lines_multi(p_from, p_to)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

-- web_spending: 0049's body, showing the label and offering the
-- issuer's view as its own column. The dashboard groups on the label
-- because it is what a reader sees; the value is beside it for a
-- filter that must not drift when a label is reworded.
CREATE OR REPLACE VIEW web_spending AS
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           silver_source_id, account_external_id, display_name, account_kind,
           merchant_name,
           COALESCE(spend_primary,  '(uncategorized)') AS spend_primary,
           COALESCE(spend_detailed, '(uncategorized)') AS spend_detailed,
           COALESCE(spend_primary_label, '(uncategorized)') AS spend_primary_label,
           COALESCE(spend_label,         '(uncategorized)') AS spend_label,
           provider_spend_label,
           value_usd, value_chf, value_eur
      FROM report_spending_transactions_multi(0, 9223372036854775807);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (59, CAST(epoch(now()) AS BIGINT));
