-- ============================================================
-- gold schema, migration 0099 —
--   the ledger shows what a trade was looked up BY.
--
-- Migration 0098 stores the token an adapter resolved an instrument
-- from when that resolution failed. This puts it on the one surface a
-- person reads a transaction from, behind `-C`, so the set still to
-- close is a query rather than a per-feed payload excavation:
--
--     wealthdb transactions - today -C +instrument_hint -f json
--
-- and the entries that close it go in `transaction_instruments`, keyed
-- by exactly this value.
--
-- Carried forward whole from 0082; the one column in each projection is
-- the only change. It sits after `check_number` and before the cashflow
-- trio, because the Go reader scans this macro POSITIONALLY — the
-- projection, the row struct and the scan list move together or not at
-- all.
-- ============================================================

CREATE OR REPLACE MACRO report_transactions(p_from, p_to, p_ccy) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.account_kind, a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description,
               sc.merchant_name, sc.spend_primary, sc.spend_detailed,
               ic.payer_name, ic.income_primary, ic.income_detailed,
               t.check_number, t.instrument_hint,
               cf.section AS cashflow_section, cf.class AS cashflow_class, cf.grp AS cashflow_group
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
          LEFT JOIN cashflow_txn_nodes(p_from, p_to) cf ON cf.silver_source_id = t.silver_source_id
               AND cf.transaction_external_id = t.transaction_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.account_kind, b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS VARCHAR) AS gross_amount, CAST(b.net_amount AS VARCHAR) AS net_amount,
           CAST(b.quantity AS VARCHAR) AS quantity, CAST(b.price AS VARCHAR) AS price, b.description,
           b.merchant_name, b.spend_primary, b.spend_detailed,
           b.payer_name, b.income_primary, b.income_detailed,
           b.check_number, b.instrument_hint,
           b.cashflow_section, b.cashflow_class, b.cashflow_group,
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
    VALUES (99, CAST(epoch(now()) AS BIGINT));
