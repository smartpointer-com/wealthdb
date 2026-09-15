-- The income reports, and the income columns on `wealthdb transactions`.
--
-- Migration 0042 read in the other direction, carrying forward what
-- 0059 added to it: every macro below is its spending twin with the
-- income vocabulary, and where one differs there is a reason in a
-- comment. The FX blocks are duplicated rather than shared because a
-- DuckDB table macro cannot take a table as a parameter — the same
-- reason 0042 duplicates them across its own pair.
--
-- The vocabulary of the numbers is the mirror of spending's, and the
-- mirror is exact: spending sums debits as `spend` and credits as
-- `refunds` and subtracts; income sums credits as `income` and debits
-- as `reversals` and subtracts. Both report positive magnitudes, so a
-- reader comparing the two columns is comparing like with like, and
-- `net_income` is what the household actually received.
--
-- One column has no spending twin: `withheld`. It is the tax deducted
-- at source in the same window on the same accounts — shown beside the
-- income it was withheld from, off by default, and NEVER subtracted.
-- Income is gross as booked, so the withholding is already the
-- spending side's WITHHOLDING_TAX; netting it here would subtract it
-- twice from the household's arithmetic. It is a memo, and
-- `net_income` does not read it.
--
-- IF NOT EXISTS / OR REPLACE throughout keep this replayable for the
-- DDL-rerun test.

-- ============================================================
-- The FX pair: the base with its amounts converted.
-- ============================================================

CREATE OR REPLACE MACRO income_lines_outccy(p_from, p_to, p_ccy) AS TABLE (
    SELECT b.*,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * d.rate, b.net_amount::DOUBLE * c1.rate * c2.rate,
               b.net_amount::DOUBLE * u1.rate * u2.rate) AS DECIMAL(28,4)) AS value_outccy
      FROM income_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy  = b.currency AND d.to_ccy  = p_ccy AND d.day  <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= (b.occurred_at // 86400)
);

CREATE OR REPLACE MACRO income_lines_multi(p_from, p_to) AS TABLE (
    SELECT b.*,
           CAST(COALESCE(CASE WHEN b.currency = 'USD' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_usd.rate,
               b.net_amount::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS value_usd,
           CAST(COALESCE(CASE WHEN b.currency = 'CHF' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_chf.rate,
               b.net_amount::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS value_chf,
           CAST(COALESCE(CASE WHEN b.currency = 'EUR' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_eur.rate,
               b.net_amount::DOUBLE * p_chf.rate * chf_eur.rate,
               b.net_amount::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS value_eur
      FROM income_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = b.currency AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = b.currency AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = b.currency AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF'      AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF'      AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD'      AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD'      AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (b.occurred_at // 86400)
);

-- ============================================================
-- The withholding memo.
--
-- Tax deducted at source, bucketed and converted exactly like the
-- lines it sits beside, so the two numbers are comparable. It reads
-- `transactions` directly rather than any income macro: withholding is
-- an OUTFLOW and is in no income population, which is the whole point
-- of showing it as a memo rather than as part of the arithmetic.
--
-- Scoped to income's accounts, since that is the set the income beside
-- it was summed over. Positive magnitude, like every other figure in
-- these reports.
-- ============================================================
CREATE OR REPLACE MACRO income_withheld_outccy(p_from, p_to, p_ccy, p_period) AS TABLE (
    SELECT spend_period_bucket(p_period, t.occurred_at) AS period_start,
           CAST(SUM(-COALESCE(CASE WHEN t.currency = p_ccy THEN t.net_amount::DOUBLE END,
               t.net_amount::DOUBLE * d.rate, t.net_amount::DOUBLE * c1.rate * c2.rate,
               t.net_amount::DOUBLE * u1.rate * u2.rate)) AS DECIMAL(28,4)) AS withheld
      FROM transactions t
      JOIN income_scoped_accounts() sa
             ON sa.silver_source_id    = t.silver_source_id
            AND sa.account_external_id = t.account_external_id
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy  = t.currency AND d.to_ccy  = p_ccy AND d.day  <= (t.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = t.currency AND c1.to_ccy = 'CHF' AND c1.day <= (t.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF'      AND c2.to_ccy = p_ccy AND c2.day <= (t.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = t.currency AND u1.to_ccy = 'USD' AND u1.day <= (t.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD'      AND u2.to_ccy = p_ccy AND u2.day <= (t.occurred_at // 86400)
     WHERE t.occurred_at BETWEEN p_from AND p_to
       AND t.kind = 'tax' AND t.net_amount < 0
     GROUP BY 1
);

-- ============================================================
-- The three reports.
-- ============================================================

-- report_income_summary: one row per bucket. `income` and `reversals`
-- are positive magnitudes and `net_income` is the difference, mirroring
-- report_spending_summary's spend/refunds/net_spend exactly.
--
-- A bucket where nothing converted reports NULL rather than zero — the
-- same n_converted guard the spending summary uses, so a missing FX
-- rate reads as "not known" instead of "nothing received".
--
-- `withheld` is FULL JOINed by bucket, so neither side can drop a
-- bucket the other has: a window with income and no withholding shows
-- the income and an empty memo, and a bucket whose only booking was a
-- tax row keeps its row rather than vanishing. The join itself carries
-- the reasoning.
CREATE OR REPLACE MACRO report_income_summary(p_from, p_to, p_ccy, p_period) AS TABLE (
    WITH agg AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COUNT(*)            AS txn_count,
               COUNT(value_outccy) AS n_converted,
               SUM(CASE WHEN value_outccy > 0 THEN  value_outccy ELSE 0 END) AS income_raw,
               SUM(CASE WHEN value_outccy < 0 THEN -value_outccy ELSE 0 END) AS reversals_raw
          FROM income_lines_outccy(p_from, p_to, p_ccy)
         GROUP BY 1),
    vals AS (
        SELECT period_start, txn_count,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE income_raw    END AS DECIMAL(28,4)) AS income_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE reversals_raw END AS DECIMAL(28,4)) AS reversals_val
          FROM agg)
    -- A FULL join, not a LEFT one. The two sides are independent
    -- populations: a bucket can hold income and no withholding (the
    -- common case) and, on a quarter whose only booking was tax
    -- deducted at source, withholding and no income. A left join would
    -- drop that second bucket entirely and take the memo with it —
    -- the one row a reader went looking for.
    --
    -- IS NOT DISTINCT FROM, not `=`: `--period total` collapses the
    -- window into ONE bucket whose period_start is NULL on both sides,
    -- and NULL = NULL matches nothing. An equality join here would
    -- leave the memo empty on exactly the report most likely to be read
    -- for a tax year.
    SELECT COALESCE(v.period_start, w.period_start) AS period_start,
           COALESCE(v.txn_count, 0) AS txn_count,
           CAST(v.income_val    AS VARCHAR) AS income,
           CAST(v.reversals_val AS VARCHAR) AS reversals,
           CAST(v.income_val - v.reversals_val AS VARCHAR) AS net_income,
           CAST(w.withheld AS VARCHAR) AS withheld
      FROM vals v
      FULL JOIN income_withheld_outccy(p_from, p_to, p_ccy, p_period) w
             ON w.period_start IS NOT DISTINCT FROM v.period_start
     ORDER BY 1
);

-- report_income_types: a (bucket, type) pair with its share of the
-- bucket. report_spending_categories' body with the income vocabulary;
-- `share` is over net-income magnitudes, as spending's is over net
-- spend.
--
-- No `withheld` here, and that is a correction to the first draft of
-- the design rather than an omission: a tax row names the security it
-- was withheld from, not the income TYPE, so there is no honest way to
-- attribute it to one row of this report. The memo lives on the summary,
-- where the window is the only grain it needs.
CREATE OR REPLACE MACRO report_income_types(p_from, p_to, p_ccy, p_period, p_level) AS TABLE (
    WITH labelled AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COALESCE(CASE WHEN p_level = 'primary' THEN income_primary ELSE income_detailed END,
                        '(uncategorized)') AS type,
               COALESCE(CASE WHEN p_level = 'primary' THEN income_primary_label ELSE income_label END,
                        '(uncategorized)') AS type_label,
               value_outccy
          FROM income_lines_outccy(p_from, p_to, p_ccy)),
    agg AS (
        SELECT period_start, type, type_label,
               COUNT(*)            AS txn_count,
               COUNT(value_outccy) AS n_converted,
               SUM(CASE WHEN value_outccy > 0 THEN  value_outccy ELSE 0 END) AS income_raw,
               SUM(CASE WHEN value_outccy < 0 THEN -value_outccy ELSE 0 END) AS reversals_raw
          FROM labelled
         GROUP BY 1, 2, 3),
    vals AS (
        SELECT period_start, type, type_label, txn_count,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE income_raw    END AS DECIMAL(28,4)) AS income_val,
               CAST(CASE WHEN n_converted = 0 THEN NULL ELSE reversals_raw END AS DECIMAL(28,4)) AS reversals_val
          FROM agg),
    net AS (
        SELECT period_start, type, type_label, txn_count, income_val, reversals_val,
               CAST(income_val - reversals_val AS DECIMAL(28,4)) AS net_val
          FROM vals)
    SELECT period_start, type, type_label, txn_count,
           CAST(income_val    AS VARCHAR) AS income,
           CAST(reversals_val AS VARCHAR) AS reversals,
           CAST(net_val       AS VARCHAR) AS net_income,
           ABS(net_val)::DOUBLE
               / NULLIF(SUM(ABS(net_val)) OVER (PARTITION BY period_start), 0) AS share
      FROM net
     ORDER BY period_start, net_val DESC, type
);

CREATE OR REPLACE MACRO report_income_types_multi(p_from, p_to, p_period, p_level) AS TABLE (
    WITH labelled AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COALESCE(CASE WHEN p_level = 'primary' THEN income_primary ELSE income_detailed END,
                        '(uncategorized)') AS type,
               COALESCE(CASE WHEN p_level = 'primary' THEN income_primary_label ELSE income_label END,
                        '(uncategorized)') AS type_label,
               value_usd, value_chf, value_eur
          FROM income_lines_multi(p_from, p_to))
    SELECT period_start, type, type_label,
           COUNT(*) AS txn_count,
           CAST(SUM(CASE WHEN value_usd > 0 THEN value_usd ELSE 0 END)
              - SUM(CASE WHEN value_usd < 0 THEN -value_usd ELSE 0 END) AS VARCHAR) AS net_income_usd,
           CAST(SUM(CASE WHEN value_chf > 0 THEN value_chf ELSE 0 END)
              - SUM(CASE WHEN value_chf < 0 THEN -value_chf ELSE 0 END) AS VARCHAR) AS net_income_chf,
           CAST(SUM(CASE WHEN value_eur > 0 THEN value_eur ELSE 0 END)
              - SUM(CASE WHEN value_eur < 0 THEN -value_eur ELSE 0 END) AS VARCHAR) AS net_income_eur
      FROM labelled
     GROUP BY 1, 2, 3
     ORDER BY period_start, type
);

-- report_income_transactions: an income line, oldest first. The
-- merchant columns of its spending twin become the payer ones; nothing
-- else moves.
CREATE OR REPLACE MACRO report_income_transactions(p_from, p_to, p_ccy) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, payer_signature, payer_name, income_primary, income_detailed,
           income_label, income_primary_label,
           provenance, provider_income_detailed, provider_income_label, currency,
           CAST(net_amount AS VARCHAR) AS net_amount,
           description, counterparty,
           CAST(value_outccy AS VARCHAR) AS value_outccy
      FROM income_lines_outccy(p_from, p_to, p_ccy)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

CREATE OR REPLACE MACRO report_income_transactions_multi(p_from, p_to) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, payer_signature, payer_name, income_primary, income_detailed,
           income_label, income_primary_label,
           provenance, provider_income_detailed, provider_income_label, currency,
           CAST(net_amount AS DECIMAL(28,4)) AS net_amount,
           description, counterparty,
           value_usd, value_chf, value_eur
      FROM income_lines_multi(p_from, p_to)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

-- ============================================================
-- report_transactions gains the income trio.
--
-- 0042's body carried forward whole — the spending join, the symbol
-- resolutions, the FX block — with income_txn_categories() joined
-- beside spend_txn_categories() and its three columns projected after
-- the spending ones. A transaction can carry both: a deposit the
-- matcher paired has a verdict in each overlay, and this is the one
-- surface that shows a row from both sides at once.
--
-- The projection order is a contract. gold.TransactionRow scans this
-- macro POSITIONALLY, so the new columns go at the end of the named
-- block and the row struct, the scan list and this macro move together
-- in one commit.
-- ============================================================
CREATE OR REPLACE MACRO report_transactions(p_from, p_to, p_ccy) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.account_kind, a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description,
               sc.merchant_name, sc.spend_primary, sc.spend_detailed,
               ic.payer_name, ic.income_primary, ic.income_detailed
          FROM transactions t
          LEFT JOIN accounts a ON t.silver_source_id = a.silver_source_id AND t.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON t.silver_source_id = i.silver_source_id AND t.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = t.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = t.instrument_external_id
          LEFT JOIN symbol_resolutions srn ON srn.silver_source_id = t.silver_source_id
               AND srn.lookup_kind = 'name' AND srn.lookup_value = t.description
          LEFT JOIN spend_txn_categories() sc ON sc.silver_source_id = t.silver_source_id
               AND sc.transaction_external_id = t.transaction_external_id
          LEFT JOIN income_txn_categories() ic ON ic.silver_source_id = t.silver_source_id
               AND ic.transaction_external_id = t.transaction_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.account_kind, b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS VARCHAR) AS gross_amount, CAST(b.net_amount AS VARCHAR) AS net_amount,
           CAST(b.quantity AS VARCHAR) AS quantity, CAST(b.price AS VARCHAR) AS price, b.description,
           b.merchant_name, b.spend_primary, b.spend_detailed,
           b.payer_name, b.income_primary, b.income_detailed,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * d.rate, b.net_amount::DOUBLE * c1.rate * c2.rate,
               b.net_amount::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy
      FROM base b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy = b.currency AND d.to_ccy = p_ccy AND d.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = p_ccy AND c2.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = p_ccy AND u2.day <= (b.occurred_at // 86400)
     ORDER BY b.occurred_at, b.silver_source_id, b.transaction_external_id
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (71, CAST(epoch(now()) AS BIGINT));
