-- ============================================================
-- gold schema, migration 0132 — market cap and size.
--
-- * exposure_size_funds: the broad funds whose weights size the stocks
--   no market cap sizes (config exposure.size_funds), each with the share
--   of its markets' value it covers, in percent.
-- * securities.size, size_origin: a stock's size bucket (large, mid,
--   small) and what it rests on: its market cap, or its rank in a size
--   fund (exposure_sizes.go).
-- * report_exposure_sizes: one row per asset class and size; a size
--   applies to public equity alone.
-- * report_exposure_securities: 0126's, with each stock's size.
--
-- IF NOT EXISTS and OR REPLACE keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

CREATE TABLE IF NOT EXISTS exposure_size_funds (
    fund_ref      TEXT    NOT NULL PRIMARY KEY,
    coverage_pct  DOUBLE  NOT NULL
);

ALTER TABLE securities ADD COLUMN IF NOT EXISTS size TEXT;
ALTER TABLE securities ADD COLUMN IF NOT EXISTS size_origin TEXT;

-- report_exposure_sizes: one row per asset class and size, with the
-- share within the asset class. A public equity leaf takes its
-- security's size, `unclassified` where none is known or where it is a
-- fund nothing looks through; any other leaf has no size, `(none)`.
CREATE OR REPLACE MACRO report_exposure_sizes(p_asof, p_ccy) AS TABLE (
    WITH l AS (
        SELECT l.*, CASE WHEN l.asset_class <> 'public_equity' THEN '(none)'
                         WHEN l.fund_leaf THEN 'unclassified'
                         ELSE COALESCE(s.size, 'unclassified') END AS size
          FROM exposure_leaves(p_asof, p_ccy) l
          LEFT JOIN securities s ON s.security_key = l.key),
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
              CASE l.size WHEN 'large' THEN 1 WHEN 'mid' THEN 2 WHEN 'small' THEN 3 WHEN 'unclassified' THEN 4 ELSE 5 END
);

-- report_exposure_securities: one row per security, held directly and
-- through every fund. A leaf with no security is a row of its own: cash
-- per currency (a balance, or a fund's cash line that names nothing
-- else), any other fund line by its fund and name, a holding by its
-- instrument, a remainder or leverage leg by its fund. A stock carries
-- its size; a fund nothing looks through does not.
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
           arg_max(g.currency, abs(g.value)) AS currency, MAX(g.market_cap) AS market_cap, MAX(s.size) AS size,
           SUM(g.value) AS value,
           COALESCE(SUM(g.value) FILTER (WHERE g.depth = 0), 0) AS direct_value,
           COALESCE(SUM(g.value) FILTER (WHERE g.depth > 0), 0) AS via_funds_value,
           COUNT(DISTINCT g.top_key) FILTER (WHERE g.depth > 0) AS funds,
           100 * SUM(g.value) / NULLIF(t.total, 0) AS share_pct,
           exposure_quality(g.flags, g.value) AS quality
      FROM g LEFT JOIN securities s ON s.security_key = g.key AND g.asset_class = 'public_equity' AND NOT g.fund_leaf, t
     GROUP BY g.security, t.total
     ORDER BY abs(SUM(g.value)) DESC, security
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (132, CAST(epoch(now()) AS BIGINT));
