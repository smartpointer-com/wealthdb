-- Surface the 2-D taxonomy (asset_class_new + vehicle) through the
-- multi-currency + history position macros the web (Metabase) reads.
-- See docs/TAXONOMY.md. Bodies copied verbatim from migration 0024 with
-- asset_class_new / vehicle added after the legacy asset_class in both
-- the base/pv CTE and the final projection. The single-currency CLI
-- macros were handled in 0029.

CREATE OR REPLACE MACRO report_positions_multi(p_asof) AS TABLE (
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
           CAST(b.quantity AS DECIMAL(28,8)) AS quantity, CAST(b.market_value AS DECIMAL(28,4)) AS market_value,
           CAST(COALESCE(CASE WHEN b.currency = 'USD' THEN b.market_value::DOUBLE END,
               b.market_value::DOUBLE * p_usd.rate, b.market_value::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS value_usd,
           CAST(COALESCE(CASE WHEN b.currency = 'CHF' THEN b.market_value::DOUBLE END,
               b.market_value::DOUBLE * p_chf.rate, b.market_value::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS value_chf,
           CAST(COALESCE(CASE WHEN b.currency = 'EUR' THEN b.market_value::DOUBLE END,
               b.market_value::DOUBLE * p_eur.rate, b.market_value::DOUBLE * p_chf.rate * chf_eur.rate,
               b.market_value::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS value_eur
      FROM base b
      ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = b.currency AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = b.currency AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = b.currency AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (b.snapshot_at // 86400)
     ORDER BY b.silver_source_id, b.account_external_id, b.position_key
);

CREATE OR REPLACE MACRO report_positions_history_multi() AS TABLE (
    WITH pv AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name, p.asset_class, p.asset_class_new, p.vehicle, p.currency,
               CAST(p.quantity AS DECIMAL(28,8)) AS quantity, CAST(p.market_value AS DECIMAL(28,4)) AS market_value,
               CAST(COALESCE(CASE WHEN p.currency = 'USD' THEN p.market_value::DOUBLE END,
                   p.market_value::DOUBLE * p_usd.rate, p.market_value::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS value_usd,
               CAST(COALESCE(CASE WHEN p.currency = 'CHF' THEN p.market_value::DOUBLE END,
                   p.market_value::DOUBLE * p_chf.rate, p.market_value::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS value_chf,
               CAST(COALESCE(CASE WHEN p.currency = 'EUR' THEN p.market_value::DOUBLE END,
                   p.market_value::DOUBLE * p_eur.rate, p.market_value::DOUBLE * p_chf.rate * chf_eur.rate,
                   p.market_value::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS value_eur
          FROM positions p
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = p.currency AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = p.currency AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = p.currency AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (p.snapshot_at // 86400))
    SELECT ap.day * 86400 AS as_of_day, pv.src AS silver_source_id, pv.snap AS snapshot_at,
           pv.account_external_id, pv.display_name, pv.relationship_id, pv.nickname, pv.account_category,
           pv.position_key, pv.instrument_external_id, pv.symbol, pv.name, pv.asset_class, pv.asset_class_new, pv.vehicle, pv.currency,
           pv.quantity, pv.market_value, pv.value_usd, pv.value_chf, pv.value_eur
      FROM pv JOIN hist_active_pos() ap ON ap.src = pv.src AND ap.snap = pv.snap
     ORDER BY as_of_day, pv.src, pv.account_external_id, pv.position_key
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (30, CAST(epoch(now()) AS BIGINT));
