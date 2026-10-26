-- ============================================================
-- gold schema, migration 0156 — report joins that hash.
--
-- A LEFT JOIN whose ON clause tests the preserved side alone, or ORs
-- a test of it with one of the other side, runs as a nested loop:
-- each row of one side meets each row of the other. The joins below
-- carry such a test in their key instead. A NULL key matches nothing,
-- as the failed test did, so each macro returns the same rows.
--
--   * exposure_leaves: a holding's declared parts. A currency forward
--     line's long-leg rate is looked up once per long currency, fund
--     currency and day, not once per leaf: the three as-of lookups ran
--     over every leaf of the walk, though only forward lines have one.
--     The rate joins back on those three, a missing fund currency or
--     day matching its like, so each line keeps the rate it had.
--   * exposure_credit_leaves, exposure_maturity_leaves: a page fund's
--     breakdown.
--   * report_exposure_funds: a declared fund's attributes. A holding's
--     composition joins on the key alone, and its declaration is
--     tested after the join, as exposure_leaves tests it.
--
-- Each macro is re-issued whole; only those joins change.
-- ============================================================

-- exposure_leaves: 0141's, a holding's declared parts joined by a key.
CREATE OR REPLACE MACRO exposure_leaves(p_asof, p_ccy, p_country := 'risk') AS TABLE (
    WITH RECURSIVE
    fa_all AS MATERIALIZED (SELECT * FROM exposure_funds_at(p_asof)),
    -- decl: the record that describes the holding. expands: a composition
    -- does. Where a declaration describes it too, only if the fund has one
    -- of its own; an always-proxy then still stands in for it.
    h AS MATERIALIZED (
        SELECT h.*, d.line_no AS decl,
               f.fund_ref IS NOT NULL AND (d.line_no IS NULL OR f.own_as_of IS NOT NULL) AS expands
          FROM exposure_holdings(p_asof, p_ccy) h
          LEFT JOIN exposure_declarations_at(p_asof) d
                 ON d.silver_source_id = h.src AND d.instrument_external_id = h.iid
          LEFT JOIN fa_all f ON f.fund_ref = h.key
         WHERE h.value IS NOT NULL),
    -- r1 to r3: the funds a holding reaches at each level, a line's key
    -- read through the aliases.
    r1 AS (SELECT DISTINCT key AS fund_ref FROM h WHERE expands),
    r2 AS (
        SELECT DISTINCT f2.fund_ref
          FROM (SELECT exposure_key(l.security_key) AS key
                  FROM fa_all f1 SEMI JOIN r1 ON r1.fund_ref = f1.fund_ref
                  JOIN fund_lines l ON l.silver_source_id = f1.silver_source_id AND l.fund_ref = f1.stored_ref
                       AND l.as_of = f1.as_of AND l.basis = f1.basis) x
          JOIN fa_all f2 ON f2.fund_ref = x.key),
    r3 AS (
        SELECT DISTINCT f2.fund_ref
          FROM (SELECT exposure_key(l.security_key) AS key
                  FROM fa_all f1 SEMI JOIN r2 ON r2.fund_ref = f1.fund_ref
                  JOIN fund_lines l ON l.silver_source_id = f1.silver_source_id AND l.fund_ref = f1.stored_ref
                       AND l.as_of = f1.as_of AND l.basis = f1.basis) x
          JOIN fa_all f2 ON f2.fund_ref = x.key),
    -- fa: the funds the holdings reach, each with own_ref, the fund
    -- whose lines the look-through reads (a proxy's own, where one
    -- stands in).
    fa AS MATERIALIZED (
        SELECT *, CASE via WHEN 'proxy' THEN proxy_ref ELSE fund_ref END AS own_ref FROM fa_all
         WHERE fund_ref IN (SELECT fund_ref FROM r1 UNION SELECT fund_ref FROM r2 UNION SELECT fund_ref FROM r3)),
    -- fk: what the filing of each fund whose lines the look-through
    -- reads (own_ref) states of each line.
    fk AS MATERIALIZED (
        SELECT * FROM fund_filing_kinds(p_asof) WHERE fund_ref IN (SELECT own_ref FROM fa)),
    -- A line that states no coupon kind takes its filing's.
    kids AS MATERIALIZED (
        SELECT k.* EXCLUDE (own_ref) REPLACE (COALESCE(k.line_coupon_kind, fk.coupon_kind) AS line_coupon_kind)
          FROM (SELECT fa.fund_ref, 'line' AS kind, fa.leverage * l.weight::DOUBLE / 100 AS factor,
                       exposure_key(l.security_key) AS key, l.name, l.ticker, l.currency AS line_ccy,
                       l.asset_type AS asset_type_raw, lt.value AS class_map,
                       l.sector AS sector_raw, ls.value AS sector_map,
                       COALESCE(l.risk_country, l.country) AS country_raw, lc.value AS country_map,
                       l.long_currency, l.long_amount::DOUBLE AS long_amount, l.short_currency,
                       COALESCE(l.short_currency IS NOT NULL OR lt.value = 'foreign_exchange', FALSE) AS fx_line,
                       fa.as_of, fa.via, fa.gap_days, fa.forward, fa.aged, FALSE AS partial, fa.hedged_currency,
                       fa.currency AS fund_ccy, fa.net_assets, fa.leverage, fa.provider,
                       l.maturity AS line_maturity, l.coupon::DOUBLE AS line_coupon, l.coupon_kind AS line_coupon_kind,
                       l.real_yield IS NOT NULL AS line_real, l.lei AS line_lei, fa.own_ref
                  FROM fa
                  JOIN fund_lines l ON l.silver_source_id = fa.silver_source_id AND l.fund_ref = fa.stored_ref
                       AND l.as_of = fa.as_of AND l.basis = fa.basis
                  LEFT JOIN fund_line_labels lt ON lt.provider = fa.provider AND lt.field = 'asset_type'
                       AND lt.raw = l.asset_type
                  LEFT JOIN fund_line_labels ls ON ls.provider = fa.provider AND ls.field = 'sector' AND ls.raw = l.sector
                  LEFT JOIN fund_line_labels lc ON lc.provider = fa.provider AND lc.field = 'country'
                       AND lc.raw = COALESCE(l.risk_country, l.country)
                 WHERE l.weight IS NOT NULL) k
          LEFT JOIN fk ON fk.fund_ref = k.own_ref AND fk.key = k.key
        UNION ALL
        SELECT fa.fund_ref, 'remainder', fa.leverage * (100 - fa.wsum) / 100, NULL, NULL, NULL, NULL,
               NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, FALSE,
               fa.as_of, fa.via, fa.gap_days, fa.forward, fa.aged, fa.partial, fa.hedged_currency,
               fa.currency, fa.net_assets, fa.leverage, fa.provider, NULL, NULL, NULL, FALSE, NULL
          FROM fa WHERE fa.wsum <> 100
        UNION ALL
        SELECT fa.fund_ref, 'leverage', 1 - fa.leverage, NULL, NULL, NULL, NULL,
               NULL, 'cash', NULL, NULL, NULL, NULL, NULL, NULL, NULL, FALSE,
               fa.as_of, fa.via, fa.gap_days, fa.forward, fa.aged, FALSE, fa.hedged_currency,
               fa.currency, fa.net_assets, fa.leverage, fa.provider, NULL, NULL, NULL, FALSE, NULL
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
               CAST(NULL AS VARCHAR) AS provider, h.decl, h.expands,
               CAST(NULL AS DATE) AS line_maturity, CAST(NULL AS DOUBLE) AS line_coupon,
               CAST(NULL AS VARCHAR) AS line_coupon_kind, FALSE AS line_real, CAST(NULL AS VARCHAR) AS line_lei
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
               COALESCE(t.hedged, k.hedged_currency), k.fund_ccy, k.net_assets, k.leverage, k.provider,
               CAST(NULL AS INTEGER), CAST(NULL AS BOOLEAN),
               k.line_maturity, k.line_coupon, k.line_coupon_kind, k.line_real, k.line_lei
          FROM tree t JOIN kids k ON k.fund_ref = t.key
         WHERE t.depth < 3 AND t.kind IN ('holding', 'line') AND (t.depth > 0 OR t.expands)),
    leaves AS (
        SELECT t.*,
               COALESCE(t.depth = 3 AND t.kind = 'line' AND t.key IN (SELECT fund_ref FROM fa_all), FALSE) AS truncated
          FROM tree t
         WHERE CASE WHEN t.depth = 0 THEN NOT t.expands
                    ELSE NOT (t.depth < 3 AND t.kind = 'line' AND COALESCE(t.key IN (SELECT fund_ref FROM fa), FALSE))
               END),
    -- A declared holding splits into the declaration's parts. A part's
    -- class is the one it states; the part a declaration leaves
    -- unknown keeps the holding's.
    parts AS (
        SELECT l.* REPLACE (l.value * COALESCE(x.share, 1) AS value,
                            COALESCE(NULLIF(x.asset_class, 'unknown'), l.class) AS class),
               x.region AS d_region, x.country AS d_country, x.currency AS d_currency, x.sector AS d_sector
          FROM leaves l
          -- The depth is in the key, so the join hashes.
          LEFT JOIN exposure_declaration_leaves x ON x.line_no = CASE WHEN l.depth = 0 THEN l.decl END),
    -- A currency forward line that states its long leg splits in two: the
    -- long leg at notional in its currency, the rest (the short leg, its
    -- value less the long leg) in the short currency, else the line's
    -- own, so the pair sums to the line. Without a long leg it counts in
    -- its own currency, flagged partial.
    -- The rate is looked up once per long currency, fund currency and
    -- day the forward lines state, then joined back by those keys. A
    -- line without a long currency has no rate either way.
    fwd AS (
        SELECT f.long_currency, f.fund_ccy, f.day,
               COALESCE(CASE WHEN f.long_currency = f.fund_ccy THEN 1.0 END, r1.rate, u1.rate * u2.rate) AS long_rate
          FROM (SELECT DISTINCT long_currency, fund_ccy, comp_as_of // 86400 AS day
                  FROM parts WHERE long_currency IS NOT NULL) f
          ASOF LEFT JOIN fx_daily r1 ON r1.from_ccy = f.long_currency AND r1.to_ccy = f.fund_ccy
               AND r1.day <= f.day
          ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = f.long_currency AND u1.to_ccy = 'USD'
               AND u1.day <= f.day
          ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = f.fund_ccy
               AND u2.day <= f.day),
    legs AS (
        SELECT l.*, fwd.long_rate
          FROM parts l
          LEFT JOIN fwd ON fwd.long_currency = l.long_currency
               AND fwd.fund_ccy IS NOT DISTINCT FROM l.fund_ccy
               AND fwd.day IS NOT DISTINCT FROM l.comp_as_of // 86400),
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
    known AS (SELECT fund_ref FROM exposure_fund_keys()),
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
               v.asset_type_raw, v.sector_raw, v.country_raw, v.sector_map, v.country_map, v.fund_leaf,
               v.decl IS NOT NULL AS declared,
               v.line_maturity, v.line_coupon, v.line_coupon_kind, v.line_real, v.line_lei,
               CASE WHEN NOT (v.fx_line AND NOT v.fx_unsplit) THEN v.value
                    WHEN v.long_leg THEN v.long_value ELSE v.value - v.long_value END AS value,
               CASE WHEN v.long_leg THEN 'foreign_exchange' ELSE v.class END AS asset_class,
               COALESCE(sec.name, v.name, v.h_name) AS name, COALESCE(sec.name, v.name) AS own_name, v.ticker,
               sec.market_cap,
               CASE WHEN v.class = 'public_equity' AND NOT v.long_leg AND NOT v.fund_leaf THEN sec.size END AS size,
               CASE WHEN v.kind = 'remainder' THEN NULL
                    WHEN v.kind = 'leverage' OR v.class = 'cash' THEN 'cash'
                    WHEN v.class = 'metal' THEN 'metal'
                    WHEN v.class = 'crypto' THEN 'crypto'
                    WHEN v.depth = 0 AND v.d_sector IS NOT NULL THEN NULLIF(v.d_sector, 'unknown')
                    WHEN v.fund_leaf THEN CASE WHEN v.class = 'real_estate' THEN 'real_estate'
                                               WHEN v.depth = 0 AND v.class = 'private_equity'
                                               THEN 'diversified' END
                    ELSE COALESCE(sec.sector, v.sector_map, CASE WHEN v.class = 'real_estate' THEN 'real_estate' END)
               END AS sector,
               CASE WHEN v.kind IN ('remainder', 'leverage') OR v.class IN ('metal', 'crypto') THEN NULL
                    -- A declared region states no country.
                    WHEN v.depth = 0 AND (v.d_country IS NOT NULL OR v.d_region IS NOT NULL)
                         THEN NULLIF(v.d_country, 'unknown')
                    WHEN v.depth = 0 AND v.is_cash THEN cu.country
                    WHEN v.fund_leaf THEN NULL
                    WHEN p_country = 'domicile' THEN COALESCE(sec.domicile, sec.risk_country, v.country_map)
                    ELSE COALESCE(sec.risk_country, v.country_map, sec.domicile)
               END AS country,
               CASE WHEN v.class NOT IN ('metal', 'crypto') THEN NULLIF(v.d_region, 'unknown') END AS declared_region,
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
                              WHEN v.depth = 0 THEN NULLIF(v.d_currency, 'unknown')
                         END
                    WHEN v.depth = 0 AND v.d_currency IS NOT NULL THEN NULLIF(v.d_currency, 'unknown')
                    WHEN v.depth = 0 THEN v.line_ccy
                    WHEN v.kind IN ('remainder', 'leverage') THEN COALESCE(v.fund_ccy, v.h_ccy)
                    ELSE COALESCE(v.line_ccy, sec.currency, v.fund_ccy, v.h_ccy)
               END AS currency,
               CASE WHEN v.fx_line OR (v.depth > 0 AND v.hedged IS NOT NULL) THEN 'fiat'
                    WHEN v.class = 'metal' THEN 'metal' WHEN v.class = 'crypto' THEN 'crypto'
                    ELSE 'fiat' END AS currency_kind,
               -- A receipt's sector, country or domicile taken from its
               -- share, matched by name (exposure_receipts.go).
               COALESCE(starts_with(sec.sector_origin, 'underlying:')
                        OR starts_with(sec.risk_country_origin, 'underlying:')
                        OR starts_with(sec.domicile_origin, 'underlying:'), FALSE) AS by_name,
               exposure_public(v.h_vehicle, v.h_class) AS public,
               -- What a source states, not what an ISIN's prefix implies;
               -- nothing for a fund no composition describes.
               NOT v.fund_leaf
                   AND (sec.sector IS NOT NULL OR sec.risk_country IS NOT NULL
                        OR (sec.domicile IS NOT NULL AND sec.domicile_origin <> 'isin_prefix')) AS described
          FROM valued v
          LEFT JOIN securities sec ON sec.security_key = v.key
          LEFT JOIN exposure_custody cu ON cu.silver_source_id = v.src)
    SELECT c.*,
           co.name AS country_name, COALESCE(co.region, c.declared_region) AS region,
           CASE WHEN c.depth = 0 AND c.is_cash THEN 'cash'
                WHEN c.depth = 0 AND c.asset_class IN ('metal', 'crypto') THEN 'asset_class'
                WHEN c.depth = 0 AND c.declared THEN 'declared'
                WHEN c.depth = 0 AND c.public AND c.described THEN 'security'
                WHEN c.depth = 0 THEN 'unknown'
                WHEN c.kind = 'remainder' THEN 'remainder'
                WHEN c.truncated THEN 'truncated'
                WHEN c.via = 'proxy' THEN 'proxied'
                ELSE 'looked_through' END AS how,
           list_filter([
               CASE WHEN c.via = 'proxy' THEN 'proxy' END,
               CASE WHEN c.declared THEN 'declared' END,
               CASE WHEN c.aged THEN 'aged' END,
               CASE WHEN c.forward THEN 'forward' END,
               CASE WHEN (c.kind = 'remainder' AND c.partial) OR c.fx_unsplit OR c.truncated THEN 'partial' END,
               CASE WHEN c.depth > 0 AND (c.class_unmapped
                         OR (c.sector_raw IS NOT NULL AND c.sector_map IS NULL AND c.sector IS NULL)
                         OR (c.country_raw IS NOT NULL AND c.country_map IS NULL AND c.country IS NULL))
                    THEN 'unmapped' END,
               CASE WHEN c.by_name AND NOT c.declared AND NOT c.fund_leaf THEN 'by_name' END,
               CASE WHEN c.depth = 0 AND c.fund_leaf AND c.asset_class = 'private_equity' THEN 'opaque' END,
               CASE WHEN c.depth = 0 AND NOT c.is_cash AND c.asset_class NOT IN ('metal', 'crypto', 'cash')
                         AND NOT c.public AND NOT c.declared THEN 'undeclared' END,
               CASE WHEN c.depth = 0 AND c.public AND NOT c.declared
                         AND (c.sector IS NULL OR c.country IS NULL) THEN 'unmatched' END
           ], lambda x: x IS NOT NULL) AS flags,
           st.situs, COALESCE(st.basis, 'unknown') AS situs_basis
      FROM cls c
      LEFT JOIN exposure_countries co ON co.code = c.country
      LEFT JOIN exposure_situs_at(p_asof) st
             ON st.silver_source_id = c.src AND st.account_external_id = c.acct AND st.position_key = c.position_key
     WHERE c.value IS NOT NULL
);

-- exposure_credit_leaves and exposure_maturity_leaves: 0137's, the page
-- fund's breakdown joined by a key.
CREATE OR REPLACE MACRO exposure_credit_leaves(p_asof, p_ccy) AS TABLE (
    WITH l AS MATERIALIZED (SELECT * FROM exposure_yield_leaves(p_asof, p_ccy))
    SELECT l.src, l.acct, l.position_key, l.depth, l.flags, l.value * COALESCE(b.share, 1) AS value,
           exposure_credit_bucket(l.asset_class, l.debt, l.rating, b.bucket) AS bucket,
           exposure_credit_unknown(l.debt, l.rating, b.unknown) AS unknown
      FROM l
      -- The condition is in the key, so the join hashes.
      LEFT JOIN exposure_page_breakdown(p_asof, 'credit_quality') b
             ON b.fund = CASE WHEN l.debt AND l.rating IS NULL THEN l.page_fund END
);

CREATE OR REPLACE MACRO exposure_maturity_leaves(p_asof, p_ccy) AS TABLE (
    WITH l AS MATERIALIZED (SELECT * FROM exposure_yield_leaves(p_asof, p_ccy))
    SELECT l.src, l.acct, l.position_key, l.depth, l.flags, l.value * COALESCE(b.share, 1) AS value,
           exposure_maturity_split(l.maturity_bucket, b.bucket) AS bucket
      FROM l
      -- The condition is in the key, so the join hashes.
      LEFT JOIN exposure_page_breakdown(p_asof, 'maturity_breakdown') b
             ON b.fund = CASE WHEN l.maturity_bucket = 'unknown' THEN l.page_fund END
);

-- report_exposure_funds: 0131's, a holding's composition and a declared
-- fund's attributes joined by a key.
CREATE OR REPLACE MACRO report_exposure_funds(p_asof, p_ccy) AS TABLE (
    WITH h AS (
        SELECT h.*, d.line_no AS decl, d.as_of AS decl_as_of, d.dimensions AS decl_dimensions,
               {'situs': st.situs, 'basis': COALESCE(st.basis, 'unknown')} AS holding_situs
          FROM exposure_holdings(p_asof, p_ccy) h
          LEFT JOIN exposure_declarations_at(p_asof) d
                 ON d.silver_source_id = h.src AND d.instrument_external_id = h.iid
          LEFT JOIN exposure_situs_at(p_asof) st
                 ON st.silver_source_id = h.src AND st.account_external_id = h.acct AND st.position_key = h.position_key
         WHERE h.value IS NOT NULL),
    t AS (SELECT SUM(value) AS total FROM h),
    fa AS (SELECT * FROM exposure_funds_at(p_asof)),
    -- comp_ref: the composition that describes the holding, its fund's own
    -- where a declaration describes it too (exposure_leaves). A declared
    -- holding that is no fund counts among the other holdings, which no
    -- declaration describes. The join is on the key alone, so it hashes;
    -- fa holds one row per fund, so testing after the join gives the
    -- same rows.
    k AS (
        SELECT h.*, CASE WHEN h.decl IS NULL OR fa.own_as_of IS NOT NULL THEN fa.fund_ref END AS comp_ref,
               comp_ref IS NULL AND h.decl IS NOT NULL AND h.vehicle IN ('etf', 'fund') AS declared
          FROM h LEFT JOIN fa ON fa.fund_ref = h.key),
    f AS (
        SELECT CASE WHEN k.comp_ref IS NOT NULL OR k.vehicle IN ('etf', 'fund') THEN COALESCE(k.key, 'holding:' || k.src || ':' || COALESCE(k.iid, k.position_key))
                    ELSE '(other holdings)' END AS fund,
               k.* FROM k),
    g AS (
        SELECT f.fund, MAX(f.name) FILTER (WHERE f.fund <> '(other holdings)') AS name,
               SUM(f.value) AS value, 100 * SUM(f.value) / NULLIF(t.total, 0) AS share_pct,
               COALESCE(MAX(fa.via), CASE WHEN BOOL_OR(f.declared) THEN 'declared' END) AS via, MAX(fa.proxy_ref) FILTER (WHERE fa.via = 'proxy') AS proxy,
               MAX(fa.leverage) FILTER (WHERE fa.via = 'proxy') AS leverage,
               MAX(fa.provider) AS provider, MAX(fa.basis) AS basis, MAX(fa.as_of) AS composition_as_of,
               MAX(fa.gap_days) AS gap_days, MAX(fa.lines) AS lines, MAX(fa.wsum) AS weight_sum,
               COALESCE(MAX(fa.domicile), MAX(fat.domicile)) AS domicile,
               arg_max(f.holding_situs, abs(f.value)) FILTER (WHERE f.fund <> '(other holdings)') AS top_situs,
               COALESCE(MAX(fa.replication), MAX(fat.replication)) AS replication,
               COALESCE(BOOL_OR(fa.lending), BOOL_OR(fat.lending)) AS lending,
               COALESCE(MAX(fa.hedged_currency), MAX(fat.hedged_currency)) AS hedged_currency,
               MAX(f.decl_as_of) FILTER (WHERE f.declared) AS declaration_as_of,
               MAX(f.decl_dimensions) FILTER (WHERE f.declared) AS declared_dimensions,
               array_to_string(list_sort(list_distinct(list_filter([
                   CASE WHEN BOOL_OR(fa.via = 'proxy') THEN 'proxy' END,
                   CASE WHEN BOOL_OR(fa.aged) THEN 'aged' END,
                   CASE WHEN BOOL_OR(fa.forward) THEN 'forward' END,
                   CASE WHEN BOOL_OR(fa.partial) THEN 'partial' END,
                   CASE WHEN BOOL_OR(f.declared) THEN 'declared' END,
                   CASE WHEN f.fund <> '(other holdings)' AND MAX(fa.fund_ref) IS NULL AND NOT BOOL_OR(f.declared)
                        THEN 'no_composition' END],
                   lambda x: x IS NOT NULL))), ',') AS quality
          FROM f
          LEFT JOIN fa ON fa.fund_ref = f.comp_ref
          -- A declared fund's attributes are its own, as they are under a
          -- composition (exposure_funds_at). The condition is in the key,
          -- so the join hashes.
          LEFT JOIN exposure_attributes_at(p_asof) fat ON fat.fund_ref = CASE WHEN f.declared THEN f.key END, t
         GROUP BY f.fund, t.total)
    SELECT * EXCLUDE (top_situs), top_situs.situs AS situs, top_situs.basis AS situs_basis
      FROM g
     ORDER BY fund = '(other holdings)', abs(value) DESC, fund
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (156, CAST(epoch(now()) AS BIGINT));
