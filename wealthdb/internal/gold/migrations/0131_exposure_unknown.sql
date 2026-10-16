-- ============================================================
-- gold schema, migration 0131 — what nothing describes reads
-- `unknown`: a dimension no composition, source or declaration
-- fills, a situs the rule cannot place and its basis, and the `how`
-- of such a leaf. The declarations ledger states the part of a
-- dimension it leaves open as `unknown` too (docs/EXPOSURE.md §9).
-- The macros and the view that carry the label are re-created with
-- it, and the stored tables rewritten: the ledger's leaves, each
-- holding's situs, and report_exposure.
--
-- OR REPLACE and UPDATEs that match nothing twice keep this
-- replayable (see gold.Migrate's REPLAY note).
-- ============================================================

UPDATE exposure_declaration_leaves SET asset_class = 'unknown' WHERE asset_class = 'unclassified';
UPDATE exposure_declaration_leaves SET region = 'unknown' WHERE region = 'unclassified';
UPDATE exposure_declaration_leaves SET country = 'unknown' WHERE country = 'unclassified';
UPDATE exposure_declaration_leaves SET currency = 'unknown' WHERE currency = 'unclassified';
UPDATE exposure_declaration_leaves SET sector = 'unknown' WHERE sector = 'unclassified';
UPDATE exposure_situs SET basis = 'unknown' WHERE basis = 'unclassified';
UPDATE report_exposure SET how = 'unknown' WHERE how = 'unclassified';
UPDATE report_exposure SET situs_basis = 'unknown' WHERE situs_basis = 'unclassified';

-- exposure_leaves: the look-through at an instant (docs/EXPOSURE.md §2).
-- One row per leaf: a holding no composition describes, a part of a
-- declared holding, a fund's line, a remainder (what a fund's weights
-- leave unexplained) or the cash leg of a leveraged proxy. Each carries
-- its value in p_ccy, its class, sector, country, region and currency, a
-- public equity security's size, the quality flags it rests on, the
-- reason it is classified (`how`), and its holding's situs and the basis
-- of it (exposure_situs). The leaves of an instant sum to
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
               fa.currency AS fund_ccy, fa.net_assets, fa.leverage, fa.provider
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
               CAST(NULL AS VARCHAR) AS provider, h.decl, h.expands
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
               CAST(NULL AS INTEGER), CAST(NULL AS BOOLEAN)
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
               CASE WHEN NOT (v.fx_line AND NOT v.fx_unsplit) THEN v.value
                    WHEN v.long_leg THEN v.long_value ELSE v.value - v.long_value END AS value,
               CASE WHEN v.long_leg THEN 'foreign_exchange' ELSE v.class END AS asset_class,
               COALESCE(sec.name, v.name, v.h_name) AS name, v.ticker,
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

CREATE OR REPLACE MACRO report_exposure_sectors(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l),
    c AS (SELECT asset_class, SUM(value) AS class_total FROM l GROUP BY 1)
    SELECT l.asset_class, COALESCE(l.sector, 'unknown') AS sector, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * SUM(l.value) / NULLIF(c.class_total, 0) AS class_share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l JOIN c USING (asset_class), t
     GROUP BY l.asset_class, COALESCE(l.sector, 'unknown'), t.total, c.class_total
     ORDER BY c.class_total DESC, l.asset_class, value DESC, sector
);

-- report_exposure_sizes: one row per asset class and size, with the
-- share within the asset class. A public equity leaf reads its size,
-- `unknown` where none is known or where it is a fund nothing looks
-- through; any other leaf has no size, `(none)`.
CREATE OR REPLACE MACRO report_exposure_sizes(p_asof, p_ccy) AS TABLE (
    WITH l AS (
        SELECT * REPLACE (CASE WHEN asset_class <> 'public_equity' THEN '(none)'
                               ELSE COALESCE(size, 'unknown') END AS size)
          FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l),
    c AS (SELECT asset_class, SUM(value) AS class_total FROM l GROUP BY 1)
    SELECT l.asset_class, l.size, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * SUM(l.value) / NULLIF(c.class_total, 0) AS class_share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l JOIN c USING (asset_class), t
     GROUP BY l.asset_class, l.size, t.total, c.class_total
     ORDER BY c.class_total DESC, l.asset_class,
              CASE l.size WHEN 'large' THEN 1 WHEN 'mid' THEN 2 WHEN 'small' THEN 3 WHEN 'unknown' THEN 4 ELSE 5 END
);

CREATE OR REPLACE MACRO report_exposure_countries(p_asof, p_ccy, p_country := 'risk') AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy, p_country := p_country)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT CASE WHEN l.country IS NOT NULL THEN l.country
                WHEN l.asset_class IN ('metal', 'crypto') THEN '(none)' ELSE 'unknown' END AS country,
           MAX(l.country_name) AS country_name, l.region, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l, t GROUP BY 1, l.region, t.total
     ORDER BY value DESC, country, region
);

CREATE OR REPLACE MACRO report_exposure_currencies(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT COALESCE(l.currency, 'unknown') AS currency, l.currency_kind, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth = 0), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.depth > 0), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l, t GROUP BY 1, 2, t.total
     ORDER BY value DESC, currency
);

-- exposure_situs_quality: an aggregate over a group of leaves, the
-- share of the group's gross value each situs basis covers
-- ("declared=12%,isin=80%"), in the rule's order.
CREATE OR REPLACE MACRO exposure_situs_quality(p_basis, p_value) AS
    array_to_string(list_filter([
        exposure_flag_share('declared', SUM(abs(p_value)) FILTER (WHERE p_basis = 'declared'), SUM(abs(p_value))),
        exposure_flag_share('underlying', SUM(abs(p_value)) FILTER (WHERE p_basis = 'underlying'), SUM(abs(p_value))),
        exposure_flag_share('isin', SUM(abs(p_value)) FILTER (WHERE p_basis = 'isin'), SUM(abs(p_value))),
        exposure_flag_share('domicile', SUM(abs(p_value)) FILTER (WHERE p_basis = 'domicile'), SUM(abs(p_value))),
        exposure_flag_share('custody', SUM(abs(p_value)) FILTER (WHERE p_basis = 'custody'), SUM(abs(p_value))),
        exposure_flag_share('country', SUM(abs(p_value)) FILTER (WHERE p_basis = 'country'), SUM(abs(p_value))),
        exposure_flag_share('unknown', SUM(abs(p_value)) FILTER (WHERE p_basis = 'unknown'), SUM(abs(p_value)))
    ], lambda x: x IS NOT NULL), ',');

-- report_exposure_situs: one row per legal situs, a holding's whole
-- value at its own: no fund is looked through. funds_pct is the share of
-- the row's gross value held in pooled vehicles (vehicle etf or fund),
-- quality the situs bases it rests on.
CREATE OR REPLACE MACRO report_exposure_situs(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT COALESCE(l.situs, 'unknown') AS situs, MAX(co.name) AS country_name, MAX(co.region) AS region,
           SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(abs(l.value)) FILTER (WHERE l.h_vehicle IN ('etf', 'fund')), 0)
               / NULLIF(SUM(abs(l.value)), 0) AS funds_pct,
           exposure_situs_quality(l.situs_basis, l.value) AS quality
      FROM l
      LEFT JOIN exposure_countries co ON co.code = l.situs, t
     GROUP BY 1, t.total
     ORDER BY value DESC, situs
);

-- report_exposure_funds: one row per fund held directly, and one more
-- for all other holdings together, so the view sums to net worth. A fund
-- a declaration describes, and no composition of its own, reads `via`
-- declared, with the declaration's date and the dimensions it states. A
-- fund's situs is its largest holding's.
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
    -- declaration describes.
    k AS (
        SELECT h.*, fa.fund_ref AS comp_ref,
               fa.fund_ref IS NULL AND h.decl IS NOT NULL AND h.vehicle IN ('etf', 'fund') AS declared
          FROM h LEFT JOIN fa ON fa.fund_ref = h.key AND (h.decl IS NULL OR fa.own_as_of IS NOT NULL)),
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
          -- composition (exposure_funds_at).
          LEFT JOIN exposure_attributes_at(p_asof) fat ON f.declared AND fat.fund_ref = f.key, t
         GROUP BY f.fund, t.total)
    SELECT * EXCLUDE (top_situs), top_situs.situs AS situs, top_situs.basis AS situs_basis
      FROM g
     ORDER BY fund = '(other holdings)', abs(value) DESC, fund
);

-- report_exposure_coverage: what the figures rest on. Section `how`
-- splits net worth by how each leaf is classified and sums to it;
-- `age` splits what compositions describe by their distance from the
-- day; `unmapped` lists the provider labels no map knows; `unmatched`
-- the listed securities held directly that no source describes;
-- `undeclared` the private holdings no record of the declarations
-- ledger describes, each with the silver_source_id and instrument a
-- record names; `divergence` each fund read from a report beside its
-- proxy: the value held through it, and how far the two mixes lie
-- apart. `situs` splits net worth by the basis of each holding's situs
-- and sums to it, then lists each holding whose situs is unknown.
-- In the sections that list labels or holdings, what has no gross value
-- shares one line, `(no value)`. A situs line counts holdings.
CREATE OR REPLACE MACRO report_exposure_coverage(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l),
    how AS (
        SELECT 1 AS section_rank, 'how' AS section, how AS reason, COUNT(*) AS items, SUM(value) AS value,
               CAST(NULL AS VARCHAR) AS detail, CAST(NULL AS VARCHAR) AS silver_source_id,
               CAST(NULL AS VARCHAR) AS instrument,
               CASE how WHEN 'looked_through' THEN 1 WHEN 'proxied' THEN 2 WHEN 'security' THEN 3
                        WHEN 'declared' THEN 4 WHEN 'cash' THEN 5 WHEN 'asset_class' THEN 6
                        WHEN 'remainder' THEN 7 WHEN 'truncated' THEN 8 ELSE 9 END AS reason_rank
          FROM l GROUP BY how),
    age AS (
        SELECT 2, 'age', bucket, COUNT(DISTINCT top_key), SUM(value), CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR),
               CAST(NULL AS VARCHAR), bucket_rank
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
    unmapped_items AS (
        SELECT 3 AS section_rank, 'unmapped' AS section, f.field || ': ' || f.raw AS reason, COUNT(*) AS items,
               SUM(f.value) AS value, SUM(abs(f.value)) AS gross, f.provider AS detail,
               CAST(NULL AS VARCHAR) AS silver_source_id, CAST(NULL AS VARCHAR) AS instrument
          FROM (SELECT 'sector' AS field, sector_raw AS raw, provider, value FROM l
                 WHERE depth > 0 AND sector_raw IS NOT NULL AND sector_map IS NULL
                UNION ALL
                SELECT 'country', country_raw, provider, value FROM l
                 WHERE depth > 0 AND country_raw IS NOT NULL AND country_map IS NULL
                UNION ALL
                SELECT 'asset_type', asset_type_raw, provider, value FROM l
                 WHERE depth > 0 AND class_unmapped) f
         GROUP BY f.field, f.raw, f.provider),
    unmatched_items AS (
        SELECT CASE WHEN undeclared THEN 5 ELSE 4 END AS section_rank,
               CASE WHEN undeclared THEN 'undeclared' ELSE 'unmatched' END AS section,
               holding AS reason, COUNT(*) AS items, SUM(value) AS value, SUM(abs(value)) AS gross,
               MAX(name) AS detail, src AS silver_source_id, iid AS instrument
          FROM (SELECT *, COALESCE(key, 'holding:' || src || ':' || COALESCE(iid, position_key)) AS holding,
                       list_contains(flags, 'undeclared') AS undeclared FROM l
                 WHERE list_contains(flags, 'unmatched') OR list_contains(flags, 'undeclared'))
         GROUP BY undeclared, holding, src, iid),
    -- situs: a holding's leaves all carry its situs, so its name is the
    -- holding's and its key the one it is held under.
    situs_bases AS (
        SELECT 7 AS section_rank, 'situs' AS section, situs_basis AS reason,
               COUNT(DISTINCT (src, acct, position_key)) AS items, SUM(value) AS value,
               CAST(NULL AS VARCHAR) AS detail, CAST(NULL AS VARCHAR) AS silver_source_id,
               CAST(NULL AS VARCHAR) AS instrument,
               CASE situs_basis WHEN 'declared' THEN 1 WHEN 'underlying' THEN 2 WHEN 'isin' THEN 3
                                WHEN 'domicile' THEN 4 WHEN 'custody' THEN 5 WHEN 'country' THEN 6 ELSE 7 END
                   AS reason_rank
          FROM l GROUP BY situs_basis),
    situs_items AS (
        SELECT 7 AS section_rank, 'situs' AS section, holding AS reason,
               COUNT(DISTINCT (acct, position_key)) AS items, SUM(value) AS value, SUM(abs(value)) AS gross,
               COALESCE(MAX(name) FILTER (WHERE depth = 0), MAX(h_name)) AS detail,
               src AS silver_source_id, iid AS instrument
          FROM (SELECT *, COALESCE(top_key, 'holding:' || src || ':' || COALESCE(iid, position_key)) AS holding
                  FROM l WHERE situs_basis = 'unknown')
         GROUP BY holding, src, iid),
    -- A label or holding whose leaves have no gross value is listed
    -- only as a count.
    listable AS (
        SELECT * FROM unmapped_items UNION ALL BY NAME SELECT * FROM unmatched_items
        UNION ALL BY NAME SELECT * FROM situs_items),
    listed AS (
        SELECT section_rank, section, reason, items, value, detail, silver_source_id, instrument, 10 AS reason_rank
          FROM listable
         WHERE round(gross, 2) <> 0
        UNION ALL
        SELECT section_rank, section, '(no value)', CAST(SUM(items) AS BIGINT), SUM(value), CAST(NULL AS VARCHAR),
               CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), 11
          FROM listable
         WHERE round(gross, 2) = 0
         GROUP BY section_rank, section),
    divergence AS (
        SELECT 6, 'divergence', d.fund_ref, v.items, v.value,
               concat_ws(', ', printf('sector %.1f', d.sector_points), printf('country %.1f', d.country_points))
                   || ' points from ' || d.proxy_ref,
               CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), 0
          FROM exposure_divergence(p_asof) d
          JOIN (SELECT fund_ref, COUNT(DISTINCT key) AS items, SUM(value) AS value FROM l WHERE depth > 0 GROUP BY 1) v
            USING (fund_ref))
    SELECT section, reason, items, value, 100 * value / NULLIF(t.total, 0) AS share_pct, detail,
           silver_source_id, instrument
      FROM (SELECT * FROM how UNION ALL SELECT * FROM age UNION ALL SELECT * FROM listed
            UNION ALL SELECT * FROM divergence UNION ALL SELECT * FROM situs_bases) x, t
     ORDER BY section_rank, reason_rank, abs(value) DESC, reason, silver_source_id, instrument
);

-- web_exposure: report_exposure as the dashboard reads it, every label
-- filled in: a missing dimension reads unknown, a metal's or a
-- coin's country and region (none), the size of anything but public
-- equity (none). silver_source_id and tax_wrapper are the holding's
-- source and its account's wrapper, NULL where the account states none.
CREATE OR REPLACE VIEW web_exposure AS
SELECT CAST(epoch_ms(r.as_of * 1000) AS DATE) AS as_of_day,
       r.currency,
       r.silver_source_id,
       r.tax_wrapper,
       r.asset_class,
       COALESCE(r.sector, 'unknown') AS sector,
       CASE WHEN r.country IS NOT NULL THEN r.country
            WHEN r.asset_class IN ('metal', 'crypto') THEN '(none)' ELSE 'unknown' END AS country,
       co.name AS country_name,
       COALESCE(r.region, CASE WHEN r.asset_class IN ('metal', 'crypto') THEN '(none)' ELSE 'unknown' END)
           AS region,
       COALESCE(r.exposure_currency, 'unknown') AS exposure_currency,
       r.currency_kind,
       CASE WHEN r.asset_class <> 'public_equity' THEN '(none)' ELSE COALESCE(r.size, 'unknown') END
           AS size,
       CASE WHEN r.via_funds THEN 'via funds' ELSE 'direct' END AS held,
       r.how,
       COALESCE(r.situs, 'unknown') AS situs,
       COALESCE(r.situs_basis, 'unknown') AS situs_basis,
       r.value
  FROM report_exposure r
  LEFT JOIN exposure_countries co ON co.code = r.country;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (131, CAST(epoch(now()) AS BIGINT));
