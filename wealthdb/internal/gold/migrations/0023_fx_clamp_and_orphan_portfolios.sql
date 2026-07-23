-- Two fixes:
--
-- 1. FX clamp. fx_daily previously returned no rate for a line whose snapshot
--    predates its currency's FX history (e.g. a 2022 CHF holding when CHF->USD
--    rates only start 2023), so the line failed to convert (NULL) and dropped
--    out of the rollups. Extend each pair's EARLIEST rate back to day 0 via a
--    floor row, so the macros' at-or-before ASOF clamps to the earliest
--    available rate instead of failing. Rates within history are unaffected
--    (a real day always out-ranks the day-0 floor in the ASOF).
--
-- 2. Orphan portfolios. report_portfolios / report_portfolios_history routed an
--    account to the bucket named by its portfolio_external_id, but only the
--    portfolios-table rows (+ a '' sentinel for NULL-portfolio accounts) were
--    real buckets — so an account whose portfolio_external_id is set but absent
--    from the portfolios table (e.g. a mortgage account) was silently dropped from
--    the rollup, making portfolios != accounts. Route those orphans to the same
--    '' catch-all the NULL-portfolio accounts use.

-- ---- 1. fx_daily with the earliest-rate floor (clamp) -----------------------
CREATE OR REPLACE VIEW fx_daily AS
    WITH per_day AS (
        SELECT from_ccy, to_ccy, day, rate
          FROM (
            SELECT from_ccy, to_ccy, (snapshot_at // 86400) AS day, rate,
                   ROW_NUMBER() OVER (
                       PARTITION BY from_ccy, to_ccy, (snapshot_at // 86400)
                       ORDER BY pri ASC, snapshot_at DESC
                   ) AS rn
              FROM fx_norm
          )
         WHERE rn = 1)
    SELECT from_ccy, to_ccy, day, rate FROM per_day
    UNION ALL
    SELECT from_ccy, to_ccy, 0 AS day, arg_min(rate, day) AS rate
      FROM per_day GROUP BY from_ccy, to_ccy;

-- ---- 2a. report_portfolios (orphan -> '' bucket) ----------------------------
CREATE OR REPLACE MACRO report_portfolios(p_asof, p_ccy) AS TABLE (
    WITH lp AS (SELECT silver_source_id, MAX(snapshot_at) AS snap FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    acct_port AS (
        SELECT a.silver_source_id AS src, a.account_external_id AS acct,
               CASE WHEN pf.portfolio_external_id IS NOT NULL THEN a.portfolio_external_id ELSE '' END AS pid
          FROM accounts a
          LEFT JOIN portfolios pf ON pf.silver_source_id = a.silver_source_id AND pf.portfolio_external_id = a.portfolio_external_id),
    taxo AS (
        SELECT a.silver_source_id AS src,
               CASE WHEN pf.portfolio_external_id IS NOT NULL THEN a.portfolio_external_id ELSE '' END AS pid,
            CASE WHEN COUNT(*) FILTER (WHERE account_kind != 'overlay') > 0
                  AND COUNT(DISTINCT CASE WHEN account_kind != 'overlay' THEN COALESCE(tax_wrapper, 'taxable_personal') END) = 1
                 THEN MAX(CASE WHEN account_kind != 'overlay' THEN COALESCE(tax_wrapper, 'taxable_personal') END) END AS tax_wrapper,
            CASE WHEN COUNT(*) FILTER (WHERE account_kind != 'overlay') > 0
                  AND COUNT(DISTINCT CASE WHEN account_kind != 'overlay' THEN COALESCE(management_style, 'self_directed') END) = 1
                 THEN MAX(CASE WHEN account_kind != 'overlay' THEN COALESCE(management_style, 'self_directed') END) END AS management_style,
            CASE WHEN COUNT(*) FILTER (WHERE account_kind != 'overlay') > 0
                  AND COUNT(*) FILTER (WHERE account_kind != 'overlay' AND a.base_currency IS NULL) = 0
                  AND COUNT(DISTINCT CASE WHEN account_kind != 'overlay' THEN a.base_currency END) = 1
                 THEN MAX(CASE WHEN account_kind != 'overlay' THEN a.base_currency END) END AS rolled_base
          FROM accounts a
          LEFT JOIN portfolios pf ON pf.silver_source_id = a.silver_source_id AND pf.portfolio_external_id = a.portfolio_external_id
         GROUP BY 1, 2),
    buckets AS (
        SELECT silver_source_id AS src, portfolio_external_id AS pid, display_name, relationship_id, nickname, base_currency
          FROM portfolios
        UNION ALL
        SELECT DISTINCT src, '' AS pid, CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR)
          FROM acct_port WHERE pid = ''),
    bk AS (
        SELECT b.src, b.pid, b.display_name, b.relationship_id, b.nickname,
               COALESCE(NULLIF(b.base_currency, ''), t.rolled_base) AS base_currency,
               t.tax_wrapper, t.management_style
          FROM buckets b LEFT JOIN taxo t ON t.src = b.src AND t.pid = b.pid),
    lines AS (
        SELECT p.silver_source_id AS src, ap.pid, p.currency AS ccy, p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.silver_source_id AND p.snapshot_at = lp.snap
          JOIN acct_port ap ON ap.src = p.silver_source_id AND ap.acct = p.account_external_id
        UNION ALL
        SELECT cc.silver_source_id, ap.pid, cc.currency, cc.amount, cc.snapshot_at, TRUE
          FROM cash_chosen(p_asof) cc JOIN acct_port ap ON ap.src = cc.silver_source_id AND ap.acct = cc.account_external_id),
    conv AS (
        SELECT l.src, l.pid, l.snap, l.is_cash, bk.base_currency,
               CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate, l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val,
               CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
                   CAST(COALESCE(CASE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                       l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate, l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
          FROM lines l
          JOIN bk ON bk.src = l.src AND bk.pid = l.pid
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)),
    agg AS (
        SELECT src, pid,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(out_val) FILTER (WHERE NOT is_cash) AS n_po, COUNT(out_val) FILTER (WHERE is_cash) AS n_co,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(out_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_o, CAST(SUM(out_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_o,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               MAX(snap) AS max_snap
          FROM conv GROUP BY 1, 2),
    ssnap AS (SELECT src, MAX(snap) AS s FROM lines GROUP BY 1),
    vals AS (
        SELECT bk.src, bk.pid, bk.display_name, bk.base_currency, bk.relationship_id, bk.nickname,
               bk.tax_wrapper, bk.management_style,
               COALESCE(g.max_snap, ss.s, 0) AS snapshot_at,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po=0 THEN NULL ELSE g.pos_o END AS DECIMAL(28,4)) AS pvo,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co=0 THEN NULL ELSE g.cash_o END AS DECIMAL(28,4)) AS cvo
          FROM bk
          LEFT JOIN agg g ON bk.src = g.src AND bk.pid = g.pid
          LEFT JOIN ssnap ss ON bk.src = ss.src)
    SELECT src AS silver_source_id, pid AS portfolio_external_id, display_name, base_currency,
           relationship_id, nickname, tax_wrapper, management_style, snapshot_at,
           CAST(pvb AS VARCHAR) AS positions_value_base,
           CAST(cvb AS VARCHAR) AS cash_balance_base,
           CAST(CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS VARCHAR) AS total_value_base,
           CAST(pvo AS VARCHAR) AS positions_value_outccy,
           CAST(cvo AS VARCHAR) AS cash_balance_outccy,
           CAST(CASE WHEN pvo IS NULL OR cvo IS NULL THEN NULL ELSE pvo + cvo END AS VARCHAR) AS total_value_outccy
      FROM vals
     ORDER BY src, pid
);

-- ---- 2b. report_portfolios_history (orphan -> '' bucket) --------------------
CREATE OR REPLACE MACRO report_portfolios_history(p_ccy) AS TABLE (
    WITH cash_all AS (
        SELECT src, acct, ccy, amt, snap FROM (
            SELECT cb.silver_source_id AS src, cb.account_external_id AS acct, cb.currency AS ccy,
                   cb.amount AS amt, cb.snapshot_at AS snap,
                   ROW_NUMBER() OVER (PARTITION BY cb.silver_source_id, cb.snapshot_at, cb.account_external_id, cb.currency
                       ORDER BY CASE cb.balance_kind WHEN 'current' THEN 1 WHEN 'closing' THEN 2 WHEN 'available' THEN 3
                           WHEN 'aggregated' THEN 4 WHEN 'opening' THEN 5 WHEN 'initial' THEN 6 WHEN 'projected' THEN 7 ELSE 99 END,
                           cb.balance_kind) AS rn
              FROM cash_balances cb)
        WHERE rn = 1 AND amt <> 0),
    acct_port AS (
        SELECT a.silver_source_id AS src, a.account_external_id AS acct,
               CASE WHEN pf.portfolio_external_id IS NOT NULL THEN a.portfolio_external_id ELSE '' END AS pid
          FROM accounts a
          LEFT JOIN portfolios pf ON pf.silver_source_id = a.silver_source_id AND pf.portfolio_external_id = a.portfolio_external_id),
    taxo AS (
        SELECT a.silver_source_id AS src,
               CASE WHEN pf.portfolio_external_id IS NOT NULL THEN a.portfolio_external_id ELSE '' END AS pid,
            CASE WHEN COUNT(*) FILTER (WHERE account_kind != 'overlay') > 0
                  AND COUNT(DISTINCT CASE WHEN account_kind != 'overlay' THEN COALESCE(tax_wrapper, 'taxable_personal') END) = 1
                 THEN MAX(CASE WHEN account_kind != 'overlay' THEN COALESCE(tax_wrapper, 'taxable_personal') END) END AS tax_wrapper,
            CASE WHEN COUNT(*) FILTER (WHERE account_kind != 'overlay') > 0
                  AND COUNT(DISTINCT CASE WHEN account_kind != 'overlay' THEN COALESCE(management_style, 'self_directed') END) = 1
                 THEN MAX(CASE WHEN account_kind != 'overlay' THEN COALESCE(management_style, 'self_directed') END) END AS management_style,
            CASE WHEN COUNT(*) FILTER (WHERE account_kind != 'overlay') > 0
                  AND COUNT(*) FILTER (WHERE account_kind != 'overlay' AND a.base_currency IS NULL) = 0
                  AND COUNT(DISTINCT CASE WHEN account_kind != 'overlay' THEN a.base_currency END) = 1
                 THEN MAX(CASE WHEN account_kind != 'overlay' THEN a.base_currency END) END AS rolled_base
          FROM accounts a
          LEFT JOIN portfolios pf ON pf.silver_source_id = a.silver_source_id AND pf.portfolio_external_id = a.portfolio_external_id
         GROUP BY 1, 2),
    buckets AS (
        SELECT silver_source_id AS src, portfolio_external_id AS pid, display_name, relationship_id, nickname, base_currency
          FROM portfolios
        UNION ALL
        SELECT DISTINCT src, '' AS pid, CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR)
          FROM acct_port WHERE pid = ''),
    bk AS (
        SELECT b.src, b.pid, b.display_name, b.relationship_id, b.nickname,
               COALESCE(NULLIF(b.base_currency, ''), t.rolled_base) AS base_currency,
               t.tax_wrapper, t.management_style
          FROM buckets b LEFT JOIN taxo t ON t.src = b.src AND t.pid = b.pid),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, ap.pid, p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p JOIN acct_port ap ON ap.src = p.silver_source_id AND ap.acct = p.account_external_id
        UNION ALL
        SELECT cc.src, cc.snap, ap.pid, cc.ccy, cc.amt, TRUE
          FROM cash_all cc JOIN acct_port ap ON ap.src = cc.src AND ap.acct = cc.acct),
    conv AS (
        SELECT l.src, l.snap, l.pid, l.is_cash, bk.base_currency,
               CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate, l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val,
               CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
                   CAST(COALESCE(CASE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                       l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate, l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
          FROM lines l
          JOIN bk ON bk.src = l.src AND bk.pid = l.pid
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.pid, c.is_cash, c.base_currency, c.out_val, c.base_val
          FROM conv c JOIN hist_active_pos() ap ON ap.src = c.src AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.pid, c.is_cash, c.base_currency, c.out_val, c.base_val
          FROM conv c JOIN hist_active_cash() ac ON ac.src = c.src AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src, pid,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(out_val) FILTER (WHERE NOT is_cash) AS n_po, COUNT(out_val) FILTER (WHERE is_cash) AS n_co,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(out_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_o, CAST(SUM(out_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_o,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b
          FROM daily GROUP BY 1, 2, 3),
    vals AS (
        SELECT g.day, bk.src, bk.pid, bk.display_name, bk.base_currency, bk.relationship_id, bk.nickname,
               bk.tax_wrapper, bk.management_style,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po=0 THEN NULL ELSE g.pos_o END AS DECIMAL(28,4)) AS pvo,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co=0 THEN NULL ELSE g.cash_o END AS DECIMAL(28,4)) AS cvo
          FROM agg g JOIN bk ON bk.src = g.src AND bk.pid = g.pid)
    SELECT day * 86400 AS as_of_day, src AS silver_source_id, pid AS portfolio_external_id, display_name, base_currency,
           relationship_id, nickname, tax_wrapper, management_style,
           CAST(pvb AS VARCHAR) AS positions_value_base,
           CAST(cvb AS VARCHAR) AS cash_balance_base,
           CAST(CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS VARCHAR) AS total_value_base,
           CAST(pvo AS VARCHAR) AS positions_value_outccy,
           CAST(cvo AS VARCHAR) AS cash_balance_outccy,
           CAST(CASE WHEN pvo IS NULL OR cvo IS NULL THEN NULL ELSE pvo + cvo END AS VARCHAR) AS total_value_outccy
      FROM vals
     ORDER BY as_of_day, src, pid
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (23, CAST(epoch(now()) AS BIGINT));
