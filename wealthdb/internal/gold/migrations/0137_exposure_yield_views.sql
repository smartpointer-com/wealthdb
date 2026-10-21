-- ============================================================
-- gold schema, migration 0137 — five composition views of exposure:
-- credit, maturities, coupons, cycles and issuers (docs/EXPOSURE.md §7).
-- Each looks the holdings through as every exposure view does and sums
-- to net worth; a leaf the dimension does not apply to reads (none), one
-- no source describes unknown.
--
-- fund_lines gains the LEI a line's payload states, read once here and
-- at every load (gold.Writer), so the look-through reads a column.
-- exposure_leaves gains the debt and issuer columns of a fund's line.
-- exposure_yield_leaves gives each leaf its maturity, coupon kind,
-- rating, cycle and issuer: a holding's from its terms
-- (instrument_terms_at), a line's from the line.
-- exposure_credit_leaves and exposure_maturity_leaves split a leaf no
-- rating or maturity describes by its fund's page. The views group them,
-- as the dashboard's table does (gold.MaterializeExposure).
--
-- IF NOT EXISTS and OR REPLACE keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS lei TEXT;
UPDATE fund_lines SET lei = json_extract_string(payload, '$.lei')
 WHERE lei IS NULL AND json_type(payload, '$.lei') = 'VARCHAR' AND json_extract_string(payload, '$.lei') <> '';

-- exposure_leaves: the look-through at an instant (docs/EXPOSURE.md §2).
-- One row per leaf: a holding no composition describes, a part of a
-- declared holding, a fund's line, a remainder (what a fund's weights
-- leave unexplained) or the cash leg of a leveraged proxy. Each carries
-- its value in p_ccy, its class, sector, country, region and currency, a
-- public equity security's size, the quality flags it rests on, the
-- reason it is classified (`how`), and its holding's situs and the basis
-- of it (exposure_situs). A fund's line carries what it states of its
-- debt and its issuer, for the credit, maturity, coupon and issuer views:
-- line_maturity, line_coupon, line_coupon_kind (as the provider writes
-- it), line_real (it states a real yield) and line_lei; own_name is a
-- leaf's own name, never its fund's. The leaves of an instant sum to
-- `holdings global`. p_country: 'risk' or 'domicile'. Only the funds a
-- holding reaches within three levels are expanded; a fund the third
-- level still names stays a leaf, flagged partial. A holding a
-- declaration describes is looked through only where its fund has a
-- composition of its own (which an always-proxy replaces), never by a
-- proxy that fills a gap. Otherwise it stays a leaf, split into the
-- declaration's parts, each a share of its value
-- (exposure_declaration_leaves). A part takes the class, region,
-- country, currency and sector the declaration states for it. For a
-- dimension the declaration leaves out, it reads what the holding reads
-- without one.
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
    fa AS MATERIALIZED (
        SELECT * FROM fa_all
         WHERE fund_ref IN (SELECT fund_ref FROM r1 UNION SELECT fund_ref FROM r2 UNION SELECT fund_ref FROM r3)),
    kids AS MATERIALIZED (
        SELECT fa.fund_ref, 'line' AS kind, fa.leverage * l.weight::DOUBLE / 100 AS factor,
               exposure_key(l.security_key) AS key, l.name, l.ticker, l.currency AS line_ccy,
               l.asset_type AS asset_type_raw, lt.value AS class_map,
               l.sector AS sector_raw, ls.value AS sector_map,
               COALESCE(l.risk_country, l.country) AS country_raw, lc.value AS country_map,
               l.long_currency, l.long_amount::DOUBLE AS long_amount, l.short_currency,
               COALESCE(l.short_currency IS NOT NULL OR lt.value = 'foreign_exchange', FALSE) AS fx_line,
               fa.as_of, fa.via, fa.gap_days, fa.forward, fa.aged, FALSE AS partial, fa.hedged_currency,
               fa.currency AS fund_ccy, fa.net_assets, fa.leverage, fa.provider,
               l.maturity AS line_maturity, l.coupon::DOUBLE AS line_coupon, l.coupon_kind AS line_coupon_kind,
               l.real_yield IS NOT NULL AS line_real, l.lei AS line_lei
          FROM fa
          JOIN fund_lines l ON l.silver_source_id = fa.silver_source_id AND l.fund_ref = fa.stored_ref
               AND l.as_of = fa.as_of AND l.basis = fa.basis
          LEFT JOIN fund_line_labels lt ON lt.provider = fa.provider AND lt.field = 'asset_type' AND lt.raw = l.asset_type
          LEFT JOIN fund_line_labels ls ON ls.provider = fa.provider AND ls.field = 'sector' AND ls.raw = l.sector
          LEFT JOIN fund_line_labels lc ON lc.provider = fa.provider AND lc.field = 'country'
               AND lc.raw = COALESCE(l.risk_country, l.country)
         WHERE l.weight IS NOT NULL
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
          LEFT JOIN exposure_declaration_leaves x ON l.depth = 0 AND x.line_no = l.decl),
    -- A currency forward line that states its long leg splits in two: the
    -- long leg at notional in its currency, the rest (the short leg, its
    -- value less the long leg) in the short currency, else the line's
    -- own, so the pair sums to the line. Without a long leg it counts in
    -- its own currency, flagged partial.
    legs AS (
        SELECT l.*, COALESCE(CASE WHEN l.long_currency = l.fund_ccy THEN 1.0 END, r1.rate, u1.rate * u2.rate) AS long_rate
          FROM parts l
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

-- exposure_label_words: a camel-cased key as words: its first letter in
-- upper case, then a space at each step from lower to upper case and
-- after a figure (aA as AA, notRated as Not Rated, 5to10Years as 5 to10
-- Years). A label in words stays as it is.
CREATE OR REPLACE MACRO exposure_label_words(p_label) AS
    regexp_replace(regexp_replace(upper(left(p_label, 1)) || substr(p_label, 2), '([a-z])([A-Z])', '\1 \2', 'g'),
                   '([0-9])([A-Za-z])', '\1 \2', 'g');

-- exposure_rating_label: the bucket of a label a fund's credit-quality
-- breakdown or a description states (AAA, AA+, Baa1, (P)A2, AA Rated,
-- Below B, B and below, Not Rated, Cash and/or Derivatives, or the same
-- as one camel-cased key: aA, notRated, cCCandbelow): AAA to B (B and
-- below; a grade of default among them), unrated, cash, else NULL.
-- exposure_rating_grade reads a label once it is upper case and bare.
CREATE OR REPLACE MACRO exposure_rating_grade(g) AS
    CASE WHEN g LIKE 'CASH%' OR g LIKE '%DERIVATIVE%' THEN 'cash'
         WHEN g IN ('NR', 'N/R', 'NOT RATED', 'NOTRATED', 'UNRATED', 'NON RATED', 'NON-RATED', 'NONRATED',
                    'NO RATING', 'NORATING', 'N/A', 'NA', 'WR', 'WD') THEN 'unrated'
         WHEN regexp_matches(g, '^AAA[+-]?( RATED)?$') THEN 'AAA'
         WHEN regexp_matches(g, '^(AA[+-]?|AA[1-3])( RATED)?$') THEN 'AA'
         WHEN regexp_matches(g, '^(A[+-]?|A[1-3])( RATED)?$') THEN 'A'
         WHEN regexp_matches(g, '^(BBB[+-]?|BAA[1-3]?)( RATED)?$') THEN 'BBB'
         WHEN regexp_matches(g, '^(BB[+-]?|BA[1-3]?)( RATED)?$') THEN 'BB'
         WHEN regexp_matches(g,
                  '^(B[+-]?|B[1-3]|CCC.*|CC|C|D|DD|DDD|RD|SD|CAA.*|CA|BELOW ?[BC].*|BB? ?AND ?BELOW)( RATED)?$')
              THEN 'B' END;

CREATE OR REPLACE MACRO exposure_rating_label(p_label) AS
    exposure_rating_grade(regexp_replace(trim(regexp_replace(regexp_replace(upper(
        exposure_label_words(trim(COALESCE(p_label, '')))),
        '^\(P\)\s*', ''), '[*()]', '', 'g')), '\s+', ' ', 'g'));

-- exposure_maturity_band: the bucket of a time to maturity in years.
CREATE OR REPLACE MACRO exposure_maturity_band(p_years) AS
    CASE WHEN p_years < 1 THEN 'under 1y' WHEN p_years < 3 THEN '1-3y' WHEN p_years < 5 THEN '3-5y'
         WHEN p_years < 10 THEN '5-10y' WHEN p_years < 20 THEN '10-20y' ELSE 'over 20y' END;

-- exposure_maturity_years: the first figure of a band a fund's maturity
-- breakdown states, in years (a band in months over twelve).
-- exposure_maturity_upper: its second, after a dash or TO; NULL for one
-- figure.
CREATE OR REPLACE MACRO exposure_maturity_years(p_label) AS
    TRY_CAST(regexp_extract(p_label, '([0-9]+(\.[0-9]+)?)', 1) AS DOUBLE)
        / CASE WHEN upper(p_label) LIKE '%MONTH%' THEN 12 ELSE 1 END;

CREATE OR REPLACE MACRO exposure_maturity_upper(p_label) AS
    TRY_CAST(regexp_extract(upper(p_label), '[0-9.]+\s*(-|–|TO)\s*([0-9]+(\.[0-9]+)?)', 2) AS DOUBLE)
        / CASE WHEN upper(p_label) LIKE '%MONTH%' THEN 12 ELSE 1 END;

-- exposure_maturity_label: the bucket of a band a fund's maturity
-- breakdown states (0 - 1 Years, 6-12 Months, 20+ Years, > 30 Years,
-- Under 1 Year, 10 Years and over, or one camel-cased key: 5to10Years).
-- A band that lies in one bucket reads that bucket. One that spans
-- buckets reads NULL: a closed band across a bucket's edge, an open band
-- from under 20 years up, one up to more than a year. Cash; else NULL.
-- exposure_maturity_words reads a band once it is upper case and spaced.
CREATE OR REPLACE MACRO exposure_maturity_words(u) AS
    CASE WHEN u LIKE 'CASH%' OR u LIKE '%DERIVATIVE%' THEN 'cash'
         WHEN regexp_matches(u, '^(>|OVER|MORE ?THAN|GREATER ?THAN|ABOVE)\s*[0-9]')
              OR regexp_matches(u, '^[0-9.]+\s*(\+|PLUS\b)')
              OR regexp_matches(u, '^[0-9].*(\+| AND (OVER|ABOVE|LONGER)| OR (MORE|LONGER))$') THEN
              CASE WHEN exposure_maturity_years(u) >= 20 THEN 'over 20y' END
         WHEN regexp_matches(u, '^(<|UNDER|LESS ?THAN|UP ?TO|BELOW)\s*[0-9]') THEN
              CASE WHEN exposure_maturity_years(u) <= 1 THEN 'under 1y' END
         WHEN NOT regexp_matches(u, '^[0-9]') THEN NULL
         WHEN exposure_maturity_upper(u) IS NOT NULL AND exposure_maturity_band(exposure_maturity_years(u))
              <> exposure_maturity_band(exposure_maturity_upper(u) - 1e-6) THEN NULL
         ELSE exposure_maturity_band(exposure_maturity_years(u)) END;

CREATE OR REPLACE MACRO exposure_maturity_label(p_label) AS
    exposure_maturity_words(upper(trim(exposure_label_words(trim(COALESCE(p_label, ''))))));

-- exposure_maturity_bucket: the bucket of a maturity on a day: under a
-- year to over 20 years, perpetual from a century on. A maturity exactly
-- N years on is in the longer bucket.
CREATE OR REPLACE MACRO exposure_maturity_bucket(p_day, p_maturity) AS
    CASE WHEN p_maturity < p_day + INTERVAL 1 YEAR THEN 'under 1y'
         WHEN p_maturity < p_day + INTERVAL 3 YEAR THEN '1-3y'
         WHEN p_maturity < p_day + INTERVAL 5 YEAR THEN '3-5y'
         WHEN p_maturity < p_day + INTERVAL 10 YEAR THEN '5-10y'
         WHEN p_maturity < p_day + INTERVAL 20 YEAR THEN '10-20y'
         WHEN p_maturity < p_day + INTERVAL 100 YEAR THEN 'over 20y'
         ELSE 'perpetual' END;

-- exposure_issuer_text: a name upper case, ß, Ø and the accents read
-- as plain letters, its words up to the first figure after the first
-- word (a coupon, a date), N/B and A/S as one word, U.S., UNITED STATES
-- and TREAS as US and TREASURY, punctuation as spaces, a leading THE
-- dropped.
CREATE OR REPLACE MACRO exposure_issuer_text(p_name) AS
    regexp_replace(regexp_replace(regexp_replace(regexp_replace(regexp_replace(regexp_replace(regexp_replace(
        upper(strip_accents(replace(replace(replace(replace(COALESCE(p_name, ''), 'ß', 'ss'), 'ẞ', 'SS'), 'Ø', 'O'),
                                    'ø', 'o'))),
        '^(\S+.*?)\s+\S*[0-9].*$', '\1'),
        '\b([A-Z])/([A-Z])\b', '\1\2', 'g'),
        '\bU\.\s?S\.?(\s|$)', 'US\1', 'g'),
        '^UNITED STATES\b', 'US'),
        '\bTREAS\b', 'TREASURY', 'g'),
        '[^A-Z0-9& ]', ' ', 'g'),
        '^\s*THE(\s+|$)', '');

-- exposure_issuer_name: a name as the issuers view groups it: its text
-- (exposure_issuer_text), its first word kept, from the others a class
-- letter (CLASS A, CL B) and the words of a legal form, a share class or
-- a bond's kind (INC, CORP, PLC, AG, ADR, NOTE, MTN, SR UNSECURED)
-- dropped.
CREATE OR REPLACE MACRO exposure_issuer_name(p_name) AS
    nullif(trim(regexp_replace(
        regexp_extract(exposure_issuer_text(p_name), '^\s*(\S*)', 1) || ' ' ||
        regexp_replace(regexp_replace(exposure_issuer_text(p_name), '^\s*\S*', ''),
            '\b(CLASS|CL|SERIES|SER)\s+[A-Z]\b'
            || '|\b(THE|INC|INCORPORATED|CORP|CORPORATION|CO|COMPANY|LTD|LIMITED|PLC|AG|SA|NV|SE|AB|AS|ASA|SPA'
            || '|LLC|LP|HOLDINGS|HOLDING|GROUP|CLASS|CL|COM|COMMON|STOCK|ORD|SHS|SHARES|ADR|ADS|GDR|SPONSORED'
            || '|UNSPONSORED|REG|REGISTERED|NEW|NB|NOTE|NOTES|BOND|BONDS|BILL|BILLS|MTN|EMTN|GMTN|SR|SNR|SENIOR'
            || '|SUB|SUBORDINATED|UNSECURED|SECURED|REGS|144A|FRN)\b',
            ' ', 'g'),
        '\s+', ' ', 'g')), '');

-- exposure_credit_bucket: a leaf's credit bucket: cash; (none) for no
-- debt; its rating; else the bucket of its page fund's breakdown; else
-- unrated. exposure_credit_unknown: a debt leaf no rating or known label
-- describes. exposure_maturity_split: a leaf's maturity bucket, its page
-- fund's band where it has none.
CREATE OR REPLACE MACRO exposure_credit_bucket(p_class, p_debt, p_rating, p_page) AS
    CASE WHEN p_class = 'cash' THEN 'cash' WHEN NOT p_debt THEN '(none)'
         ELSE COALESCE(p_rating, p_page, 'unrated') END;

CREATE OR REPLACE MACRO exposure_credit_unknown(p_debt, p_rating, p_page_unknown) AS
    p_debt AND p_rating IS NULL AND COALESCE(p_page_unknown, TRUE);

CREATE OR REPLACE MACRO exposure_maturity_split(p_bucket, p_page) AS
    CASE WHEN p_bucket = 'unknown' AND p_page IS NOT NULL THEN p_page ELSE p_bucket END;

-- exposure_yield_leaves: exposure_leaves with each leaf's debt and issuer
-- dimensions at the instant:
--
--   * debt: a leaf of fixed income or private debt; deposit: a holding of
--     cash at its custodian (a balance, a demand or a time deposit);
--   * maturity: a holding's from its terms, else a term's end its
--     account's rates state; a line's as its fund states it.
--     maturity_bucket reads it on the day (exposure_maturity_bucket); cash
--     without a term counts under a year, a debt leaf without one that
--     says PERP is perpetual, any other unknown;
--   * coupon_kind: fixed, floating, inflation_linked or zero; a holding's
--     from its terms; a line's inflation-linked where it states a real
--     yield or its name says so (TIPS, INFLATION), else as its fund types
--     it (N-PORT's Fixed, Floating, Variable, None), else read from its
--     name (FRN) and its coupon (zero is zero, any other fixed);
--   * page_fund: the fund whose page may describe a leaf: a fund leaf's
--     own, a line's fund;
--   * rating: a holding's bucket from its terms, else the worse end of
--     the range yield_rating_funds gives its page fund;
--   * cycle: a public equity leaf's, by its sector (yield_cycles);
--   * issuer_key and issuer_name: a deposit's custodian, else its source;
--     a security's issuer by the LEI its line or GLEIF states, else by
--     the name exposure_issuer_name gives, which a GLEIF legal name of the
--     same text ties to its LEI; else by its key, else its LEI, else
--     `unknown`; NULL where no issuer applies (a fund nothing looks
--     through, the parts a declaration splits a private holding into,
--     real estate, a metal, a fund's cash). name_rank orders the names an
--     issuer may print by: GLEIF's, a stock's, any other (its normalized
--     name).
CREATE OR REPLACE MACRO exposure_yield_leaves(p_asof, p_ccy) AS TABLE (
    WITH l AS MATERIALIZED (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT * FROM instrument_terms_at(p_asof)),
    rf AS (SELECT exposure_key(fund_ref) AS fund_ref,
                  arg_max(bucket_low, list_position(['AAA', 'AA', 'A', 'BBB', 'BB', 'B'], bucket_low)) AS bucket_low
             FROM yield_rating_funds GROUP BY 1),
    gk AS (SELECT exposure_key(security_key) AS key, first(lei ORDER BY as_of DESC, lei) AS lei
             FROM security_identities WHERE provider = 'gleif' AND lei IS NOT NULL GROUP BY 1),
    gl AS (SELECT lei, first(name ORDER BY as_of DESC, name) AS name FROM security_identities
            WHERE provider = 'gleif' AND lei IS NOT NULL AND name IS NOT NULL GROUP BY 1),
    gn AS (SELECT exposure_issuer_name(name) AS norm, MIN(lei) AS lei FROM gl
            GROUP BY 1 HAVING norm IS NOT NULL AND count(DISTINCT lei) = 1),
    te AS (SELECT silver_source_id, account_external_id, COALESCE(currency, '') AS ccy, term_end
             FROM account_rates
            WHERE side = 'credit' AND term_end > CAST(to_timestamp(p_asof) AT TIME ZONE 'UTC' AS DATE)),
    -- What each distinct name says, read once: its issuer text and
    -- whether it names an inflation-linked, a floating or a perpetual
    -- bond.
    nm AS MATERIALIZED (
        SELECT name, exposure_issuer_name(name) AS norm,
               regexp_matches(upper(name), '\b(TIPS|INFLATION|INFL|LINKERS?|I/L|INDEX[- ]LINKED|CPI)\b') AS infl,
               regexp_matches(upper(name), '\b(FRN|FLOAT|FLOATING|FLTG)\b') AS frn,
               regexp_matches(upper(name), '\bPERP') AS perp
          FROM (SELECT own_name AS name FROM l WHERE own_name IS NOT NULL
                UNION SELECT custodian FROM exposure_custody WHERE custodian IS NOT NULL)),
    d AS (
        SELECT l.*,
               l.asset_class IN ('fixed_income', 'private_debt') AS debt,
               l.depth = 0 AND (l.is_cash OR (l.asset_class = 'cash'
                                              AND l.h_vehicle IN ('demand_deposit', 'time_deposit'))) AS deposit,
               CASE WHEN l.fund_leaf THEN l.key WHEN l.depth > 0 THEN l.fund_ref END AS page_fund,
               CASE WHEN l.depth > 0 THEN l.line_maturity
                    WHEN l.is_cash OR (l.asset_class = 'cash' AND l.h_vehicle IN ('demand_deposit', 'time_deposit'))
                         THEN COALESCE(t.maturity, (SELECT MIN(te.term_end) FROM te
                                                     WHERE te.silver_source_id = l.src
                                                       AND te.account_external_id = l.acct
                                                       AND te.ccy IN (l.currency, '')))
                    ELSE t.maturity END AS maturity,
               CASE WHEN l.depth = 0 THEN t.coupon_kind
                    WHEN l.line_real OR o.infl THEN 'inflation_linked'
                    WHEN l.line_coupon_kind = 'Fixed' THEN 'fixed'
                    WHEN l.line_coupon_kind IN ('Floating', 'Variable') THEN 'floating'
                    WHEN l.line_coupon_kind = 'None' THEN 'zero'
                    WHEN o.frn THEN 'floating'
                    WHEN l.line_coupon = 0 THEN 'zero'
                    WHEN l.line_coupon > 0 THEN 'fixed' END AS coupon_kind,
               CASE WHEN l.depth = 0 THEN t.rating END AS own_rating,
               cu.custodian, COALESCE(o.perp, FALSE) AS named_perpetual, o.norm AS own_norm, c.norm AS custodian_norm,
               COALESCE(l.line_lei, gk.lei) AS stated_lei
          FROM l
          -- Every use of t below is a holding's own (depth 0).
          LEFT JOIN t ON t.silver_source_id = l.src AND t.instrument_external_id = l.iid
          LEFT JOIN gk ON gk.key = l.key
          LEFT JOIN exposure_custody cu ON cu.silver_source_id = l.src
          LEFT JOIN nm o ON o.name = l.own_name
          LEFT JOIN nm c ON c.name = cu.custodian),
    n AS (
        SELECT d.* EXCLUDE (own_norm, custodian_norm),
               CASE WHEN d.deposit THEN d.custodian_norm ELSE d.own_norm END AS norm,
               d.deposit OR (d.kind IN ('holding', 'line') AND NOT d.fund_leaf AND NOT (d.depth = 0 AND d.declared)
                             AND d.asset_class IN ('public_equity', 'fixed_income', 'private_debt')) AS has_issuer
          FROM d)
    SELECT n.*,
           CASE WHEN n.asset_class = 'cash' AND n.maturity IS NULL THEN 'under 1y'
                WHEN NOT n.debt AND n.asset_class <> 'cash' THEN '(none)'
                WHEN n.maturity IS NULL AND n.named_perpetual THEN 'perpetual'
                WHEN n.maturity IS NULL THEN 'unknown'
                ELSE exposure_maturity_bucket(CAST(to_timestamp(p_asof) AT TIME ZONE 'UTC' AS DATE), n.maturity)
           END AS maturity_bucket,
           CASE WHEN NOT n.debt THEN '(none)' ELSE COALESCE(n.coupon_kind, 'unknown') END AS coupon_bucket,
           COALESCE(n.own_rating, rf.bucket_low) AS rating,
           CASE WHEN n.asset_class <> 'public_equity' THEN '(none)' ELSE COALESCE(cy.cycle, 'unknown') END
               AS cycle,
           CASE WHEN NOT n.has_issuer THEN NULL
                WHEN COALESCE(CASE WHEN NOT n.deposit THEN n.stated_lei END, gn.lei) IS NOT NULL
                     THEN 'lei:' || COALESCE(CASE WHEN NOT n.deposit THEN n.stated_lei END, gn.lei)
                WHEN n.norm IS NOT NULL THEN 'name:' || n.norm
                WHEN n.deposit THEN 'source:' || n.src
                WHEN n.key IS NOT NULL THEN 'key:' || n.key
                ELSE 'unknown' END AS issuer_key,
           CASE WHEN NOT n.has_issuer THEN NULL
                ELSE COALESCE(gl.name, CASE WHEN n.deposit THEN COALESCE(n.custodian, n.src) END,
                              CASE WHEN n.asset_class = 'public_equity' THEN n.own_name END, n.norm, n.own_name, n.key,
                              n.stated_lei, 'unknown') END AS issuer_name,
           CASE WHEN gl.name IS NOT NULL THEN 2 WHEN n.asset_class = 'public_equity' THEN 1 ELSE 0 END AS name_rank,
           CASE WHEN n.has_issuer THEN COALESCE(CASE WHEN NOT n.deposit THEN n.stated_lei END, gn.lei) END AS lei
      FROM n
      LEFT JOIN rf ON rf.fund_ref = n.page_fund
      LEFT JOIN yield_cycles cy ON cy.sector = n.sector
      LEFT JOIN gn ON gn.norm = n.norm
      LEFT JOIN gl ON gl.lei = COALESCE(CASE WHEN NOT n.deposit THEN n.stated_lei END, gn.lei)
);

-- exposure_page_breakdown: a fund's newest breakdown of one field on its
-- page within the exposure's max_gap_days, its fund's cash left out: each
-- bucket's share of the rest, and whether the bucket comes from a label
-- the map does not know (unknown).
CREATE OR REPLACE MACRO exposure_page_breakdown(p_asof, p_field) AS TABLE (
    SELECT fund, bucket, unknown, SUM(v) / SUM(SUM(v)) OVER (PARTITION BY fund) AS share
      FROM (SELECT exposure_key(f.fund_ref) AS fund,
                   CASE p_field WHEN 'credit_quality' THEN exposure_rating_label(f.label)
                                ELSE exposure_maturity_label(f.label) END AS mapped,
                   COALESCE(mapped, CASE p_field WHEN 'credit_quality' THEN 'unrated' ELSE 'unknown' END) AS bucket,
                   mapped IS NULL AS unknown, f.value::DOUBLE AS v
              FROM fund_characteristics f, exposure_settings_row() s
             WHERE f.field = p_field AND f.as_of <= p_asof
               AND (p_asof // 86400) - (f.as_of // 86400) <= s.max_gap_days
           QUALIFY dense_rank() OVER (PARTITION BY exposure_key(f.fund_ref) ORDER BY f.as_of DESC, f.provider) = 1)
     WHERE bucket <> 'cash' AND v > 0
     GROUP BY fund, bucket, unknown
);

-- exposure_credit_leaves: each leaf's credit bucket: cash; (none) for a
-- leaf of no debt; a debt leaf's rating; else the credit-quality
-- breakdown of its page fund, the leaf split by its shares; else
-- unrated. unknown marks a debt leaf no rating or known label describes.
CREATE OR REPLACE MACRO exposure_credit_leaves(p_asof, p_ccy) AS TABLE (
    WITH l AS MATERIALIZED (SELECT * FROM exposure_yield_leaves(p_asof, p_ccy))
    SELECT l.src, l.acct, l.position_key, l.depth, l.flags, l.value * COALESCE(b.share, 1) AS value,
           exposure_credit_bucket(l.asset_class, l.debt, l.rating, b.bucket) AS bucket,
           exposure_credit_unknown(l.debt, l.rating, b.unknown) AS unknown
      FROM l
      LEFT JOIN exposure_page_breakdown(p_asof, 'credit_quality') b
             ON l.debt AND l.rating IS NULL AND b.fund = l.page_fund
);

-- exposure_maturity_leaves: each leaf's maturity bucket; a debt leaf no
-- maturity describes split by its page fund's maturity breakdown.
CREATE OR REPLACE MACRO exposure_maturity_leaves(p_asof, p_ccy) AS TABLE (
    WITH l AS MATERIALIZED (SELECT * FROM exposure_yield_leaves(p_asof, p_ccy))
    SELECT l.src, l.acct, l.position_key, l.depth, l.flags, l.value * COALESCE(b.share, 1) AS value,
           exposure_maturity_split(l.maturity_bucket, b.bucket) AS bucket
      FROM l
      LEFT JOIN exposure_page_breakdown(p_asof, 'maturity_breakdown') b
             ON l.maturity_bucket = 'unknown' AND b.fund = l.page_fund
);

-- report_exposure_credit: one row per credit bucket (AAA to B, B and
-- below; unrated; cash; (none)), with grade IG or HY, and the share of an
-- unrated row no source describes (`unknown=NN%`).
CREATE OR REPLACE MACRO report_exposure_credit(p_asof, p_ccy) AS TABLE (
    WITH c AS (SELECT * FROM exposure_credit_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM c)
    SELECT bucket,
           CASE WHEN bucket IN ('AAA', 'AA', 'A', 'BBB') THEN 'IG' WHEN bucket IN ('BB', 'B') THEN 'HY' END AS grade,
           SUM(value) AS value,
           100 * SUM(value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           concat_ws(',', nullif(exposure_quality(flags, value), ''),
                     exposure_flag_share('unknown', SUM(abs(value)) FILTER (WHERE unknown), SUM(abs(value))))
               AS quality
      FROM c, t
     GROUP BY bucket, t.total
     ORDER BY list_position(['AAA', 'AA', 'A', 'BBB', 'BB', 'B', 'unrated', 'cash', '(none)'], bucket)
);

-- report_exposure_maturities: one row per maturity bucket (under 1y to
-- over 20y, perpetual, unknown, (none)).
CREATE OR REPLACE MACRO report_exposure_maturities(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_maturity_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT bucket, SUM(value) AS value,
           100 * SUM(value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(flags, value) AS quality
      FROM l, t
     GROUP BY bucket, t.total
     ORDER BY list_position(['under 1y', '1-3y', '3-5y', '5-10y', '10-20y', 'over 20y', 'perpetual', 'unknown',
                             '(none)'], bucket)
);

-- report_exposure_coupons: one row per coupon kind of the debt (fixed,
-- floating, inflation_linked, zero, unknown), the rest (none).
CREATE OR REPLACE MACRO report_exposure_coupons(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_yield_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT coupon_bucket AS coupon_kind, SUM(value) AS value,
           100 * SUM(value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(flags, value) AS quality
      FROM l, t
     GROUP BY coupon_bucket, t.total
     ORDER BY list_position(['fixed', 'floating', 'inflation_linked', 'zero', 'unknown', '(none)'], coupon_bucket)
);

-- report_exposure_cycles: one row per cycle of the public equity
-- (cyclical, defensive, sensitive, unknown), the rest (none).
CREATE OR REPLACE MACRO report_exposure_cycles(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_yield_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT cycle, SUM(value) AS value,
           100 * SUM(value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(flags, value) AS quality
      FROM l, t
     GROUP BY cycle, t.total
     ORDER BY list_position(['cyclical', 'defensive', 'sensitive', 'unknown', '(none)'], cycle)
);

-- report_exposure_issuers: one row per issuer, its stock, bonds and
-- deposits together, the largest first, with the running share of net
-- worth (cumulative_share_pct) and what part of the issuer's value is
-- equity, debt and cash. Its name is GLEIF's, else its stock's, else its
-- largest leaf's. The leaves no issuer applies to are one (none) row at
-- the end.
CREATE OR REPLACE MACRO report_exposure_issuers(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT *, COALESCE(issuer_key, '(none)') AS ik FROM exposure_yield_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l),
    g AS (
        SELECT ik AS issuer_key,
               CASE WHEN ik = '(none)' THEN '(none)'
                    ELSE first(issuer_name ORDER BY name_rank DESC, abs(value) DESC, issuer_name) END AS issuer,
               MIN(lei) AS lei, SUM(value) AS value,
               SUM(value) FILTER (WHERE asset_class = 'public_equity') AS equity,
               SUM(value) FILTER (WHERE debt) AS debt,
               SUM(value) FILTER (WHERE deposit) AS cash,
               COUNT(DISTINCT key) AS securities,
               exposure_quality(flags, value) AS quality
          FROM l GROUP BY ik)
    SELECT g.issuer, g.lei, g.value, 100 * g.value / NULLIF(t.total, 0) AS share_pct,
           CASE WHEN g.issuer_key <> '(none)' THEN
                100 * SUM(g.value) OVER (ORDER BY g.issuer_key = '(none)', g.value DESC, g.issuer_key
                                         ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) / NULLIF(t.total, 0) END
               AS cumulative_share_pct,
           100 * COALESCE(g.equity, 0) / NULLIF(g.value, 0) AS equity_pct,
           100 * COALESCE(g.debt, 0) / NULLIF(g.value, 0) AS debt_pct,
           100 * COALESCE(g.cash, 0) / NULLIF(g.value, 0) AS cash_pct,
           g.securities, g.quality
      FROM g, t
     ORDER BY g.issuer_key = '(none)', g.value DESC, g.issuer_key
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (137, CAST(epoch(now()) AS BIGINT));
