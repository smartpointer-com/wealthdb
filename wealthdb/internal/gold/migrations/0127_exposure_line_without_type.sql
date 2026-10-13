-- ============================================================
-- gold schema, migration 0127 — exposure_leaves reads a fund line with
-- no asset type.
--
-- In 0126 a line whose asset type is absent or no map knows leaves
-- fx_line NULL, and with it the line's value: the leaf drops out of
-- every view, which then sums to less than `holdings global`. Here
-- fx_line is false unless a line states a short currency or maps to
-- foreign_exchange. The macro is otherwise 0126's.
--
-- OR REPLACE keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

-- exposure_leaves: the look-through at an instant (docs/EXPOSURE.md §5).
-- One row per leaf: a holding no composition describes, a fund's line, a
-- remainder (what a fund's weights leave unexplained) or the cash leg of
-- a leveraged proxy. Each carries its value in p_ccy, its class, sector,
-- country, region and currency, the quality flags it rests on and the
-- reason it is classified (`how`). The leaves of an instant sum to
-- `holdings global`. p_country: 'risk' or 'domicile'. Only the funds a
-- holding reaches within three levels are expanded; a fund the third
-- level still names stays a leaf, flagged partial.
CREATE OR REPLACE MACRO exposure_leaves(p_asof, p_ccy, p_country := 'risk') AS TABLE (
    WITH RECURSIVE
    h AS MATERIALIZED (SELECT * FROM exposure_holdings(p_asof, p_ccy) WHERE value IS NOT NULL),
    fa_all AS MATERIALIZED (SELECT * FROM exposure_funds_at(p_asof)),
    -- r1 to r3: the funds a holding reaches at each level, a line's key
    -- read through the aliases.
    r1 AS (SELECT fund_ref FROM fa_all WHERE fund_ref IN (SELECT key FROM h)),
    r2 AS (
        SELECT DISTINCT f2.fund_ref
          FROM fa_all f1 SEMI JOIN r1 ON r1.fund_ref = f1.fund_ref
          JOIN fund_lines l ON l.silver_source_id = f1.silver_source_id AND l.fund_ref = f1.stored_ref
               AND l.as_of = f1.as_of AND l.basis = f1.basis
          LEFT JOIN security_aliases sa ON sa.alias = l.security_key
          JOIN fa_all f2 ON f2.fund_ref = COALESCE(sa.security_key, l.security_key)),
    r3 AS (
        SELECT DISTINCT f2.fund_ref
          FROM fa_all f1 SEMI JOIN r2 ON r2.fund_ref = f1.fund_ref
          JOIN fund_lines l ON l.silver_source_id = f1.silver_source_id AND l.fund_ref = f1.stored_ref
               AND l.as_of = f1.as_of AND l.basis = f1.basis
          LEFT JOIN security_aliases sa ON sa.alias = l.security_key
          JOIN fa_all f2 ON f2.fund_ref = COALESCE(sa.security_key, l.security_key)),
    fa AS MATERIALIZED (
        SELECT * FROM fa_all
         WHERE fund_ref IN (SELECT fund_ref FROM r1 UNION SELECT fund_ref FROM r2 UNION SELECT fund_ref FROM r3)),
    kids AS MATERIALIZED (
        SELECT fa.fund_ref, 'line' AS kind, fa.leverage * l.weight::DOUBLE / 100 AS factor,
               COALESCE(sa.security_key, l.security_key) AS key, l.name, l.ticker, l.currency AS line_ccy,
               l.asset_type AS asset_type_raw, lt.value AS class_map,
               l.sector AS sector_raw, ls.value AS sector_map,
               COALESCE(l.risk_country, l.country) AS country_raw, lc.value AS country_map,
               l.long_currency, l.long_amount::DOUBLE AS long_amount, l.short_currency,
               COALESCE(l.short_currency IS NOT NULL OR lt.value = 'foreign_exchange', FALSE) AS fx_line,
               fa.as_of, fa.via, fa.gap_days, fa.forward, fa.aged, FALSE AS partial, fa.hedged_currency,
               fa.currency AS fund_ccy, fa.net_assets, fa.leverage, fa.provider
          FROM fa
          JOIN fund_lines l ON l.silver_source_id = fa.silver_source_id AND l.fund_ref = fa.stored_ref
               AND l.as_of = fa.as_of AND l.basis = fa.basis
          LEFT JOIN security_aliases sa ON sa.alias = l.security_key
          LEFT JOIN fund_line_labels lt ON lt.provider = fa.provider AND lt.field = 'asset_type' AND lt.raw = l.asset_type
          LEFT JOIN fund_line_labels ls ON ls.provider = fa.provider AND ls.field = 'sector' AND ls.raw = l.sector
          LEFT JOIN fund_line_labels lc ON lc.provider = fa.provider AND lc.field = 'country'
               AND lc.raw = COALESCE(l.risk_country, l.country)
         WHERE l.weight IS NOT NULL
        UNION ALL
        SELECT fa.fund_ref, 'remainder', fa.leverage * (100 - fa.wsum) / 100, NULL, NULL, NULL, NULL,
               NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, FALSE,
               fa.as_of, fa.via, fa.gap_days, fa.forward, fa.aged, fa.partial, fa.hedged_currency,
               fa.currency, fa.net_assets, fa.leverage, fa.provider
          FROM fa WHERE fa.wsum <> 100
        UNION ALL
        SELECT fa.fund_ref, 'leverage', 1 - fa.leverage, NULL, NULL, NULL, NULL,
               NULL, 'cash', NULL, NULL, NULL, NULL, NULL, NULL, NULL, FALSE,
               fa.as_of, fa.via, fa.gap_days, fa.forward, fa.aged, FALSE, fa.hedged_currency,
               fa.currency, fa.net_assets, fa.leverage, fa.provider
          FROM fa WHERE fa.leverage <> 1),
    tree AS (
        SELECT 0 AS depth, 'holding' AS kind, h.src, h.acct, h.position_key, h.iid,
               h.asset_class AS h_class, h.vehicle AS h_vehicle, h.ccy AS h_ccy, h.is_cash,
               h.name AS h_name, h.symbol AS h_symbol, h.key AS top_key, h.key AS key,
               CAST(NULL AS VARCHAR) AS fund_ref, h.value, CAST(NULL AS DOUBLE) AS fund_value,
               h.asset_class AS class, h.name AS name, h.symbol AS ticker, h.ccy AS line_ccy,
               CAST(NULL AS VARCHAR) AS asset_type_raw, CAST(NULL AS VARCHAR) AS sector_raw,
               CAST(NULL AS VARCHAR) AS sector_map, CAST(NULL AS VARCHAR) AS country_raw,
               CAST(NULL AS VARCHAR) AS country_map, CAST(NULL AS VARCHAR) AS long_currency,
               CAST(NULL AS DOUBLE) AS long_amount, CAST(NULL AS VARCHAR) AS short_currency,
               COALESCE(h.vehicle = 'forward', FALSE) AS fx_line, FALSE AS class_unmapped,
               CAST(NULL AS BIGINT) AS comp_as_of, CAST(NULL AS VARCHAR) AS via,
               CAST(NULL AS BIGINT) AS gap_days, FALSE AS forward, FALSE AS aged, FALSE AS partial,
               CAST(NULL AS VARCHAR) AS hedged, CAST(NULL AS VARCHAR) AS fund_ccy,
               CAST(NULL AS DOUBLE) AS net_assets, CAST(NULL AS DOUBLE) AS leverage,
               CAST(NULL AS VARCHAR) AS provider
          FROM h
        UNION ALL
        SELECT t.depth + 1, k.kind, t.src, t.acct, t.position_key, t.iid, t.h_class, t.h_vehicle, t.h_ccy,
               t.is_cash, t.h_name, t.h_symbol, t.top_key, k.key, k.fund_ref, t.value * k.factor, t.value,
               COALESCE(k.class_map, t.class), k.name, k.ticker, k.line_ccy, k.asset_type_raw, k.sector_raw,
               k.sector_map, k.country_raw, k.country_map, k.long_currency, k.long_amount, k.short_currency,
               k.fx_line,
               k.asset_type_raw IS NOT NULL AND k.class_map IS NULL,
               COALESCE(t.comp_as_of, k.as_of),
               CASE WHEN t.via = 'proxy' OR k.via = 'proxy' THEN 'proxy' ELSE 'direct' END,
               GREATEST(COALESCE(t.gap_days, 0), abs(k.gap_days)),
               t.forward OR k.forward, t.aged OR k.aged, k.partial,
               COALESCE(t.hedged, k.hedged_currency), k.fund_ccy, k.net_assets, k.leverage, k.provider
          FROM tree t JOIN kids k ON k.fund_ref = t.key
         WHERE t.depth < 3 AND t.kind IN ('holding', 'line')),
    leaves AS (
        SELECT t.*,
               COALESCE(t.depth = 3 AND t.kind = 'line' AND t.key IN (SELECT fund_ref FROM fa_all), FALSE) AS truncated
          FROM tree t
         WHERE NOT (t.depth < 3 AND t.kind IN ('holding', 'line')
                    AND COALESCE(t.key IN (SELECT fund_ref FROM fa), FALSE))),
    -- A currency forward line that states its long leg splits in two: the
    -- long leg at notional in its currency, the rest (the short leg, its
    -- value less the long leg) in the short currency, else the line's
    -- own, so the pair sums to the line. Without a long leg it counts in
    -- its own currency, flagged partial.
    legs AS (
        SELECT l.*, COALESCE(CASE WHEN l.long_currency = l.fund_ccy THEN 1.0 END, r1.rate, u1.rate * u2.rate) AS long_rate
          FROM leaves l
          ASOF LEFT JOIN fx_daily r1 ON r1.from_ccy = l.long_currency AND r1.to_ccy = l.fund_ccy
               AND r1.day <= (l.comp_as_of // 86400)
          ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = l.long_currency AND u1.to_ccy = 'USD'
               AND u1.day <= (l.comp_as_of // 86400)
          ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = l.fund_ccy
               AND u2.day <= (l.comp_as_of // 86400)),
    split AS (
        SELECT legs.*, FALSE AS long_leg,
               legs.fx_line AND (legs.long_amount IS NULL OR legs.long_rate IS NULL OR legs.net_assets IS NULL
                                 OR legs.net_assets = 0) AS fx_unsplit
          FROM legs
        UNION ALL
        SELECT legs.*, TRUE, FALSE
          FROM legs
         WHERE legs.fx_line AND legs.long_amount IS NOT NULL AND legs.long_rate IS NOT NULL
           AND legs.net_assets IS NOT NULL AND legs.net_assets <> 0),
    -- Every key known as a fund: one with a composition or attributes.
    known AS (
        SELECT fund_ref FROM exposure_comps()
        UNION SELECT COALESCE(sa.security_key, a.fund_ref) FROM fund_attributes a
          LEFT JOIN security_aliases sa ON sa.alias = a.fund_ref),
    valued AS (
        SELECT s.*,
               CASE WHEN s.fx_line AND NOT s.fx_unsplit THEN
                        s.fund_value * s.leverage * s.long_amount * s.long_rate / s.net_assets END AS long_value,
               -- A fund that stays a leaf: held with no composition, or
               -- a line no level expands. Where it is domiciled says
               -- nothing of where its value is.
               COALESCE((s.depth = 0 AND s.h_vehicle IN ('etf', 'fund'))
                        OR (s.depth > 0 AND s.key IN (SELECT fund_ref FROM known)), FALSE) AS fund_leaf
          FROM split s),
    cls AS (
        SELECT v.depth, v.kind, v.src, v.acct, v.position_key, v.iid, v.h_class, v.h_vehicle, v.h_name,
               v.h_symbol, v.top_key, v.fund_ref, v.key, v.comp_as_of, v.via, v.gap_days, v.forward, v.aged,
               v.partial, v.truncated, v.class_unmapped, v.fx_unsplit, v.provider, v.is_cash,
               v.asset_type_raw, v.sector_raw, v.country_raw, v.sector_map, v.country_map,
               CASE WHEN NOT (v.fx_line AND NOT v.fx_unsplit) THEN v.value
                    WHEN v.long_leg THEN v.long_value ELSE v.value - v.long_value END AS value,
               CASE WHEN v.long_leg THEN 'foreign_exchange' ELSE v.class END AS asset_class,
               COALESCE(sec.name, v.name, v.h_name) AS name, v.ticker,
               sec.market_cap,
               CASE WHEN v.kind = 'remainder' THEN NULL
                    WHEN v.kind = 'leverage' OR v.class = 'cash' THEN 'cash'
                    WHEN v.class = 'metal' THEN 'metal'
                    WHEN v.class = 'crypto' THEN 'crypto'
                    WHEN v.depth = 0 AND tg.sector IS NOT NULL THEN tg.sector
                    WHEN v.fund_leaf THEN CASE WHEN v.class = 'real_estate' THEN 'real_estate' END
                    ELSE COALESCE(sec.sector, v.sector_map, CASE WHEN v.class = 'real_estate' THEN 'real_estate' END)
               END AS sector,
               CASE WHEN v.kind IN ('remainder', 'leverage') OR v.class IN ('metal', 'crypto') THEN NULL
                    WHEN v.depth = 0 AND tg.country IS NOT NULL THEN tg.country
                    WHEN v.depth = 0 AND v.is_cash THEN cu.country
                    WHEN v.fund_leaf THEN NULL
                    WHEN p_country = 'domicile' THEN COALESCE(sec.domicile, sec.risk_country, v.country_map)
                    ELSE COALESCE(sec.risk_country, v.country_map, sec.domicile)
               END AS country,
               CASE WHEN v.long_leg THEN v.long_currency
                    WHEN v.fx_line AND NOT v.fx_unsplit THEN COALESCE(v.short_currency, v.line_ccy)
                    WHEN v.fx_line THEN COALESCE(v.line_ccy, v.fund_ccy, v.h_ccy)
                    WHEN v.depth > 0 AND v.hedged IS NOT NULL THEN v.hedged
                    WHEN v.class = 'metal' THEN
                         CASE WHEN regexp_matches(lower(COALESCE(v.name, v.h_name, '')), 'silver') THEN 'XAG'
                              WHEN regexp_matches(lower(COALESCE(v.name, v.h_name, '')), 'platinum') THEN 'XPT'
                              WHEN regexp_matches(lower(COALESCE(v.name, v.h_name, '')), 'palladium') THEN 'XPD'
                              ELSE 'XAU' END
                    -- A coin held in a wallet is its symbol; a coin in a
                    -- product or a fund is the one its name states.
                    WHEN v.class = 'crypto' THEN
                         CASE WHEN v.depth = 0 AND v.h_vehicle = 'physical' THEN upper(v.h_symbol)
                              WHEN regexp_matches(lower(COALESCE(v.name, v.h_name, '')), '\bbitcoin\b') THEN 'BTC'
                              WHEN regexp_matches(lower(COALESCE(v.name, v.h_name, '')), '\bether(eum)?\b') THEN 'ETH'
                         END
                    WHEN v.depth = 0 THEN COALESCE(tg.currency, v.line_ccy)
                    WHEN v.kind IN ('remainder', 'leverage') THEN COALESCE(v.fund_ccy, v.h_ccy)
                    ELSE COALESCE(v.line_ccy, sec.currency, v.fund_ccy, v.h_ccy)
               END AS currency,
               CASE WHEN v.fx_line OR (v.depth > 0 AND v.hedged IS NOT NULL) THEN 'fiat'
                    WHEN v.class = 'metal' THEN 'metal' WHEN v.class = 'crypto' THEN 'crypto'
                    ELSE 'fiat' END AS currency_kind,
               tg.silver_source_id IS NOT NULL AS tagged,
               -- A listed security, or a fund of a listed kind: a source
               -- can describe it. Anything else is opaque.
               v.h_vehicle IN ('stock', 'bond')
                   OR (v.h_vehicle IN ('etf', 'fund')
                       AND v.h_class IN ('public_equity', 'fixed_income', 'multi_asset', 'infrastructure', 'cash', 'other'))
                   AS public,
               -- What a source states, not what an ISIN's prefix implies;
               -- nothing for a fund no composition describes.
               NOT v.fund_leaf
                   AND (sec.sector IS NOT NULL OR sec.risk_country IS NOT NULL
                        OR (sec.domicile IS NOT NULL AND sec.domicile_origin <> 'isin_prefix')) AS described
          FROM valued v
          LEFT JOIN securities sec ON sec.security_key = v.key
          LEFT JOIN exposure_tags tg ON v.depth = 0 AND tg.silver_source_id = v.src AND tg.instrument_external_id = v.iid
          LEFT JOIN exposure_custody cu ON cu.silver_source_id = v.src)
    SELECT c.*,
           co.name AS country_name, co.region,
           CASE WHEN c.depth = 0 AND c.is_cash THEN 'cash'
                WHEN c.depth = 0 AND c.asset_class IN ('metal', 'crypto') THEN 'asset_class'
                WHEN c.depth = 0 AND c.tagged THEN 'tagged'
                WHEN c.depth = 0 AND c.public AND c.described THEN 'security'
                WHEN c.depth = 0 THEN 'unclassified'
                WHEN c.kind = 'remainder' THEN 'remainder'
                WHEN c.truncated THEN 'truncated'
                WHEN c.via = 'proxy' THEN 'proxied'
                ELSE 'looked_through' END AS how,
           list_filter([
               CASE WHEN c.via = 'proxy' THEN 'proxy' END,
               CASE WHEN c.aged THEN 'aged' END,
               CASE WHEN c.forward THEN 'forward' END,
               CASE WHEN (c.kind = 'remainder' AND c.partial) OR c.fx_unsplit OR c.truncated THEN 'partial' END,
               CASE WHEN c.depth > 0 AND (c.class_unmapped
                         OR (c.sector_raw IS NOT NULL AND c.sector_map IS NULL AND c.sector IS NULL)
                         OR (c.country_raw IS NOT NULL AND c.country_map IS NULL AND c.country IS NULL))
                    THEN 'unmapped' END,
               CASE WHEN c.depth = 0 AND NOT c.is_cash AND c.asset_class NOT IN ('metal', 'crypto', 'cash')
                         AND NOT c.public AND (c.sector IS NULL OR c.country IS NULL) THEN 'untagged' END,
               CASE WHEN c.depth = 0 AND c.public AND NOT c.tagged
                         AND (c.sector IS NULL OR c.country IS NULL) THEN 'unmatched' END
           ], lambda x: x IS NOT NULL) AS flags
      FROM cls c
      LEFT JOIN exposure_countries co ON co.code = c.country
     WHERE c.value IS NOT NULL
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (127, CAST(epoch(now()) AS BIGINT));
