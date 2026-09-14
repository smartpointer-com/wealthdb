-- report_income_types_multi becomes the mirror it was meant to be.
--
-- A `_multi` macro is the single-currency report's twin, publishing all
-- three reporting currencies at once so a consumer can pick a currency
-- by choosing a column rather than by re-querying. This one was
-- published in 0071 as a LOSSY SUMMARY of its own single-currency twin
-- rather than a widening of it, and nothing reads it yet — so it is
-- corrected now, before anything is built on the shape it has.
--
-- Seven differences from `report_spending_categories_multi`, all closed
-- here. Four mattered:
--
--   * VARCHAR money. Every `_multi` since 0024 emits DECIMAL(28,4), and
--     `report_income_transactions_multi` — the sibling published beside
--     this macro in 0071 — already does. A string cannot be summed,
--     ordered numerically, or divided, which is why the two below were
--     impossible to add.
--   * No `n_converted` guard. A bucket where nothing converted reported
--     0, indistinguishable from "received nothing", contradicting the
--     invariant 0071's own summary states in as many words: a missing FX
--     rate reads as "not known", never as "nothing arrived".
--   * No gross/reversal split. Only the net was published, so a reader
--     could not see that a type's quiet month was a reversal rather
--     than an absence — the one thing the split exists for.
--   * No `share`. The single-currency twin publishes one, and
--     `wealthdb income types` describes itself by it — "one row per
--     (bucket, income type), with its share". A consumer would have had
--     to recompute it per currency.
--
-- And three cosmetic ones, taken while the body is being rewritten
-- anyway: the ordering is largest-first like every other report rather
-- than alphabetical, the aggregation moves into its own CTE (a window
-- function over an aggregate needs the extra level, which is why the
-- share could not simply be appended), and the macro gets the comment
-- it never had. The spending mirror has none either — that one is not a
-- difference closed but a small thing done while here.
--
-- What is NOT closed: income publishes a `_multi` for two of its three
-- reports and spending for all three. There is no
-- `report_income_summary_multi`, and nothing reads either summary
-- `_multi`, so adding one would be a shape to keep true for nothing
-- (docs/INCOME.md §11).
--
-- The projection goes from 7 columns to 16. Nothing reads it —
-- neither Go, nor a view, nor a web card; the only reference is a
-- COUNT(*) in a test — so the widening breaks no positional scan.
CREATE OR REPLACE MACRO report_income_types_multi(p_from, p_to, p_period, p_level) AS TABLE (
    WITH labelled AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COALESCE(CASE WHEN p_level = 'primary' THEN income_primary ELSE income_detailed END,
                        '(uncategorized)') AS type,
               COALESCE(CASE WHEN p_level = 'primary' THEN income_primary_label ELSE income_label END,
                        '(uncategorized)') AS type_label,
               value_usd, value_chf, value_eur
          FROM income_lines_multi(p_from, p_to)),
    agg AS (
        SELECT period_start, type, type_label,
               COUNT(*)         AS txn_count,
               COUNT(value_usd) AS n_usd,
               COUNT(value_chf) AS n_chf,
               COUNT(value_eur) AS n_eur,
               SUM(CASE WHEN value_usd > 0 THEN  value_usd ELSE 0 END) AS income_usd_raw,
               SUM(CASE WHEN value_chf > 0 THEN  value_chf ELSE 0 END) AS income_chf_raw,
               SUM(CASE WHEN value_eur > 0 THEN  value_eur ELSE 0 END) AS income_eur_raw,
               SUM(CASE WHEN value_usd < 0 THEN -value_usd ELSE 0 END) AS reversals_usd_raw,
               SUM(CASE WHEN value_chf < 0 THEN -value_chf ELSE 0 END) AS reversals_chf_raw,
               SUM(CASE WHEN value_eur < 0 THEN -value_eur ELSE 0 END) AS reversals_eur_raw
          FROM labelled
         GROUP BY 1, 2, 3),
    vals AS (
        SELECT period_start, type, type_label, txn_count,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE income_usd_raw    END AS DECIMAL(28,4)) AS income_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE income_chf_raw    END AS DECIMAL(28,4)) AS income_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE income_eur_raw    END AS DECIMAL(28,4)) AS income_eur,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE reversals_usd_raw END AS DECIMAL(28,4)) AS reversals_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE reversals_chf_raw END AS DECIMAL(28,4)) AS reversals_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE reversals_eur_raw END AS DECIMAL(28,4)) AS reversals_eur
          FROM agg),
    net AS (
        SELECT *,
               CAST(income_usd - reversals_usd AS DECIMAL(28,4)) AS net_income_usd,
               CAST(income_chf - reversals_chf AS DECIMAL(28,4)) AS net_income_chf,
               CAST(income_eur - reversals_eur AS DECIMAL(28,4)) AS net_income_eur
          FROM vals)
    SELECT period_start, type, type_label, txn_count,
           income_usd, income_chf, income_eur,
           reversals_usd, reversals_chf, reversals_eur,
           net_income_usd, net_income_chf, net_income_eur,
           ABS(net_income_usd)::DOUBLE
               / NULLIF(SUM(ABS(net_income_usd)) OVER (PARTITION BY period_start), 0) AS share_usd,
           ABS(net_income_chf)::DOUBLE
               / NULLIF(SUM(ABS(net_income_chf)) OVER (PARTITION BY period_start), 0) AS share_chf,
           ABS(net_income_eur)::DOUBLE
               / NULLIF(SUM(ABS(net_income_eur)) OVER (PARTITION BY period_start), 0) AS share_eur
      FROM net
     ORDER BY period_start, net_income_usd DESC, type
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (74, CAST(epoch(now()) AS BIGINT));
