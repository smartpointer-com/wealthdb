-- Taxonomy cutover: the `asset_class` column now holds the 2-D
-- exposure (the adapters write it after this binary's reload), and
-- `vehicle` the wrapper. See docs/TAXONOMY.md.
--
-- The four position report macros (0029/0030) projected
-- asset_class + asset_class_new + vehicle; redefine them to project
-- just asset_class + vehicle. Every OTHER macro references
-- `asset_class` by name only, so it keeps working unchanged (the
-- column now carries V2 values).
--
-- The transitional `asset_class_new` column is left in place, dead:
-- the writer stops populating it (so it is NULL on every reloaded
-- row) and no macro projects it. DuckDB refuses ALTER TABLE ... DROP
-- COLUMN on a table any macro depends on (positions and instruments
-- are both joined by the report macros), and dropping+recreating the
-- whole macro suite to reclaim one nullable column is not worth it.

-- ---- single-currency CLI macros (from 0029) --------------------------

CREATE OR REPLACE MACRO report_positions(p_asof, p_ccy) AS TABLE (
    WITH latest AS (
        SELECT silver_source_id, MAX(snapshot_at) AS snap
          FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    base AS (
        SELECT p.silver_source_id, p.snapshot_at, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name,
               p.asset_class, p.vehicle, p.currency, p.quantity, p.market_value
          FROM positions p
          JOIN latest l ON p.silver_source_id = l.silver_source_id AND p.snapshot_at = l.snap
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id)
    SELECT b.silver_source_id, b.snapshot_at, b.account_external_id, b.display_name,
           b.relationship_id, b.nickname, b.account_category, b.position_key,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.vehicle, b.currency,
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

CREATE OR REPLACE MACRO report_cash(p_asof, p_ccy) AS TABLE (
    SELECT cc.silver_source_id, cc.snapshot_at, cc.account_external_id, a.display_name,
           a.relationship_id, a.nickname, a.account_category,
           'cash:' || cc.currency AS position_key, CAST(NULL AS VARCHAR) AS instrument_external_id,
           cc.currency AS symbol, 'Cash ' || cc.currency AS name,
           'cash' AS asset_class, 'demand_deposit' AS vehicle, cc.currency,
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

-- ---- multi-currency + history macros (from 0030) ---------------------

CREATE OR REPLACE MACRO report_positions_multi(p_asof) AS TABLE (
    WITH latest AS (
        SELECT silver_source_id, MAX(snapshot_at) AS snap
          FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    base AS (
        SELECT p.silver_source_id, p.snapshot_at, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name,
               p.asset_class, p.vehicle, p.currency, p.quantity, p.market_value
          FROM positions p
          JOIN latest l ON p.silver_source_id = l.silver_source_id AND p.snapshot_at = l.snap
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id)
    SELECT b.silver_source_id, b.snapshot_at, b.account_external_id, b.display_name,
           b.relationship_id, b.nickname, b.account_category, b.position_key,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.vehicle, b.currency,
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
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name, p.asset_class, p.vehicle, p.currency,
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
           pv.position_key, pv.instrument_external_id, pv.symbol, pv.name, pv.asset_class, pv.vehicle, pv.currency,
           pv.quantity, pv.market_value, pv.value_usd, pv.value_chf, pv.value_eur
      FROM pv JOIN hist_active_pos() ap ON ap.src = pv.src AND ap.snap = pv.snap
     ORDER BY as_of_day, pv.src, pv.account_external_id, pv.position_key
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (31, CAST(epoch(now()) AS BIGINT));
