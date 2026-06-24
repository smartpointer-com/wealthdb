-- Daily-history variants of the report macros (migration 0021), for time-series
-- analytics in Metabase. Each report_*_history(p_ccy) produces one row per entity per
-- UTC day, from the first snapshot day to today, with the value CARRIED FORWARD: on a
-- day with no new snapshot, the most recent snapshot's value is repeated (positions and
-- cash carried independently, since their snapshot series can diverge). Every line is
-- valued once at its own snapshot's FX day, then expanded onto the daily spine via an
-- at-or-before ASOF join — so history@today == the report_*(MAX) "latest" values, and
-- per-day compute stays cheap. Empty entity-days are omitted (they would be 0 and do not
-- affect any Σ). `as_of_day` is epoch-seconds at UTC midnight (rendered TIMESTAMP by the
-- Metabase models).

-- Daily spine: first snapshot day .. today (UTC). Empty gold -> no rows.
CREATE OR REPLACE MACRO hist_days() AS TABLE (
    WITH b AS (
        SELECT MIN(d) AS lo, GREATEST(MAX(d), CAST(epoch(now()) AS BIGINT) // 86400) AS hi
          FROM (SELECT DISTINCT snapshot_at // 86400 AS d FROM positions
                UNION SELECT DISTINCT snapshot_at // 86400 FROM cash_balances))
    SELECT unnest(generate_series(b.lo, b.hi)) AS day FROM b WHERE b.lo IS NOT NULL
);

-- Active snapshot per (source, day): latest snapshot_at at-or-before end-of-day.
-- Positions and cash are independent (a source's two series can diverge).
CREATE OR REPLACE MACRO hist_active_pos() AS TABLE (
    SELECT s.day, sc.src, ps.snap
      FROM hist_days() s
      CROSS JOIN (SELECT DISTINCT silver_source_id AS src FROM positions) sc
      ASOF JOIN (SELECT DISTINCT silver_source_id AS src, snapshot_at AS snap FROM positions) ps
        ON ps.src = sc.src AND ps.snap <= s.day * 86400 + 86399
);
CREATE OR REPLACE MACRO hist_active_cash() AS TABLE (
    SELECT s.day, sc.src, cs.snap
      FROM hist_days() s
      CROSS JOIN (SELECT DISTINCT silver_source_id AS src FROM cash_balances) sc
      ASOF JOIN (SELECT DISTINCT silver_source_id AS src, snapshot_at AS snap FROM cash_balances) cs
        ON cs.src = sc.src AND cs.snap <= s.day * 86400 + 86399
);

-- Every (src, snapshot, account) line valued once to out-ccy + the account's base
-- currency, over ALL snapshots (account_line_values without the latest filter).
CREATE OR REPLACE MACRO hist_acct_lines(p_ccy) AS TABLE (
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
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, p.account_external_id AS acct,
               p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p
        UNION ALL
        SELECT src, snap, acct, ccy, amt, TRUE FROM cash_all)
    SELECT l.src, l.snap, l.acct, l.is_cash, a.base_currency,
           CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
               l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate,
               l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val,
           CASE WHEN a.base_currency IS NULL THEN NULL ELSE
               CAST(COALESCE(CASE WHEN l.ccy = a.base_currency THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate,
                   l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
      FROM lines l
      LEFT JOIN accounts a ON l.src = a.silver_source_id AND l.acct = a.account_external_id
      ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = a.base_currency AND b0.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = a.base_currency AND b2.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = a.base_currency AND b4.day <= (l.snap // 86400)
);

-- report_accounts_history: report_accounts per (account, day), value carried forward.
CREATE OR REPLACE MACRO report_accounts_history(p_ccy) AS TABLE (
    WITH conv AS (SELECT * FROM hist_acct_lines(p_ccy)),
    daily AS (
        SELECT ap.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_val, c.base_val
          FROM conv c JOIN hist_active_pos() ap ON ap.src = c.src AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_val, c.base_val
          FROM conv c JOIN hist_active_cash() ac ON ac.src = c.src AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src, acct,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(out_val) FILTER (WHERE NOT is_cash) AS n_po, COUNT(out_val) FILTER (WHERE is_cash) AS n_co,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(out_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_o,
               CAST(SUM(out_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_o,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b,
               CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b
          FROM daily GROUP BY 1, 2, 3),
    vals AS (
        SELECT g.day, a.silver_source_id, a.account_external_id, a.account_kind, a.display_name,
               a.base_currency, a.relationship_id, a.nickname, a.account_category,
               a.portfolio_external_id, a.tax_wrapper, a.management_style,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po=0 THEN NULL ELSE g.pos_o END AS DECIMAL(28,4)) AS pvo,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co=0 THEN NULL ELSE g.cash_o END AS DECIMAL(28,4)) AS cvo
          FROM agg g JOIN accounts a ON a.silver_source_id = g.src AND a.account_external_id = g.acct)
    SELECT day * 86400 AS as_of_day, silver_source_id, account_external_id, account_kind, display_name,
           base_currency, relationship_id, nickname, account_category, portfolio_external_id,
           tax_wrapper, management_style,
           CAST(pvb AS VARCHAR) AS positions_value_base,
           CAST(cvb AS VARCHAR) AS cash_balance_base,
           CAST(CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS VARCHAR) AS total_value_base,
           CAST(pvo AS VARCHAR) AS positions_value_outccy,
           CAST(cvo AS VARCHAR) AS cash_balance_outccy,
           CAST(CASE WHEN pvo IS NULL OR cvo IS NULL THEN NULL ELSE pvo + cvo END AS VARCHAR) AS total_value_outccy
      FROM vals
     ORDER BY as_of_day, silver_source_id, account_external_id
);

-- report_global_history: Σ over report_accounts_history per day (global == Σ accounts).
CREATE OR REPLACE MACRO report_global_history(p_ccy) AS TABLE (
    SELECT as_of_day,
           CAST(COALESCE(SUM(cash_balance_outccy::DECIMAL(28,4)), 0) AS VARCHAR) AS cash_balance_outccy,
           CAST(COALESCE(SUM(positions_value_outccy::DECIMAL(28,4)), 0) AS VARCHAR) AS positions_value_outccy,
           CAST(COALESCE(SUM(total_value_outccy::DECIMAL(28,4)), 0) AS VARCHAR) AS total_value_outccy
      FROM report_accounts_history(p_ccy)
     GROUP BY as_of_day
     ORDER BY as_of_day
);

-- report_positions_history: report_positions per (position, day), value carried forward.
CREATE OR REPLACE MACRO report_positions_history(p_ccy) AS TABLE (
    WITH pv AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name, p.asset_class, p.currency,
               CAST(p.quantity AS VARCHAR) AS quantity, CAST(p.market_value AS VARCHAR) AS market_value,
               CAST(COALESCE(CASE WHEN p.currency = p_ccy THEN p.market_value::DOUBLE END,
                   p.market_value::DOUBLE * d.rate, p.market_value::DOUBLE * c1.rate * c2.rate,
                   p.market_value::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy
          FROM positions p
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id
          ASOF LEFT JOIN fx_daily d  ON d.from_ccy = p.currency AND d.to_ccy = p_ccy AND d.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = p.currency AND c1.to_ccy = 'CHF' AND c1.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = p_ccy AND c2.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = p.currency AND u1.to_ccy = 'USD' AND u1.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = p_ccy AND u2.day <= (p.snapshot_at // 86400))
    SELECT ap.day * 86400 AS as_of_day, pv.src AS silver_source_id, pv.snap AS snapshot_at,
           pv.account_external_id, pv.display_name, pv.relationship_id, pv.nickname, pv.account_category,
           pv.position_key, pv.instrument_external_id, pv.symbol, pv.name, pv.asset_class, pv.currency,
           pv.quantity, pv.market_value, pv.value_outccy
      FROM pv JOIN hist_active_pos() ap ON ap.src = pv.src AND ap.snap = pv.snap
     ORDER BY as_of_day, pv.src, pv.account_external_id, pv.position_key
);

-- report_portfolios_history: report_portfolios per (portfolio, day). Buckets + taxonomy
-- are time-invariant (from accounts/portfolios); only line values vary by snapshot.
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
    acct_port AS (SELECT silver_source_id AS src, account_external_id AS acct, COALESCE(portfolio_external_id, '') AS pid FROM accounts),
    taxo AS (
        SELECT silver_source_id AS src, COALESCE(portfolio_external_id, '') AS pid,
            CASE WHEN COUNT(*) FILTER (WHERE account_kind != 'overlay') > 0
                  AND COUNT(DISTINCT CASE WHEN account_kind != 'overlay' THEN COALESCE(tax_wrapper, 'taxable_personal') END) = 1
                 THEN MAX(CASE WHEN account_kind != 'overlay' THEN COALESCE(tax_wrapper, 'taxable_personal') END) END AS tax_wrapper,
            CASE WHEN COUNT(*) FILTER (WHERE account_kind != 'overlay') > 0
                  AND COUNT(DISTINCT CASE WHEN account_kind != 'overlay' THEN COALESCE(management_style, 'self_directed') END) = 1
                 THEN MAX(CASE WHEN account_kind != 'overlay' THEN COALESCE(management_style, 'self_directed') END) END AS management_style,
            CASE WHEN COUNT(*) FILTER (WHERE account_kind != 'overlay') > 0
                  AND COUNT(*) FILTER (WHERE account_kind != 'overlay' AND base_currency IS NULL) = 0
                  AND COUNT(DISTINCT CASE WHEN account_kind != 'overlay' THEN base_currency END) = 1
                 THEN MAX(CASE WHEN account_kind != 'overlay' THEN base_currency END) END AS rolled_base
          FROM accounts GROUP BY 1, 2),
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
    VALUES (22, CAST(epoch(now()) AS BIGINT));
