-- ============================================================
-- gold schema, migration 0154 — the size of a share held directly,
-- for the dashboards' position lists.
--
-- web_positions_history labels a position by symbol, name, asset class
-- and vehicle, and carries no security key. web_position_sizes gives
-- each such label of a public-equity stock its share's size (large,
-- mid, small) from securities, as the exposure leaves take it. A
-- position's security key is the one exposure_holdings reads: the
-- holding alias, else the instrument alias, else the instrument itself,
-- through instrument_keys and exposure_key. A key the look-through
-- expands, as a fund's, is no share and has no size. A label over its
-- whole history may name several securities (a new ISIN after a
-- corporate action): it takes the size its sized securities agree on,
-- and none where they differ. A fund, an option, a private share or any
-- other vehicle has no row.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

CREATE OR REPLACE VIEW web_position_sizes AS
    WITH lines AS (
        SELECT DISTINCT silver_source_id AS src, account_external_id AS acct, position_key,
               instrument_external_id AS iid, symbol, name, asset_class, vehicle
          FROM serving_positions_history
         WHERE vehicle = 'stock' AND asset_class = 'public_equity'),
    keyed AS (
        SELECT l.*, COALESCE(ha.to_key, hd.to_key, ia.k) AS akey
          FROM lines l
          LEFT JOIN holding_aliases ha ON ha.silver_source_id = l.src AND ha.account_external_id = l.acct
               AND ha.from_key = holding_key(l.iid, l.position_key)
          LEFT JOIN holding_aliases hd ON hd.silver_source_id = l.src AND hd.account_external_id = ''
               AND hd.from_key = holding_key(l.iid, l.position_key)
          LEFT JOIN instrument_aliases() ia ON ia.src = l.src AND ia.alias = l.iid),
    secs AS (
        SELECT k.symbol, k.name, k.asset_class, k.vehicle,
               exposure_key(COALESCE(ik1.security_key, ik2.security_key)) AS security_key
          FROM keyed k
          LEFT JOIN instrument_keys ik1 ON ik1.silver_source_id = k.src AND ik1.instrument_external_id = k.akey
          LEFT JOIN instrument_keys ik2 ON ik2.silver_source_id = k.src AND ik2.instrument_external_id = k.iid)
    SELECT x.symbol, x.name, x.asset_class, x.vehicle,
           CASE WHEN COUNT(DISTINCT s.size) = 1 THEN MAX(s.size) END AS size
      FROM secs x
      LEFT JOIN securities s ON s.security_key = x.security_key
           AND s.security_key NOT IN (SELECT fund_ref FROM exposure_fund_keys())
     GROUP BY 1, 2, 3, 4;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (154, CAST(epoch(now()) AS BIGINT));
