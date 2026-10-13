-- ============================================================
-- gold schema, migration 0126 — the exposure report: every holding
-- looked through to what it holds, each leaf classified
-- (docs/EXPOSURE.md).
--
-- Derived tables, rewritten by the exposure pass at the end of every
-- load (gold.RebuildExposure):
--
--   * exposure_labels, exposure_countries, exposure_sic: the label maps
--     and vocabularies of internal/canonical, so SQL reads them;
--   * fund_line_labels: every label the fund lines carry, per provider,
--     with the value it maps to, NULL where no map knows it;
--   * instrument_keys: the security key of each instrument;
--   * security_aliases: a key read as another, the ISIN a source states
--     beside it first, the US CUSIP rule last;
--   * securities: one row per security, its best-known name, sector,
--     countries, currency and market cap, each with where it came from.
--
-- Macros: exposure_holdings (positions and cash, valued as `holdings
-- global` values them), exposure_comps, exposure_funds_at (the temporal
-- join), exposure_leaves (the look-through) and one
-- report_exposure_<view> per view.
--
-- OR REPLACE and IF NOT EXISTS keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

CREATE TABLE IF NOT EXISTS exposure_labels (
    provider  TEXT NOT NULL,     -- '' for the generic map
    field     TEXT NOT NULL,     -- sector | country | asset_type
    label     TEXT NOT NULL,     -- normalized: lower-case, single spaces
    value     TEXT NOT NULL,
    PRIMARY KEY (provider, field, label)
);

CREATE TABLE IF NOT EXISTS exposure_countries (
    code    TEXT NOT NULL PRIMARY KEY,
    name    TEXT NOT NULL,
    region  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS exposure_sic (
    lo      INTEGER NOT NULL,
    hi      INTEGER NOT NULL,
    sector  TEXT    NOT NULL,
    rank    INTEGER NOT NULL      -- the first range holding a code wins
);

CREATE TABLE IF NOT EXISTS fund_line_labels (
    provider  TEXT NOT NULL,
    field     TEXT NOT NULL,     -- sector | country | asset_type
    raw       TEXT NOT NULL,     -- as the line states it
    value     TEXT,              -- NULL: no map knows the label
    PRIMARY KEY (provider, field, raw)
);

CREATE TABLE IF NOT EXISTS instrument_keys (
    silver_source_id        TEXT NOT NULL,
    instrument_external_id  TEXT NOT NULL,
    security_key            TEXT NOT NULL,
    PRIMARY KEY (silver_source_id, instrument_external_id)
);

CREATE TABLE IF NOT EXISTS security_aliases (
    alias         TEXT NOT NULL PRIMARY KEY,
    security_key  TEXT NOT NULL,
    origin        TEXT NOT NULL      -- fund_line | lookup | instrument | cusip_rule
);

CREATE TABLE IF NOT EXISTS securities (
    security_key          TEXT NOT NULL PRIMARY KEY,
    name                  TEXT,
    currency              TEXT,
    sector                TEXT,
    sector_origin         TEXT,
    risk_country          TEXT,
    risk_country_origin   TEXT,
    domicile              TEXT,
    domicile_origin       TEXT,
    market_cap            DOUBLE,
    market_cap_origin     TEXT,
    shares_outstanding    DOUBLE
);

-- exposure_norm: a label in the form the maps hold it.
CREATE OR REPLACE MACRO exposure_norm(p_label) AS
    lower(trim(regexp_replace(p_label, '\s+', ' ', 'g')));

-- exposure_settings_row: the settings, the defaults where none is
-- stamped yet.
CREATE OR REPLACE MACRO exposure_settings_row() AS TABLE (
    SELECT COALESCE(MAX(max_gap_days), 400) AS max_gap_days,
           COALESCE(MAX(aged_days), 35) AS aged_days,
           COALESCE(MAX(partial_pct), 3.0) AS partial_pct
      FROM exposure_settings
);

-- exposure_holdings: every position and cash line at the instant, each
-- valued in p_ccy line by line exactly as report_accounts values it, so
-- the leaves of the look-through sum to `holdings global`. key is the
-- holding's security key: its key read through holding_aliases and
-- instrument_aliases(), as the gains reports read it, then through
-- instrument_keys and security_aliases. A line no rate converts has no
-- value. An account report_accounts leaves out of the total (positions
-- of which none converts, or cash of which none converts) is left out
-- whole here too.
CREATE OR REPLACE MACRO exposure_holdings(p_asof, p_ccy) AS TABLE (
    WITH lp AS (SELECT silver_source_id, MAX(snapshot_at) AS snap FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    lines AS (
        SELECT p.silver_source_id AS src, p.account_external_id AS acct, p.position_key,
               p.instrument_external_id AS iid, p.asset_class, p.vehicle, p.currency AS ccy,
               p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.silver_source_id AND p.snapshot_at = lp.snap
        UNION ALL
        SELECT silver_source_id, account_external_id, 'cash:' || currency, NULL, 'cash', 'demand_deposit',
               currency, amount, snapshot_at, TRUE
          FROM cash_chosen(p_asof)),
    valued AS (
        SELECT l.*,
               CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate,
                   l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS value
          FROM lines l
          JOIN accounts a ON a.silver_source_id = l.src AND a.account_external_id = l.acct
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)),
    dropped AS (
        SELECT src, acct FROM valued GROUP BY 1, 2
        HAVING (COUNT(*) FILTER (WHERE NOT is_cash) > 0 AND COUNT(value) FILTER (WHERE NOT is_cash) = 0)
            OR (COUNT(*) FILTER (WHERE is_cash) > 0 AND COUNT(value) FILTER (WHERE is_cash) = 0)),
    keyed AS (
        SELECT v.*, COALESCE(ha.to_key, hd.to_key, ia.k) AS akey
          FROM valued v
          ANTI JOIN dropped d ON d.src = v.src AND d.acct = v.acct
          LEFT JOIN holding_aliases ha ON ha.silver_source_id = v.src AND ha.account_external_id = v.acct
               AND ha.from_key = holding_key(v.iid, v.position_key)
          LEFT JOIN holding_aliases hd ON hd.silver_source_id = v.src AND hd.account_external_id = ''
               AND hd.from_key = holding_key(v.iid, v.position_key)
          LEFT JOIN instrument_aliases() ia ON ia.src = v.src AND ia.alias = v.iid)
    SELECT k.src, k.acct, k.position_key, k.iid, k.asset_class, k.vehicle, k.ccy, k.snap, k.is_cash,
           k.value::DOUBLE AS value, il.symbol, il.name,
           COALESCE(sa.security_key, ik1.security_key, ik2.security_key) AS key
      FROM keyed k
      LEFT JOIN instrument_keys ik1 ON ik1.silver_source_id = k.src AND ik1.instrument_external_id = k.akey
      LEFT JOIN instrument_keys ik2 ON ik2.silver_source_id = k.src AND ik2.instrument_external_id = k.iid
      LEFT JOIN security_aliases sa ON sa.alias = COALESCE(ik1.security_key, ik2.security_key)
      LEFT JOIN instrument_labels() il ON il.silver_source_id = k.src AND il.instrument_external_id = k.iid
);

-- exposure_comps: the compositions the look-through reads, one per fund
-- and day, the fund read as its security key. A fund's own holdings come
-- before the index lines a swap fund lists; the collateral basket is
-- never read; one source wins where two state the same day.
CREATE OR REPLACE MACRO exposure_comps() AS TABLE (
    SELECT c.silver_source_id, c.fund_ref AS stored_ref,
           COALESCE(sa.security_key, c.fund_ref) AS fund_ref, c.as_of, c.basis, c.provider,
           c.fund_name, c.currency, c.net_assets::DOUBLE AS net_assets
      FROM fund_compositions c
      LEFT JOIN security_aliases sa ON sa.alias = c.fund_ref
     WHERE c.basis <> 'collateral'
   QUALIFY ROW_NUMBER() OVER (PARTITION BY COALESCE(sa.security_key, c.fund_ref), c.as_of
                              ORDER BY CASE c.basis WHEN 'holdings' THEN 1 ELSE 2 END,
                                       c.silver_source_id, c.fund_ref) = 1
);

-- exposure_attributes_at: per fund, its attributes as of the instant
-- (the latest on or before, else the earliest after).
CREATE OR REPLACE MACRO exposure_attributes_at(p_asof) AS TABLE (
    SELECT COALESCE(sa.security_key, a.fund_ref) AS fund_ref, a.domicile, a.legal_form, a.replication,
           a.hedged_currency, a.lending, a.ter::DOUBLE AS ter, a.index_name
      FROM fund_attributes a
      LEFT JOIN security_aliases sa ON sa.alias = a.fund_ref
   QUALIFY ROW_NUMBER() OVER (PARTITION BY COALESCE(sa.security_key, a.fund_ref)
                              ORDER BY a.as_of > p_asof, abs(a.as_of - p_asof), a.silver_source_id) = 1
);

-- exposure_funds_at: per fund, the composition that describes it at the
-- instant (docs/EXPOSURE.md §4). The fund's own, the latest on or before
-- the instant, else the earliest after, within max_gap_days; a proxy's
-- where the config says always (the fund's own while the proxy has
-- none), or where the fund has none. Leverage applies to a proxy alone.
-- gap_days is how many days older than the instant the composition is,
-- negative for one dated after it. wsum is the sum of the lines' weights
-- in percent, lines the count of them. A fund's attributes are its own,
-- even where a proxy describes its lines: a hedged class stays hedged.
CREATE OR REPLACE MACRO exposure_funds_at(p_asof) AS TABLE (
    WITH s AS (SELECT * FROM exposure_settings_row()),
    c AS MATERIALIZED (SELECT * FROM exposure_comps()),
    px AS (
        SELECT COALESCE(a1.security_key, p.fund_ref) AS fund_ref,
               COALESCE(a2.security_key, p.proxy_ref) AS proxy_ref, p.always, p.leverage
          FROM exposure_proxies p
          LEFT JOIN security_aliases a1 ON a1.alias = p.fund_ref
          LEFT JOIN security_aliases a2 ON a2.alias = p.proxy_ref
        -- Two entries that name one fund by two keys: the one under its
        -- own key wins, else the first.
       QUALIFY ROW_NUMBER() OVER (PARTITION BY COALESCE(a1.security_key, p.fund_ref)
                                  ORDER BY a1.security_key IS NOT NULL, p.fund_ref) = 1),
    near AS (
        SELECT c.fund_ref,
               MAX(c.as_of) FILTER (WHERE c.as_of <= p_asof) AS back,
               MIN(c.as_of) FILTER (WHERE c.as_of > p_asof) AS fwd
          FROM c GROUP BY 1),
    pick AS (
        SELECT n.fund_ref,
               CASE WHEN n.back IS NOT NULL AND (p_asof // 86400) - (n.back // 86400) <= s.max_gap_days THEN n.back
                    WHEN n.fwd IS NOT NULL AND (n.fwd // 86400) - (p_asof // 86400) <= s.max_gap_days THEN n.fwd
               END AS as_of
          FROM near n, s),
    refs AS (SELECT fund_ref FROM pick WHERE as_of IS NOT NULL UNION SELECT fund_ref FROM px),
    res AS (
        SELECT r.fund_ref, px.proxy_ref, COALESCE(px.leverage, 1.0) AS proxy_leverage,
               CASE WHEN px.always AND pp.as_of IS NOT NULL THEN 'proxy'
                    WHEN own.as_of IS NOT NULL THEN 'direct'
                    WHEN pp.as_of IS NOT NULL THEN 'proxy' END AS via,
               own.as_of AS own_as_of, pp.as_of AS proxy_as_of
          FROM refs r
          LEFT JOIN pick own ON own.fund_ref = r.fund_ref AND own.as_of IS NOT NULL
          LEFT JOIN px ON px.fund_ref = r.fund_ref
          LEFT JOIN pick pp ON pp.fund_ref = px.proxy_ref AND pp.as_of IS NOT NULL),
    chosen AS MATERIALIZED (
        SELECT res.fund_ref, res.via, res.proxy_ref,
               CASE res.via WHEN 'proxy' THEN res.proxy_leverage ELSE 1.0 END AS leverage,
               c.silver_source_id, c.stored_ref, c.as_of, c.basis, c.provider, c.fund_name, c.currency,
               c.net_assets
          FROM res
          JOIN c ON c.fund_ref = CASE res.via WHEN 'proxy' THEN res.proxy_ref ELSE res.fund_ref END
                AND c.as_of = CASE res.via WHEN 'proxy' THEN res.proxy_as_of ELSE res.own_as_of END
         WHERE res.via IS NOT NULL),
    w AS (
        SELECT l.silver_source_id, l.fund_ref AS stored_ref, l.as_of, l.basis,
               COALESCE(SUM(l.weight), 0)::DOUBLE AS wsum, COUNT(*) AS lines
          FROM fund_lines l
          SEMI JOIN (SELECT DISTINCT silver_source_id, stored_ref, as_of, basis FROM chosen) ch
               ON ch.silver_source_id = l.silver_source_id AND ch.stored_ref = l.fund_ref
              AND ch.as_of = l.as_of AND ch.basis = l.basis
         GROUP BY 1, 2, 3, 4)
    SELECT ch.fund_ref, ch.via, ch.as_of, ch.proxy_ref, ch.leverage,
           (p_asof // 86400) - (ch.as_of // 86400) AS gap_days,
           ch.as_of > p_asof AS forward,
           (p_asof // 86400) - (ch.as_of // 86400) > s.aged_days AS aged,
           abs(100 - COALESCE(w.wsum, 0)) > s.partial_pct AS partial,
           ch.silver_source_id, ch.stored_ref, ch.basis, ch.provider, ch.fund_name, ch.currency, ch.net_assets,
           COALESCE(w.wsum, 0) AS wsum, COALESCE(w.lines, 0) AS lines,
           a.hedged_currency, a.domicile, a.legal_form, a.replication, a.lending, a.ter, a.index_name
      FROM chosen ch
      LEFT JOIN w USING (silver_source_id, stored_ref, as_of, basis)
      CROSS JOIN s
      LEFT JOIN exposure_attributes_at(p_asof) a ON a.fund_ref = ch.fund_ref
);

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
               l.short_currency IS NOT NULL OR lt.value = 'foreign_exchange' AS fx_line,
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
               h.vehicle = 'forward' AS fx_line, FALSE AS class_unmapped,
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

-- exposure_quality: an aggregate over a group of leaves, each flag the
-- group's leaves carry with the share of the group's gross value it
-- covers ("proxy=12%", "aged=<1%"), comma-separated in a fixed order.
CREATE OR REPLACE MACRO exposure_flag_share(p_flag, p_part, p_total) AS
    CASE WHEN COALESCE(p_part, 0) > 0 AND p_total > 0 THEN
         p_flag || '=' || CASE WHEN 100 * p_part / p_total < 1 THEN '<1'
                               ELSE CAST(round(100 * p_part / p_total) AS INTEGER)::VARCHAR END || '%' END;

CREATE OR REPLACE MACRO exposure_quality(p_flags, p_value) AS
    array_to_string(list_filter([
        exposure_flag_share('proxy', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'proxy')), SUM(abs(p_value))),
        exposure_flag_share('aged', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'aged')), SUM(abs(p_value))),
        exposure_flag_share('forward', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'forward')), SUM(abs(p_value))),
        exposure_flag_share('partial', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'partial')), SUM(abs(p_value))),
        exposure_flag_share('unmapped', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'unmapped')), SUM(abs(p_value))),
        exposure_flag_share('untagged', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'untagged')), SUM(abs(p_value))),
        exposure_flag_share('unmatched', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'unmatched')), SUM(abs(p_value)))
    ], lambda x: x IS NOT NULL), ',');

-- The views. Each groups the leaves of one instant; share_pct is a share
-- of net worth (the sum of every leaf), direct_pct and via_funds_pct its
-- split into what is held directly and what through funds.
CREATE OR REPLACE MACRO report_exposure_classes(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT l.asset_class, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l, t GROUP BY l.asset_class, t.total
     ORDER BY value DESC, asset_class
);

CREATE OR REPLACE MACRO report_exposure_sectors(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l),
    c AS (SELECT asset_class, SUM(value) AS class_total FROM l GROUP BY 1)
    SELECT l.asset_class, COALESCE(l.sector, 'unclassified') AS sector, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * SUM(l.value) / NULLIF(c.class_total, 0) AS class_share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l JOIN c USING (asset_class), t
     GROUP BY l.asset_class, COALESCE(l.sector, 'unclassified'), t.total, c.class_total
     ORDER BY c.class_total DESC, l.asset_class, value DESC, sector
);

CREATE OR REPLACE MACRO report_exposure_countries(p_asof, p_ccy, p_country := 'risk') AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy, p_country := p_country)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT CASE WHEN l.country IS NOT NULL THEN l.country
                WHEN l.asset_class IN ('metal', 'crypto') THEN '(none)' ELSE 'unclassified' END AS country,
           MAX(l.country_name) AS country_name, MAX(l.region) AS region, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l, t GROUP BY 1, t.total
     ORDER BY value DESC, country
);

CREATE OR REPLACE MACRO report_exposure_currencies(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT COALESCE(l.currency, 'unclassified') AS currency, l.currency_kind, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l, t GROUP BY 1, 2, t.total
     ORDER BY value DESC, currency
);

-- report_exposure_securities: one row per security, held directly and
-- through every fund. A leaf with no security is a row of its own: cash
-- per currency (a balance, or a fund's cash line that names nothing
-- else), any other fund line by its fund and name, a holding by its
-- instrument, a remainder or leverage leg by its fund.
CREATE OR REPLACE MACRO report_exposure_securities(p_asof, p_ccy, p_country := 'risk') AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy, p_country := p_country)),
    t AS (SELECT SUM(value) AS total FROM l),
    g AS (
        SELECT l.*,
               CASE WHEN l.kind IN ('remainder', 'leverage') THEN '(' || l.kind || ') ' || l.fund_ref
                    WHEN l.asset_class = 'cash' AND (l.key IS NULL OR l.key LIKE 'name:%') THEN 'cash:' || l.currency
                    WHEN l.key IS NOT NULL THEN l.key
                    WHEN l.depth > 0 THEN 'line:' || l.fund_ref || ':' || COALESCE(l.name, l.asset_type_raw, '')
                    ELSE 'holding:' || l.src || ':' || COALESCE(l.iid, l.position_key) END AS security
          FROM l)
    SELECT g.security, MAX(g.name) AS name, MAX(g.ticker) AS ticker,
           arg_max(g.asset_class, abs(g.value)) AS asset_class,
           arg_max(g.sector, abs(g.value)) AS sector, arg_max(g.country, abs(g.value)) AS country,
           arg_max(g.currency, abs(g.value)) AS currency, MAX(g.market_cap) AS market_cap,
           SUM(g.value) AS value,
           COALESCE(SUM(g.value) FILTER (WHERE g.depth = 0), 0) AS direct_value,
           COALESCE(SUM(g.value) FILTER (WHERE g.depth > 0), 0) AS via_funds_value,
           COUNT(DISTINCT g.top_key) FILTER (WHERE g.depth > 0) AS funds,
           100 * SUM(g.value) / NULLIF(t.total, 0) AS share_pct,
           exposure_quality(g.flags, g.value) AS quality
      FROM g, t GROUP BY g.security, t.total
     ORDER BY abs(SUM(g.value)) DESC, security
);

-- report_exposure_funds: one row per fund held directly, and one more
-- for all other holdings together, so the view sums to net worth.
CREATE OR REPLACE MACRO report_exposure_funds(p_asof, p_ccy) AS TABLE (
    WITH h AS (SELECT * FROM exposure_holdings(p_asof, p_ccy) WHERE value IS NOT NULL),
    t AS (SELECT SUM(value) AS total FROM h),
    fa AS (SELECT * FROM exposure_funds_at(p_asof)),
    k AS (
        SELECT h.*, fa.fund_ref IS NOT NULL AS described
          FROM h LEFT JOIN fa ON fa.fund_ref = h.key),
    f AS (
        SELECT CASE WHEN k.described OR k.vehicle IN ('etf', 'fund') THEN COALESCE(k.key, 'holding:' || k.src || ':' || COALESCE(k.iid, k.position_key))
                    ELSE '(other holdings)' END AS fund,
               k.* FROM k)
    SELECT f.fund, MAX(f.name) FILTER (WHERE f.fund <> '(other holdings)') AS name,
           SUM(f.value) AS value, 100 * SUM(f.value) / NULLIF(t.total, 0) AS share_pct,
           MAX(fa.via) AS via, MAX(fa.proxy_ref) FILTER (WHERE fa.via = 'proxy') AS proxy,
           MAX(fa.leverage) FILTER (WHERE fa.via = 'proxy') AS leverage,
           MAX(fa.provider) AS provider, MAX(fa.basis) AS basis, MAX(fa.as_of) AS composition_as_of,
           MAX(fa.gap_days) AS gap_days, MAX(fa.lines) AS lines, MAX(fa.wsum) AS weight_sum,
           MAX(fa.domicile) AS domicile, MAX(fa.replication) AS replication, BOOL_OR(fa.lending) AS lending,
           MAX(fa.hedged_currency) AS hedged_currency,
           array_to_string(list_sort(list_distinct(list_filter([
               CASE WHEN BOOL_OR(fa.via = 'proxy') THEN 'proxy' END,
               CASE WHEN BOOL_OR(fa.aged) THEN 'aged' END,
               CASE WHEN BOOL_OR(fa.forward) THEN 'forward' END,
               CASE WHEN BOOL_OR(fa.partial) THEN 'partial' END,
               CASE WHEN f.fund <> '(other holdings)' AND MAX(fa.fund_ref) IS NULL THEN 'no_composition' END],
               lambda x: x IS NOT NULL))), ',') AS quality
      FROM f LEFT JOIN fa ON fa.fund_ref = f.key, t
     GROUP BY f.fund, t.total
     ORDER BY f.fund = '(other holdings)', abs(SUM(f.value)) DESC, f.fund
);

-- report_exposure_coverage: what the figures rest on. Section `how`
-- splits net worth by how each leaf is classified and sums to it;
-- `age` splits what compositions describe by their distance from the
-- day; `unmapped` lists the provider labels no map knows; `unmatched`
-- the securities held directly that no source describes.
CREATE OR REPLACE MACRO report_exposure_coverage(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l),
    how AS (
        SELECT 1 AS section_rank, 'how' AS section, how AS reason, COUNT(*) AS items, SUM(value) AS value,
               CAST(NULL AS VARCHAR) AS detail,
               CASE how WHEN 'looked_through' THEN 1 WHEN 'proxied' THEN 2 WHEN 'security' THEN 3
                        WHEN 'tagged' THEN 4 WHEN 'cash' THEN 5 WHEN 'asset_class' THEN 6
                        WHEN 'remainder' THEN 7 WHEN 'truncated' THEN 8 ELSE 9 END AS reason_rank
          FROM l GROUP BY how),
    age AS (
        SELECT 2, 'age', bucket, COUNT(DISTINCT top_key), SUM(value), CAST(NULL AS VARCHAR), bucket_rank
          FROM (SELECT *,
                       CASE WHEN forward THEN 'forward'
                            WHEN abs(gap_days) <= 35 THEN '0-35 days'
                            WHEN abs(gap_days) <= 95 THEN '36-95 days'
                            WHEN abs(gap_days) <= 185 THEN '96-185 days'
                            ELSE '186+ days' END AS bucket,
                       CASE WHEN forward THEN 5 WHEN abs(gap_days) <= 35 THEN 1 WHEN abs(gap_days) <= 95 THEN 2
                            WHEN abs(gap_days) <= 185 THEN 3 ELSE 4 END AS bucket_rank
                  FROM l WHERE depth > 0)
         GROUP BY bucket, bucket_rank),
    unmapped AS (
        SELECT 3, 'unmapped', f.field || ': ' || f.raw, COUNT(*), SUM(f.value), f.provider, 0
          FROM (SELECT 'sector' AS field, sector_raw AS raw, provider, value FROM l
                 WHERE depth > 0 AND sector_raw IS NOT NULL AND sector_map IS NULL
                UNION ALL
                SELECT 'country', country_raw, provider, value FROM l
                 WHERE depth > 0 AND country_raw IS NOT NULL AND country_map IS NULL
                UNION ALL
                SELECT 'asset_type', asset_type_raw, provider, value FROM l
                 WHERE depth > 0 AND class_unmapped) f
         GROUP BY f.field, f.raw, f.provider),
    unmatched AS (
        SELECT 4, 'unmatched', holding, COUNT(*), SUM(value), MAX(name), 0
          FROM (SELECT *, COALESCE(key, 'holding:' || src || ':' || COALESCE(iid, position_key)) AS holding FROM l
                 WHERE list_contains(flags, 'unmatched') OR list_contains(flags, 'untagged'))
         GROUP BY holding)
    SELECT section, reason, items, value, 100 * value / NULLIF(t.total, 0) AS share_pct, detail
      FROM (SELECT * FROM how UNION ALL SELECT * FROM age UNION ALL SELECT * FROM unmapped
            UNION ALL SELECT * FROM unmatched) x, t
     ORDER BY section_rank, reason_rank, abs(value) DESC, reason
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (126, CAST(epoch(now()) AS BIGINT));
