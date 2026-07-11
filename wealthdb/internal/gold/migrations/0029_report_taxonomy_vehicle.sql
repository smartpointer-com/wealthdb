-- Surface the 2-D taxonomy (asset_class_new + vehicle) through the
-- position report macros that the CLI `holdings positions` / `holdings
-- cash` commands consume. See docs/TAXONOMY.md.
--
-- Both new columns are projected right after the legacy `asset_class`
-- (which stays, as a control, until the cutover migration drops it),
-- so the Go scan (scanPositionRows) reads them positionally. The
-- multi-currency and history macros (report_*_multi, report_*_history)
-- are updated in the web stage; this migration is CLI-facing only.
--
-- CREATE OR REPLACE redefines the macros first defined in migration
-- 0021; the bodies are copied verbatim with `asset_class_new` /
-- `vehicle` added in the base CTE and the final projection.

CREATE OR REPLACE MACRO report_positions(p_asof, p_ccy) AS TABLE (
    WITH latest AS (
        SELECT silver_source_id, MAX(snapshot_at) AS snap
          FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    base AS (
        SELECT p.silver_source_id, p.snapshot_at, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name,
               p.asset_class, p.asset_class_new, p.vehicle, p.currency, p.quantity, p.market_value
          FROM positions p
          JOIN latest l ON p.silver_source_id = l.silver_source_id AND p.snapshot_at = l.snap
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id)
    SELECT b.silver_source_id, b.snapshot_at, b.account_external_id, b.display_name,
           b.relationship_id, b.nickname, b.account_category, b.position_key,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.asset_class_new, b.vehicle, b.currency,
           CAST(b.quantity AS VARCHAR) AS quantity, CAST(b.market_value AS VARCHAR) AS market_value,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.market_value::DOUBLE END,
               b.market_value::DOUBLE * d.rate, b.market_value::DOUBLE * c1.rate * c2.rate,
               b.market_value::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy
      FROM base b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy = b.currency AND d.to_ccy = p_ccy AND d.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = p_ccy AND c2.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = p_ccy AND u2.day <= (b.snapshot_at // 86400)
     ORDER BY b.silver_source_id, b.account_external_id, b.position_key
);

-- report_cash: cash balances are at-sight account cash, so the pair is
-- (cash, demand_deposit) for every synthetic cash row.
CREATE OR REPLACE MACRO report_cash(p_asof, p_ccy) AS TABLE (
    SELECT cc.silver_source_id, cc.snapshot_at, cc.account_external_id, a.display_name,
           a.relationship_id, a.nickname, a.account_category,
           'cash:' || cc.currency AS position_key, CAST(NULL AS VARCHAR) AS instrument_external_id,
           cc.currency AS symbol, 'Cash ' || cc.currency AS name,
           'cash' AS asset_class, 'cash' AS asset_class_new, 'demand_deposit' AS vehicle, cc.currency,
           CAST(NULL AS VARCHAR) AS quantity, CAST(cc.amount AS VARCHAR) AS market_value,
           CAST(COALESCE(CASE WHEN cc.currency = p_ccy THEN cc.amount::DOUBLE END,
               cc.amount::DOUBLE * d.rate, cc.amount::DOUBLE * c1.rate * c2.rate,
               cc.amount::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy
      FROM cash_chosen(p_asof) cc
      LEFT JOIN accounts a ON cc.silver_source_id = a.silver_source_id AND cc.account_external_id = a.account_external_id
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy = cc.currency AND d.to_ccy = p_ccy AND d.day <= (cc.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = cc.currency AND c1.to_ccy = 'CHF' AND c1.day <= (cc.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = p_ccy AND c2.day <= (cc.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = cc.currency AND u1.to_ccy = 'USD' AND u1.day <= (cc.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = p_ccy AND u2.day <= (cc.snapshot_at // 86400)
     ORDER BY cc.silver_source_id, cc.account_external_id, cc.currency
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (29, CAST(epoch(now()) AS BIGINT));
