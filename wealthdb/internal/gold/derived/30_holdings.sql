-- Holdings: the account buckets, the lines held at an instant or on
-- each day, valued, and the holdings reports over them.

-- ===========================================================================
-- PORTFOLIOS: shared buckets/taxonomy + line base + single + multi
-- ===========================================================================

-- portfolio_acct_map: account -> portfolio bucket id, routing orphans (an
-- account whose portfolio_external_id is set but absent from the portfolios
-- table) to the '' catch-all alongside the NULL-portfolio accounts, so an
-- orphan's value is never lost. The single source of truth for the routing,
-- shared by every portfolio macro so it cannot drift between them.
CREATE MACRO portfolio_acct_map() AS TABLE (
    SELECT a.silver_source_id AS src, a.account_external_id AS acct,
           CASE WHEN pf.portfolio_external_id IS NOT NULL THEN a.portfolio_external_id ELSE '' END AS pid
      FROM accounts a
      LEFT JOIN portfolios pf ON pf.silver_source_id = a.silver_source_id AND pf.portfolio_external_id = a.portfolio_external_id
);

-- portfolio_buckets: one bucket per portfolios-table row + a '' sentinel per
-- source that has an orphan/NULL-portfolio account, with the rolled-up
-- taxonomy (tax_wrapper / management_style agree across non-overlay accounts,
-- else NULL) and the effective base currency (portfolios.base_currency wins,
-- else the rolled-up account base). Time- and currency-invariant; the single
-- source of truth for the portfolio rollup, shared by the value macros below.
CREATE MACRO portfolio_buckets() AS TABLE (
    WITH taxo AS (
        SELECT a.silver_source_id AS src, m.pid,
            CASE WHEN COUNT(*) FILTER (WHERE a.account_kind != 'overlay') > 0
                  AND COUNT(DISTINCT CASE WHEN a.account_kind != 'overlay' THEN COALESCE(a.tax_wrapper, 'taxable_personal') END) = 1
                 THEN MAX(CASE WHEN a.account_kind != 'overlay' THEN COALESCE(a.tax_wrapper, 'taxable_personal') END) END AS tax_wrapper,
            CASE WHEN COUNT(*) FILTER (WHERE a.account_kind != 'overlay') > 0
                  AND COUNT(DISTINCT CASE WHEN a.account_kind != 'overlay' THEN COALESCE(a.management_style, 'self_directed') END) = 1
                 THEN MAX(CASE WHEN a.account_kind != 'overlay' THEN COALESCE(a.management_style, 'self_directed') END) END AS management_style,
            CASE WHEN COUNT(*) FILTER (WHERE a.account_kind != 'overlay') > 0
                  AND COUNT(*) FILTER (WHERE a.account_kind != 'overlay' AND a.base_currency IS NULL) = 0
                  AND COUNT(DISTINCT CASE WHEN a.account_kind != 'overlay' THEN a.base_currency END) = 1
                 THEN MAX(CASE WHEN a.account_kind != 'overlay' THEN a.base_currency END) END AS rolled_base
          FROM accounts a JOIN portfolio_acct_map() m ON m.src = a.silver_source_id AND m.acct = a.account_external_id
         GROUP BY 1, 2),
    buckets AS (
        SELECT silver_source_id AS src, portfolio_external_id AS pid, display_name, relationship_id, nickname, base_currency
          FROM portfolios
        UNION ALL
        SELECT DISTINCT src, '' AS pid, CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR)
          FROM portfolio_acct_map() WHERE pid = '')
    SELECT b.src, b.pid, b.display_name, b.relationship_id, b.nickname,
           COALESCE(NULLIF(b.base_currency, ''), t.rolled_base) AS base_currency,
           t.tax_wrapper, t.management_style
      FROM buckets b LEFT JOIN taxo t ON t.src = b.src AND t.pid = b.pid
);

-- The `sources` report: one row per silver source, rolling up every one of
-- its accounts (positions + cash) the same way report_portfolios rolls up a
-- portfolio's accounts — only the bucket key is the silver_source_id instead
-- of the portfolio. Built on the same line/FX machinery (cash_chosen +
-- fx_daily) so sum(sources.total_<ccy>) == sum(accounts.total_<ccy>) ==
-- sum(portfolios.total_<ccy>) by construction.
--
-- Like report_portfolios, base/tax/management are rolled up across the
-- source's non-overlay accounts (agree-or-NULL); the base trio is in that
-- rolled base currency (NULL when the source's accounts disagree), the
-- out-currency trio in p_ccy. NULL-vs-0 semantics mirror report_portfolios:
-- empty bucket -> 0; a bucket with lines but no FX path -> NULL; the base
-- trio -> NULL when the source's base currency is unknown/mixed.
--
-- report_sources_history_multi (below) is the daily-series variant for
-- Metabase, the per-source analogue of report_portfolios_history_multi.

-- source_buckets: one bucket per source (every source with at least one
-- account) carrying its rolled-up taxonomy + effective base currency. The
-- agree-or-NULL rollup over non-overlay accounts mirrors portfolio_buckets;
-- time- and currency-invariant, shared by the value macros below.
CREATE MACRO source_buckets() AS TABLE (
    SELECT a.silver_source_id AS src,
        CASE WHEN COUNT(*) FILTER (WHERE a.account_kind != 'overlay') > 0
              AND COUNT(DISTINCT CASE WHEN a.account_kind != 'overlay' THEN COALESCE(a.tax_wrapper, 'taxable_personal') END) = 1
             THEN MAX(CASE WHEN a.account_kind != 'overlay' THEN COALESCE(a.tax_wrapper, 'taxable_personal') END) END AS tax_wrapper,
        CASE WHEN COUNT(*) FILTER (WHERE a.account_kind != 'overlay') > 0
              AND COUNT(DISTINCT CASE WHEN a.account_kind != 'overlay' THEN COALESCE(a.management_style, 'self_directed') END) = 1
             THEN MAX(CASE WHEN a.account_kind != 'overlay' THEN COALESCE(a.management_style, 'self_directed') END) END AS management_style,
        CASE WHEN COUNT(*) FILTER (WHERE a.account_kind != 'overlay') > 0
              AND COUNT(*) FILTER (WHERE a.account_kind != 'overlay' AND a.base_currency IS NULL) = 0
              AND COUNT(DISTINCT CASE WHEN a.account_kind != 'overlay' THEN a.base_currency END) = 1
             THEN MAX(CASE WHEN a.account_kind != 'overlay' THEN a.base_currency END) END AS base_currency
      FROM accounts a GROUP BY 1
);

-- ---- shared line base, zero balances kept ----------------------------

CREATE MACRO hist_acct_lines_base() AS TABLE (
    WITH cash_all AS (
        SELECT src, acct, ccy, amt, snap FROM (
            SELECT cb.silver_source_id AS src, cb.account_external_id AS acct, cb.currency AS ccy,
                   cb.amount AS amt, cb.snapshot_at AS snap,
                   ROW_NUMBER() OVER (PARTITION BY cb.silver_source_id, cb.snapshot_at, cb.account_external_id, cb.currency
                       ORDER BY CASE cb.balance_kind WHEN 'current' THEN 1 WHEN 'closing' THEN 2 WHEN 'available' THEN 3
                           WHEN 'aggregated' THEN 4 WHEN 'opening' THEN 5 WHEN 'initial' THEN 6 WHEN 'projected' THEN 7 ELSE 99 END,
                           cb.balance_kind) AS rn
              FROM cash_balances cb)
        WHERE rn = 1),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, p.account_external_id AS acct,
               p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p
        UNION ALL
        SELECT src, snap, acct, ccy, amt, TRUE FROM cash_all)
    SELECT l.src, l.snap, l.acct, l.ccy, l.amt, l.is_cash, a.base_currency,
           CASE WHEN a.base_currency IS NULL THEN NULL ELSE
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = a.base_currency THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate,
                   l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
      FROM lines l
      LEFT JOIN accounts a ON l.src = a.silver_source_id AND l.acct = a.account_external_id
      ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = a.base_currency AND b0.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = a.base_currency AND b2.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = a.base_currency AND b4.day <= (l.snap // 86400)
);

-- ---- accounts family -------------------------------------------------

CREATE MACRO report_accounts_history(p_ccy) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.is_cash, l.base_currency, l.base_val,
               CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate,
                   l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val
          FROM hist_acct_lines_base() l
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_val, c.base_val
          FROM conv c JOIN hist_active_pos() ap
            ON ap.src = c.src AND ap.acct = c.acct AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_val, c.base_val
          FROM conv c JOIN hist_active_cash() ac
            ON ac.src = c.src AND ac.acct = c.acct AND ac.ccy = c.ccy AND ac.snap = c.snap WHERE c.is_cash),
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

CREATE MACRO report_accounts_history_multi() AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.is_cash, l.base_currency, l.base_val,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'USD', fxl, fxb) END AS DECIMAL(28,4)) AS out_usd,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'CHF', fxl, fxb) END AS DECIMAL(28,4)) AS out_chf,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'EUR', fxl, fxb) END AS DECIMAL(28,4)) AS out_eur,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'GBP', fxl, fxb) END AS DECIMAL(28,4)) AS out_gbp
          FROM hist_acct_lines_base() l
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = l.ccy AND fxl.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_usd, c.out_chf, c.out_eur, c.out_gbp, c.base_val
          FROM conv c JOIN hist_active_pos() ap
            ON ap.src = c.src AND ap.acct = c.acct AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_usd, c.out_chf, c.out_eur, c.out_gbp, c.base_val
          FROM conv c JOIN hist_active_cash() ac
            ON ac.src = c.src AND ac.acct = c.acct AND ac.ccy = c.ccy AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src, acct,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               COUNT(out_usd) FILTER (WHERE NOT is_cash) AS n_po_u, COUNT(out_usd) FILTER (WHERE is_cash) AS n_co_u,
               CAST(SUM(out_usd) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_u, CAST(SUM(out_usd) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_u,
               COUNT(out_chf) FILTER (WHERE NOT is_cash) AS n_po_c, COUNT(out_chf) FILTER (WHERE is_cash) AS n_co_c,
               CAST(SUM(out_chf) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_c, CAST(SUM(out_chf) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_c,
               COUNT(out_eur) FILTER (WHERE NOT is_cash) AS n_po_e, COUNT(out_eur) FILTER (WHERE is_cash) AS n_co_e,
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e,
               COUNT(out_gbp) FILTER (WHERE NOT is_cash) AS n_po_g, COUNT(out_gbp) FILTER (WHERE is_cash) AS n_co_g,
               CAST(SUM(out_gbp) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_g, CAST(SUM(out_gbp) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_g
          FROM daily GROUP BY 1, 2, 3),
    vals AS (
        SELECT g.day, a.silver_source_id, a.account_external_id, a.account_kind, a.display_name,
               a.base_currency, a.relationship_id, a.nickname, a.account_category,
               a.portfolio_external_id,
               -- account-level display defaults (see report_accounts_multi).
               COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
               COALESCE(a.management_style, 'self_directed') AS management_style,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_u=0 THEN NULL ELSE g.pos_u END AS DECIMAL(28,4)) AS pvu,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_u=0 THEN NULL ELSE g.cash_u END AS DECIMAL(28,4)) AS cvu,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_c=0 THEN NULL ELSE g.pos_c END AS DECIMAL(28,4)) AS pvc,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_c=0 THEN NULL ELSE g.cash_c END AS DECIMAL(28,4)) AS cvc,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_e=0 THEN NULL ELSE g.pos_e END AS DECIMAL(28,4)) AS pve,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_g=0 THEN NULL ELSE g.pos_g END AS DECIMAL(28,4)) AS pvg,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_g=0 THEN NULL ELSE g.cash_g END AS DECIMAL(28,4)) AS cvg
          FROM agg g JOIN accounts a ON a.silver_source_id = g.src AND a.account_external_id = g.acct)
    SELECT day * 86400 AS as_of_day, silver_source_id, account_external_id, account_kind, display_name,
           base_currency, relationship_id, nickname, account_category, portfolio_external_id,
           tax_wrapper, management_style,
           pvb AS positions_value_base, cvb AS cash_balance_base,
           CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS total_value_base,
           pvu AS positions_value_usd, cvu AS cash_balance_usd,
           CASE WHEN pvu IS NULL OR cvu IS NULL THEN NULL ELSE pvu + cvu END AS total_value_usd,
           pvc AS positions_value_chf, cvc AS cash_balance_chf,
           CASE WHEN pvc IS NULL OR cvc IS NULL THEN NULL ELSE pvc + cvc END AS total_value_chf,
           pve AS positions_value_eur, cve AS cash_balance_eur,
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur,
           pvg AS positions_value_gbp, cvg AS cash_balance_gbp,
           CASE WHEN pvg IS NULL OR cvg IS NULL THEN NULL ELSE pvg + cvg END AS total_value_gbp
      FROM vals
     ORDER BY as_of_day, silver_source_id, account_external_id
);

CREATE MACRO report_global_history_multi() AS TABLE (
    SELECT as_of_day,
           CAST(COALESCE(SUM(cash_balance_usd), 0) AS DECIMAL(28,4)) AS cash_balance_usd,
           CAST(COALESCE(SUM(positions_value_usd), 0) AS DECIMAL(28,4)) AS positions_value_usd,
           CAST(COALESCE(SUM(total_value_usd), 0) AS DECIMAL(28,4)) AS total_value_usd,
           CAST(COALESCE(SUM(cash_balance_chf), 0) AS DECIMAL(28,4)) AS cash_balance_chf,
           CAST(COALESCE(SUM(positions_value_chf), 0) AS DECIMAL(28,4)) AS positions_value_chf,
           CAST(COALESCE(SUM(total_value_chf), 0) AS DECIMAL(28,4)) AS total_value_chf,
           CAST(COALESCE(SUM(cash_balance_eur), 0) AS DECIMAL(28,4)) AS cash_balance_eur,
           CAST(COALESCE(SUM(positions_value_eur), 0) AS DECIMAL(28,4)) AS positions_value_eur,
           CAST(COALESCE(SUM(total_value_eur), 0) AS DECIMAL(28,4)) AS total_value_eur,
           CAST(COALESCE(SUM(cash_balance_gbp), 0) AS DECIMAL(28,4)) AS cash_balance_gbp,
           CAST(COALESCE(SUM(positions_value_gbp), 0) AS DECIMAL(28,4)) AS positions_value_gbp,
           CAST(COALESCE(SUM(total_value_gbp), 0) AS DECIMAL(28,4)) AS total_value_gbp
      FROM report_accounts_history_multi()
     GROUP BY as_of_day
     ORDER BY as_of_day
);

CREATE MACRO report_card_balances_history_multi() AS TABLE (
    WITH card_rows AS (
        SELECT src, snap, acct, ccy, amt FROM (
            SELECT cb.silver_source_id AS src, cb.snapshot_at AS snap,
                   cb.account_external_id AS acct, cb.currency AS ccy, cb.amount AS amt,
                   ROW_NUMBER() OVER (
                       PARTITION BY cb.silver_source_id, cb.snapshot_at,
                                    cb.account_external_id, cb.currency
                       ORDER BY CASE cb.balance_kind
                           WHEN 'current' THEN 1 WHEN 'closing' THEN 2
                           WHEN 'available' THEN 3 WHEN 'aggregated' THEN 4
                           WHEN 'opening' THEN 5 WHEN 'initial' THEN 6
                           WHEN 'projected' THEN 7 ELSE 99 END, cb.balance_kind) AS rn
              FROM cash_balances cb
              JOIN accounts a ON a.silver_source_id    = cb.silver_source_id
                             AND a.account_external_id = cb.account_external_id
             WHERE a.account_kind = 'card')
        WHERE rn = 1),
    valued AS (
        SELECT c.src, c.snap, c.acct, c.ccy, c.amt,
               CAST(fx_reporting_value(c.amt, c.ccy, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS bal_usd,
               CAST(fx_reporting_value(c.amt, c.ccy, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS bal_chf,
               CAST(fx_reporting_value(c.amt, c.ccy, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS bal_eur,
               CAST(fx_reporting_value(c.amt, c.ccy, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS bal_gbp
          FROM card_rows c
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = c.ccy AND fxl.day <= (c.snap // 86400)
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (c.snap // 86400)),
    spine AS (
        SELECT d.day, k.src, k.acct, k.ccy
          FROM hist_days() d
          CROSS JOIN (SELECT DISTINCT src, acct, ccy FROM card_rows) k)
    SELECT s.day * 86400 AS as_of_day, v.src AS silver_source_id,
           v.acct AS account_external_id, a.display_name, v.ccy AS currency,
           CAST(v.amt AS DECIMAL(28,4)) AS balance,
           v.bal_usd AS balance_usd, v.bal_chf AS balance_chf, v.bal_eur AS balance_eur, v.bal_gbp AS balance_gbp
      FROM spine s
      -- Per-key ASOF: each card carries ITS OWN last balance forward,
      -- so another account's dump day cannot displace it.
      ASOF JOIN valued v ON v.src = s.src AND v.acct = s.acct AND v.ccy = s.ccy
                        AND v.snap <= s.day * 86400 + 86399
      LEFT JOIN accounts a ON a.silver_source_id = v.src AND a.account_external_id = v.acct
     ORDER BY as_of_day, silver_source_id, account_external_id, currency
);

CREATE MACRO report_positions_history_multi() AS TABLE (
    WITH pv AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name, p.asset_class, p.vehicle, p.currency,
               CAST(p.quantity AS DECIMAL(28,8)) AS quantity, CAST(p.market_value AS DECIMAL(28,4)) AS market_value,
               CAST(fx_reporting_value(p.market_value, p.currency, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS value_usd,
               CAST(fx_reporting_value(p.market_value, p.currency, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS value_chf,
               CAST(fx_reporting_value(p.market_value, p.currency, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS value_eur,
               CAST(fx_reporting_value(p.market_value, p.currency, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS value_gbp
          FROM positions p
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = p.currency AND fxl.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (p.snapshot_at // 86400))
    SELECT ap.day * 86400 AS as_of_day, pv.src AS silver_source_id, pv.snap AS snapshot_at,
           pv.account_external_id, pv.display_name, pv.relationship_id, pv.nickname, pv.account_category,
           pv.position_key, pv.instrument_external_id, pv.symbol, pv.name, pv.asset_class, pv.vehicle, pv.currency,
           pv.quantity, pv.market_value, pv.value_usd, pv.value_chf, pv.value_eur, pv.value_gbp
      FROM pv JOIN hist_active_pos() ap
        ON ap.src = pv.src AND ap.acct = pv.account_external_id AND ap.snap = pv.snap
     ORDER BY as_of_day, pv.src, pv.account_external_id, pv.position_key
);

CREATE MACRO report_portfolios_history_multi() AS TABLE (
    WITH cash_all AS (
        SELECT src, acct, ccy, amt, snap FROM (
            SELECT cb.silver_source_id AS src, cb.account_external_id AS acct, cb.currency AS ccy,
                   cb.amount AS amt, cb.snapshot_at AS snap,
                   ROW_NUMBER() OVER (PARTITION BY cb.silver_source_id, cb.snapshot_at, cb.account_external_id, cb.currency
                       ORDER BY CASE cb.balance_kind WHEN 'current' THEN 1 WHEN 'closing' THEN 2 WHEN 'available' THEN 3
                           WHEN 'aggregated' THEN 4 WHEN 'opening' THEN 5 WHEN 'initial' THEN 6 WHEN 'projected' THEN 7 ELSE 99 END,
                           cb.balance_kind) AS rn
              FROM cash_balances cb)
        WHERE rn = 1),
    acct_port AS (SELECT * FROM portfolio_acct_map()),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, ap.acct, ap.pid, p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p JOIN acct_port ap ON ap.src = p.silver_source_id AND ap.acct = p.account_external_id
        UNION ALL
        SELECT cc.src, cc.snap, ap.acct, ap.pid, cc.ccy, cc.amt, TRUE
          FROM cash_all cc JOIN acct_port ap ON ap.src = cc.src AND ap.acct = cc.acct),
    conv AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.pid, l.is_cash, bk.base_currency,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'USD', fxl, fxb) END AS DECIMAL(28,4)) AS out_usd,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'CHF', fxl, fxb) END AS DECIMAL(28,4)) AS out_chf,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'EUR', fxl, fxb) END AS DECIMAL(28,4)) AS out_eur,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'GBP', fxl, fxb) END AS DECIMAL(28,4)) AS out_gbp,
               CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
                   CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                       l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate, l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
          FROM lines l
          JOIN portfolio_buckets() bk ON bk.src = l.src AND bk.pid = l.pid
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = l.ccy AND fxl.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.pid, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.out_gbp, c.base_val
          FROM conv c JOIN hist_active_pos() ap
            ON ap.src = c.src AND ap.acct = c.acct AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.pid, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.out_gbp, c.base_val
          FROM conv c JOIN hist_active_cash() ac
            ON ac.src = c.src AND ac.acct = c.acct AND ac.ccy = c.ccy AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src, pid,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               COUNT(out_usd) FILTER (WHERE NOT is_cash) AS n_po_u, COUNT(out_usd) FILTER (WHERE is_cash) AS n_co_u,
               CAST(SUM(out_usd) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_u, CAST(SUM(out_usd) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_u,
               COUNT(out_chf) FILTER (WHERE NOT is_cash) AS n_po_c, COUNT(out_chf) FILTER (WHERE is_cash) AS n_co_c,
               CAST(SUM(out_chf) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_c, CAST(SUM(out_chf) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_c,
               COUNT(out_eur) FILTER (WHERE NOT is_cash) AS n_po_e, COUNT(out_eur) FILTER (WHERE is_cash) AS n_co_e,
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e,
               COUNT(out_gbp) FILTER (WHERE NOT is_cash) AS n_po_g, COUNT(out_gbp) FILTER (WHERE is_cash) AS n_co_g,
               CAST(SUM(out_gbp) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_g, CAST(SUM(out_gbp) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_g
          FROM daily GROUP BY 1, 2, 3),
    vals AS (
        SELECT g.day, bk.src, bk.pid, bk.display_name, bk.base_currency, bk.relationship_id, bk.nickname,
               bk.tax_wrapper, bk.management_style,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_u=0 THEN NULL ELSE g.pos_u END AS DECIMAL(28,4)) AS pvu,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_u=0 THEN NULL ELSE g.cash_u END AS DECIMAL(28,4)) AS cvu,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_c=0 THEN NULL ELSE g.pos_c END AS DECIMAL(28,4)) AS pvc,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_c=0 THEN NULL ELSE g.cash_c END AS DECIMAL(28,4)) AS cvc,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_e=0 THEN NULL ELSE g.pos_e END AS DECIMAL(28,4)) AS pve,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_g=0 THEN NULL ELSE g.pos_g END AS DECIMAL(28,4)) AS pvg,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_g=0 THEN NULL ELSE g.cash_g END AS DECIMAL(28,4)) AS cvg
          FROM agg g JOIN portfolio_buckets() bk ON bk.src = g.src AND bk.pid = g.pid)
    SELECT day * 86400 AS as_of_day, src AS silver_source_id, pid AS portfolio_external_id, display_name, base_currency,
           relationship_id, nickname, tax_wrapper, management_style,
           pvb AS positions_value_base, cvb AS cash_balance_base,
           CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS total_value_base,
           pvu AS positions_value_usd, cvu AS cash_balance_usd,
           CASE WHEN pvu IS NULL OR cvu IS NULL THEN NULL ELSE pvu + cvu END AS total_value_usd,
           pvc AS positions_value_chf, cvc AS cash_balance_chf,
           CASE WHEN pvc IS NULL OR cvc IS NULL THEN NULL ELSE pvc + cvc END AS total_value_chf,
           pve AS positions_value_eur, cve AS cash_balance_eur,
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur,
           pvg AS positions_value_gbp, cvg AS cash_balance_gbp,
           CASE WHEN pvg IS NULL OR cvg IS NULL THEN NULL ELSE pvg + cvg END AS total_value_gbp
      FROM vals
     ORDER BY as_of_day, src, pid
);

CREATE MACRO report_sources_history_multi() AS TABLE (
    WITH cash_all AS (
        SELECT src, acct, ccy, amt, snap FROM (
            SELECT cb.silver_source_id AS src, cb.account_external_id AS acct, cb.currency AS ccy,
                   cb.amount AS amt, cb.snapshot_at AS snap,
                   ROW_NUMBER() OVER (PARTITION BY cb.silver_source_id, cb.snapshot_at, cb.account_external_id, cb.currency
                       ORDER BY CASE cb.balance_kind WHEN 'current' THEN 1 WHEN 'closing' THEN 2 WHEN 'available' THEN 3
                           WHEN 'aggregated' THEN 4 WHEN 'opening' THEN 5 WHEN 'initial' THEN 6 WHEN 'projected' THEN 7 ELSE 99 END,
                           cb.balance_kind) AS rn
              FROM cash_balances cb)
        WHERE rn = 1),
    acct_src AS (SELECT silver_source_id AS src, account_external_id AS acct FROM accounts),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, a.acct, p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p JOIN acct_src a ON a.src = p.silver_source_id AND a.acct = p.account_external_id
        UNION ALL
        SELECT cc.src, cc.snap, a.acct, cc.ccy, cc.amt, TRUE
          FROM cash_all cc JOIN acct_src a ON a.src = cc.src AND a.acct = cc.acct),
    conv AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.is_cash, bk.base_currency,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'USD', fxl, fxb) END AS DECIMAL(28,4)) AS out_usd,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'CHF', fxl, fxb) END AS DECIMAL(28,4)) AS out_chf,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'EUR', fxl, fxb) END AS DECIMAL(28,4)) AS out_eur,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'GBP', fxl, fxb) END AS DECIMAL(28,4)) AS out_gbp,
               CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
                   CAST(COALESCE(CASE WHEN l.amt = 0 THEN 0::DOUBLE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                       l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate, l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
          FROM lines l
          JOIN source_buckets() bk ON bk.src = l.src
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = l.ccy AND fxl.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.out_gbp, c.base_val
          FROM conv c JOIN hist_active_pos() ap
            ON ap.src = c.src AND ap.acct = c.acct AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.out_gbp, c.base_val
          FROM conv c JOIN hist_active_cash() ac
            ON ac.src = c.src AND ac.acct = c.acct AND ac.ccy = c.ccy AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               COUNT(out_usd) FILTER (WHERE NOT is_cash) AS n_po_u, COUNT(out_usd) FILTER (WHERE is_cash) AS n_co_u,
               CAST(SUM(out_usd) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_u, CAST(SUM(out_usd) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_u,
               COUNT(out_chf) FILTER (WHERE NOT is_cash) AS n_po_c, COUNT(out_chf) FILTER (WHERE is_cash) AS n_co_c,
               CAST(SUM(out_chf) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_c, CAST(SUM(out_chf) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_c,
               COUNT(out_eur) FILTER (WHERE NOT is_cash) AS n_po_e, COUNT(out_eur) FILTER (WHERE is_cash) AS n_co_e,
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e,
               COUNT(out_gbp) FILTER (WHERE NOT is_cash) AS n_po_g, COUNT(out_gbp) FILTER (WHERE is_cash) AS n_co_g,
               CAST(SUM(out_gbp) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_g, CAST(SUM(out_gbp) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_g
          FROM daily GROUP BY 1, 2),
    vals AS (
        SELECT g.day, bk.src, bk.base_currency, bk.tax_wrapper, bk.management_style,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_u=0 THEN NULL ELSE g.pos_u END AS DECIMAL(28,4)) AS pvu,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_u=0 THEN NULL ELSE g.cash_u END AS DECIMAL(28,4)) AS cvu,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_c=0 THEN NULL ELSE g.pos_c END AS DECIMAL(28,4)) AS pvc,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_c=0 THEN NULL ELSE g.cash_c END AS DECIMAL(28,4)) AS cvc,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_e=0 THEN NULL ELSE g.pos_e END AS DECIMAL(28,4)) AS pve,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po_g=0 THEN NULL ELSE g.pos_g END AS DECIMAL(28,4)) AS pvg,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_g=0 THEN NULL ELSE g.cash_g END AS DECIMAL(28,4)) AS cvg
          FROM agg g JOIN source_buckets() bk ON bk.src = g.src)
    SELECT day * 86400 AS as_of_day, src AS silver_source_id, base_currency, tax_wrapper, management_style,
           pvb AS positions_value_base, cvb AS cash_balance_base,
           CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS total_value_base,
           pvu AS positions_value_usd, cvu AS cash_balance_usd,
           CASE WHEN pvu IS NULL OR cvu IS NULL THEN NULL ELSE pvu + cvu END AS total_value_usd,
           pvc AS positions_value_chf, cvc AS cash_balance_chf,
           CASE WHEN pvc IS NULL OR cvc IS NULL THEN NULL ELSE pvc + cvc END AS total_value_chf,
           pve AS positions_value_eur, cve AS cash_balance_eur,
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur,
           pvg AS positions_value_gbp, cvg AS cash_balance_gbp,
           CASE WHEN pvg IS NULL OR cvg IS NULL THEN NULL ELSE pvg + cvg END AS total_value_gbp
      FROM vals
     ORDER BY as_of_day, src
);

-- basis_stamp: a book value's stamp as one token, origin/method/fees.
CREATE MACRO basis_stamp(p_origin, p_method, p_fees) AS
    p_origin || '/' || p_method || '/' || p_fees;

-- holding_key, lot_key: what a position line and a realized lot key on
-- in gains_windows. A line keys on its instrument, else its position
-- key; a lot on its instrument, else its instrument hint, else its
-- description, else '' (a lot that names nothing).
CREATE MACRO holding_key(p_instrument, p_position_key) AS
    COALESCE(p_instrument, p_position_key);

-- instrument_labels: each instrument's symbol (the source's, else the
-- resolved one) and name, keyed as the source keys it.
CREATE MACRO instrument_labels() AS TABLE (
    WITH k AS (
        SELECT silver_source_id, instrument_external_id FROM instruments
        UNION
        SELECT silver_source_id, lookup_value FROM symbol_resolutions WHERE lookup_kind = 'instrument_external_id')
    SELECT k.silver_source_id, k.instrument_external_id, COALESCE(i.symbol, sri.symbol) AS symbol, i.name
      FROM k
      LEFT JOIN instruments i ON i.silver_source_id = k.silver_source_id
           AND i.instrument_external_id = k.instrument_external_id
      LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = k.silver_source_id
           AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = k.instrument_external_id
);

-- ============================================================
-- Shared predicates. The lot pass (internal/gold/lots_keys.go,
-- lots_feed_stated.go) reads through the same macros, so the engine
-- and the readers agree on which holdings a cost basis describes,
-- which basis a source states, and which sales a document covers.
-- ============================================================

-- basis_applies: whether a cost basis describes a holding of this class
-- and vehicle; cash, a mortgage and an FX forward have none (GAINS.md
-- §1).
CREATE MACRO basis_applies(p_class, p_vehicle) AS
    p_class <> 'cash' AND COALESCE(p_vehicle, '') NOT IN ('mortgage', 'forward');

-- basis_missing: a holding a cost basis describes, without one.
CREATE MACRO basis_missing(p_class, p_vehicle, p_book_value) AS
    basis_applies(p_class, p_vehicle) AND p_book_value IS NULL;

-- instrument_aliases: the instrument a source keys by its CUSIP (alias)
-- and the ticker it also keys it by (k), where its instruments carry
-- both rows with one symbol: a statement names a security by CUSIP and
-- the trades and snapshots by ticker. The lot pass reads it too
-- (lotAliases).
CREATE MACRO instrument_aliases() AS TABLE (
    SELECT c.silver_source_id AS src, c.instrument_external_id AS alias, c.symbol AS k
      FROM instruments c
      JOIN instruments t ON t.silver_source_id = c.silver_source_id AND t.instrument_external_id = c.symbol
     WHERE c.cusip = c.instrument_external_id AND c.symbol <> c.instrument_external_id
);

CREATE MACRO holding_aliases() AS TABLE (
    SELECT silver_source_id, account_external_id, from_key, to_key FROM holding_aliases
);

-- ---- the point-in-time line bases, picked per key ----------------------

-- positions_at: the positions held at each instant, each account at its
-- own active snapshot.
CREATE MACRO positions_at(p_instants) AS TABLE (
    WITH pick AS (SELECT instant, src, acct, snap FROM pos_active_at(p_instants))
    SELECT pk.instant, p.silver_source_id, p.snapshot_at, p.account_external_id,
           a.display_name, a.relationship_id, a.nickname, a.account_category,
           p.position_key, p.instrument_external_id, il.symbol, il.name,
           p.asset_class, p.vehicle, p.currency, p.quantity, p.market_value,
           p.book_value, p.accrued_interest, p.acquisition_date,
           p.basis_origin, p.basis_method, p.basis_fees,
           p.book_value_known, p.quantity_without_basis, p.open_lots, p.valued_at
      FROM positions p
      JOIN pick pk ON pk.src = p.silver_source_id AND pk.acct = p.account_external_id
       AND pk.snap = p.snapshot_at
      LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id AND a.account_external_id = p.account_external_id
      LEFT JOIN instrument_labels() il ON il.silver_source_id = p.silver_source_id
           AND il.instrument_external_id = p.instrument_external_id
);

-- gains_position_lines_all: gains_position_lines under every
-- currency in p_ccys and reading in p_readings, which each line names
-- in out_currency and missing_basis.
CREATE MACRO gains_position_lines_all(p_instants, p_ccys, p_readings) AS TABLE (
    WITH a0 AS (
        SELECT p.* REPLACE (COALESCE(ol.n, p.open_lots, 0) AS open_lots),
               basis_applies(p.asset_class, p.vehicle) AS basis_applies,
               p.market_value - COALESCE(p.accrued_interest, 0) AS clean_value,
               basis_stamp(p.basis_origin, p.basis_method, p.basis_fees) AS stamp
          FROM positions_at(p_instants) p
          LEFT JOIN (SELECT silver_source_id, snapshot_at, account_external_id, position_key, COUNT(*) AS n
                       FROM position_lots GROUP BY ALL) ol
            ON ol.silver_source_id = p.silver_source_id AND ol.snapshot_at = p.snapshot_at
           AND ol.account_external_id = p.account_external_id AND ol.position_key = p.position_key),
    -- One rate per currency, day and output currency, not per line.
    rates AS (
        SELECT d.currency, d.day, d.out_currency, fx.rate
          FROM (SELECT DISTINCT a0.currency, a0.snapshot_at // 86400 AS day, c.out_currency
                  FROM a0, (SELECT unnest(p_ccys) AS out_currency) c) d
          ASOF LEFT JOIN fx_rates_into(p_ccys) fx ON fx.to_ccy = d.out_currency AND fx.from_ccy = d.currency
               AND fx.day <= d.day),
    a AS (
        SELECT a0.* REPLACE (
                   CASE WHEN r.missing_basis = 'zero' AND a0.basis_applies
                        THEN COALESCE(a0.book_value, a0.book_value_known, 0) ELSE a0.book_value END AS book_value),
               r.missing_basis = 'zero' AND basis_missing(a0.asset_class, a0.vehicle, a0.book_value) AS assumed_zero,
               CASE WHEN a0.basis_applies AND a0.book_value IS NULL
                    THEN COALESCE(a0.quantity_without_basis, abs(a0.quantity)) END AS missing_quantity,
               r.missing_basis
          FROM a0, (SELECT unnest(p_readings) AS missing_basis) r),
    l AS (
        SELECT a.*, CASE WHEN a.basis_applies THEN a.clean_value - a.book_value END AS unrealized,
               x.out_currency, x.rate AS fx_rate
          FROM a JOIN rates x ON x.currency = a.currency AND x.day = a.snapshot_at // 86400)
    SELECT l.* EXCLUDE (missing_basis, out_currency, fx_rate),
           CASE WHEN l.currency = l.out_currency THEN 1.0 ELSE l.fx_rate END AS rate,
           fx_amount(l.market_value, l.currency, l.out_currency, l.fx_rate) AS value_x,
           fx_amount(l.book_value, l.currency, l.out_currency, l.fx_rate) AS book_x,
           fx_amount(l.unrealized, l.currency, l.out_currency, l.fx_rate) AS unrealized_x,
           l.currency <> l.out_currency AND l.fx_rate IS NULL AS fx_missing,
           l.out_currency, l.missing_basis
      FROM l
);

CREATE MACRO gains_position_lines(p_instants, p_ccy, p_missing := 'ignore') AS TABLE (
    SELECT * EXCLUDE (out_currency, missing_basis)
      FROM gains_position_lines_all(p_instants, [p_ccy], [p_missing])
);

-- report_positions / report_cash: the positions and the cash balances
-- held at p_asof, valued in p_ccy, each ending in valued_at.
CREATE MACRO report_positions(p_asof, p_ccy, p_missing := 'ignore') AS TABLE (
    SELECT silver_source_id, snapshot_at, account_external_id, display_name,
           relationship_id, nickname, account_category, position_key,
           instrument_external_id, symbol, name, asset_class, vehicle, currency,
           CAST(quantity AS VARCHAR) AS quantity, CAST(market_value AS VARCHAR) AS market_value,
           CAST(value_x::DECIMAL(28,4) AS VARCHAR) AS value_outccy,
           CAST(book_value AS VARCHAR) AS book_value,
           CAST(book_x::DECIMAL(28,4) AS VARCHAR) AS book_value_outccy,
           CAST(accrued_interest AS VARCHAR) AS accrued_interest,
           CAST(clean_value AS VARCHAR) AS clean_value,
           CAST(unrealized AS VARCHAR) AS unrealized_gain,
           CAST(unrealized_x::DECIMAL(28,4) AS VARCHAR) AS unrealized_gain_outccy,
           unrealized::DOUBLE / NULLIF(book_value::DOUBLE, 0) AS unrealized_ratio,
           stamp AS basis_stamp,
           CAST(acquisition_date AS VARCHAR) AS acquisition_date,
           CAST(missing_quantity AS VARCHAR) AS quantity_without_basis,
           COALESCE(valued_at, snapshot_at) AS valued_at
      FROM gains_position_lines([p_asof], p_ccy, p_missing := p_missing)
     ORDER BY silver_source_id, account_external_id, position_key
);

-- cash_chosen: each (account, currency) at its own active
-- snapshot, one row by balance_kind precedence (current > closing >
-- available > ...), non-zero.
CREATE MACRO cash_chosen(p_asof) AS TABLE (
    WITH latest AS (SELECT src, acct, ccy, snap FROM cash_active_at([p_asof])),
    ranked AS (
        SELECT cb.silver_source_id, cb.account_external_id, cb.currency,
               cb.amount, cb.snapshot_at,
               ROW_NUMBER() OVER (
                   PARTITION BY cb.silver_source_id, cb.account_external_id, cb.currency
                   ORDER BY CASE cb.balance_kind
                       WHEN 'current' THEN 1 WHEN 'closing' THEN 2
                       WHEN 'available' THEN 3 WHEN 'aggregated' THEN 4
                       WHEN 'opening' THEN 5 WHEN 'initial' THEN 6
                       WHEN 'projected' THEN 7 ELSE 99 END, cb.balance_kind) AS rn
          FROM cash_balances cb
          JOIN latest l ON cb.silver_source_id = l.src AND cb.account_external_id = l.acct
           AND cb.currency = l.ccy AND cb.snapshot_at = l.snap)
    SELECT silver_source_id, account_external_id, currency, amount, snapshot_at
      FROM ranked WHERE rn = 1 AND amount <> 0
);

CREATE MACRO report_cash(p_asof, p_ccy) AS TABLE (
    SELECT cc.silver_source_id, cc.snapshot_at, cc.account_external_id, a.display_name,
           a.relationship_id, a.nickname, a.account_category,
           'cash:' || cc.currency AS position_key, CAST(NULL AS VARCHAR) AS instrument_external_id,
           cc.currency AS symbol, 'Cash ' || cc.currency AS name,
           'cash' AS asset_class, 'demand_deposit' AS vehicle, cc.currency,
           CAST(NULL AS VARCHAR) AS quantity, CAST(cc.amount AS VARCHAR) AS market_value,
           CAST(fx_amount(cc.amount, cc.currency, p_ccy, fx.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy,
           CAST(NULL AS VARCHAR) AS book_value, CAST(NULL AS VARCHAR) AS book_value_outccy,
           CAST(NULL AS VARCHAR) AS accrued_interest, CAST(NULL AS VARCHAR) AS clean_value,
           CAST(NULL AS VARCHAR) AS unrealized_gain, CAST(NULL AS VARCHAR) AS unrealized_gain_outccy,
           CAST(NULL AS DOUBLE) AS unrealized_ratio, CAST(NULL AS VARCHAR) AS basis_stamp,
           CAST(NULL AS VARCHAR) AS acquisition_date, CAST(NULL AS VARCHAR) AS quantity_without_basis,
           cc.snapshot_at AS valued_at
      FROM cash_chosen(p_asof) cc
      LEFT JOIN accounts a ON cc.silver_source_id = a.silver_source_id AND cc.account_external_id = a.account_external_id
      ASOF LEFT JOIN fx_rates_to(p_ccy) fx ON fx.from_ccy = cc.currency AND fx.day <= (cc.snapshot_at // 86400)
     ORDER BY cc.silver_source_id, cc.account_external_id, cc.currency
);

-- account_lines_base: positions and cash at each account's active
-- snapshot, with each line's value in the account's base currency.
CREATE MACRO account_lines_base(p_asof) AS TABLE (
    WITH lp AS (SELECT src, acct, snap FROM pos_active_at([p_asof])),
    lines AS (
        SELECT p.silver_source_id AS src, p.account_external_id AS acct, p.currency AS ccy,
               p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.src AND p.account_external_id = lp.acct
           AND p.snapshot_at = lp.snap
        UNION ALL
        SELECT silver_source_id, account_external_id, currency, amount, snapshot_at, TRUE
          FROM cash_chosen(p_asof))
    SELECT l.src, l.acct, l.ccy, l.amt, l.snap, l.is_cash, a.base_currency,
           CASE WHEN a.base_currency IS NULL THEN NULL ELSE
               CAST(COALESCE(CASE WHEN l.ccy = a.base_currency THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate,
                   l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
      FROM lines l
      LEFT JOIN accounts a ON l.src = a.silver_source_id AND l.acct = a.account_external_id
      ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = a.base_currency AND b0.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = a.base_currency AND b2.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = a.base_currency AND b4.day <= (l.snap // 86400)
);

-- report_accounts: one row per account over account_lines_base (base trio
-- NULL when base unknown; empty account -> 0; lines-but-no-FX -> NULL).
CREATE MACRO report_accounts(p_asof, p_ccy) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.acct, l.base_currency, l.snap, l.is_cash, l.base_val,
               CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate,
                   l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val
          FROM account_lines_base(p_asof) l
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)),
    agg AS (
        SELECT src, acct,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos,
               COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(out_val) FILTER (WHERE NOT is_cash) AS n_po,
               COUNT(out_val) FILTER (WHERE is_cash) AS n_co,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb,
               COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(out_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_o,
               CAST(SUM(out_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_o,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b,
               CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               MAX(snap) AS max_snap
          FROM conv GROUP BY 1, 2),
    ssnap AS (SELECT src, MAX(snap) AS s FROM conv GROUP BY 1),
    vals AS (
        SELECT a.silver_source_id, a.account_external_id, a.account_kind, a.display_name,
               a.base_currency, a.relationship_id, a.nickname, a.account_category,
               a.portfolio_external_id, a.tax_wrapper, a.management_style,
               COALESCE(g.max_snap, ss.s, 0) AS snapshot_at,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po=0 THEN NULL ELSE g.pos_o END AS DECIMAL(28,4)) AS pvo,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co=0 THEN NULL ELSE g.cash_o END AS DECIMAL(28,4)) AS cvo
          FROM accounts a
          LEFT JOIN agg g ON a.silver_source_id = g.src AND a.account_external_id = g.acct
          LEFT JOIN ssnap ss ON a.silver_source_id = ss.src)
    SELECT silver_source_id, account_external_id, account_kind, display_name, base_currency,
           relationship_id, nickname, account_category, portfolio_external_id, tax_wrapper,
           management_style, snapshot_at,
           CAST(pvb AS VARCHAR) AS positions_value_base,
           CAST(cvb AS VARCHAR) AS cash_balance_base,
           CAST(CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS VARCHAR) AS total_value_base,
           CAST(pvo AS VARCHAR) AS positions_value_outccy,
           CAST(cvo AS VARCHAR) AS cash_balance_outccy,
           CAST(CASE WHEN pvo IS NULL OR cvo IS NULL THEN NULL ELSE pvo + cvo END AS VARCHAR) AS total_value_outccy
      FROM vals
     ORDER BY silver_source_id, account_external_id
);

-- report_global: Σ over report_accounts (guarantees global == Σ accounts).
CREATE MACRO report_global(p_asof, p_ccy) AS TABLE (
    SELECT COALESCE(MIN(snapshot_at) FILTER (WHERE snapshot_at > 0), 0) AS min_snapshot_at,
           COALESCE(MAX(snapshot_at) FILTER (WHERE snapshot_at > 0), 0) AS max_snapshot_at,
           CAST(COALESCE(SUM(cash_balance_outccy::DECIMAL(28,4)), 0) AS VARCHAR) AS cash_balance_outccy,
           CAST(COALESCE(SUM(positions_value_outccy::DECIMAL(28,4)), 0) AS VARCHAR) AS positions_value_outccy,
           CAST(COALESCE(SUM(total_value_outccy::DECIMAL(28,4)), 0) AS VARCHAR) AS total_value_outccy
      FROM report_accounts(p_asof, p_ccy)
);

CREATE MACRO report_accounts_multi(p_asof) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.acct, l.base_currency, l.snap, l.is_cash, l.base_val,
               CAST(fx_reporting_value(l.amt, l.ccy, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS out_usd,
               CAST(fx_reporting_value(l.amt, l.ccy, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS out_chf,
               CAST(fx_reporting_value(l.amt, l.ccy, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS out_eur,
               CAST(fx_reporting_value(l.amt, l.ccy, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS out_gbp
          FROM account_lines_base(p_asof) l
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = l.ccy AND fxl.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (l.snap // 86400)),
    agg AS (
        SELECT src, acct,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               COUNT(out_usd) FILTER (WHERE NOT is_cash) AS n_po_u, COUNT(out_usd) FILTER (WHERE is_cash) AS n_co_u,
               CAST(SUM(out_usd) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_u, CAST(SUM(out_usd) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_u,
               COUNT(out_chf) FILTER (WHERE NOT is_cash) AS n_po_c, COUNT(out_chf) FILTER (WHERE is_cash) AS n_co_c,
               CAST(SUM(out_chf) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_c, CAST(SUM(out_chf) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_c,
               COUNT(out_eur) FILTER (WHERE NOT is_cash) AS n_po_e, COUNT(out_eur) FILTER (WHERE is_cash) AS n_co_e,
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e,
               COUNT(out_gbp) FILTER (WHERE NOT is_cash) AS n_po_g, COUNT(out_gbp) FILTER (WHERE is_cash) AS n_co_g,
               CAST(SUM(out_gbp) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_g, CAST(SUM(out_gbp) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_g,
               MAX(snap) AS max_snap
          FROM conv GROUP BY 1, 2),
    ssnap AS (SELECT src, MAX(snap) AS s FROM conv GROUP BY 1),
    vals AS (
        SELECT a.silver_source_id, a.account_external_id, a.account_kind, a.display_name,
               a.base_currency, a.relationship_id, a.nickname, a.account_category,
               a.portfolio_external_id,
               -- account-level display defaults (every account is non-NULL, matching
               -- the `wealthdb holdings accounts` CLI render); the raw accounts table keeps NULL.
               COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
               COALESCE(a.management_style, 'self_directed') AS management_style,
               COALESCE(g.max_snap, ss.s, 0) AS snapshot_at,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN a.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_u=0 THEN NULL ELSE g.pos_u END AS DECIMAL(28,4)) AS pvu,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_u=0 THEN NULL ELSE g.cash_u END AS DECIMAL(28,4)) AS cvu,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_c=0 THEN NULL ELSE g.pos_c END AS DECIMAL(28,4)) AS pvc,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_c=0 THEN NULL ELSE g.cash_c END AS DECIMAL(28,4)) AS cvc,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_e=0 THEN NULL ELSE g.pos_e END AS DECIMAL(28,4)) AS pve,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_g=0 THEN NULL ELSE g.pos_g END AS DECIMAL(28,4)) AS pvg,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_g=0 THEN NULL ELSE g.cash_g END AS DECIMAL(28,4)) AS cvg
          FROM accounts a
          LEFT JOIN agg g ON a.silver_source_id = g.src AND a.account_external_id = g.acct
          LEFT JOIN ssnap ss ON a.silver_source_id = ss.src)
    SELECT silver_source_id, account_external_id, account_kind, display_name, base_currency,
           relationship_id, nickname, account_category, portfolio_external_id, tax_wrapper,
           management_style, snapshot_at,
           pvb AS positions_value_base, cvb AS cash_balance_base,
           CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS total_value_base,
           pvu AS positions_value_usd, cvu AS cash_balance_usd,
           CASE WHEN pvu IS NULL OR cvu IS NULL THEN NULL ELSE pvu + cvu END AS total_value_usd,
           pvc AS positions_value_chf, cvc AS cash_balance_chf,
           CASE WHEN pvc IS NULL OR cvc IS NULL THEN NULL ELSE pvc + cvc END AS total_value_chf,
           pve AS positions_value_eur, cve AS cash_balance_eur,
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur,
           pvg AS positions_value_gbp, cvg AS cash_balance_gbp,
           CASE WHEN pvg IS NULL OR cvg IS NULL THEN NULL ELSE pvg + cvg END AS total_value_gbp
      FROM vals
     ORDER BY silver_source_id, account_external_id
);

CREATE MACRO report_global_multi(p_asof) AS TABLE (
    SELECT COALESCE(MIN(snapshot_at) FILTER (WHERE snapshot_at > 0), 0) AS min_snapshot_at,
           COALESCE(MAX(snapshot_at) FILTER (WHERE snapshot_at > 0), 0) AS max_snapshot_at,
           CAST(COALESCE(SUM(cash_balance_usd), 0) AS DECIMAL(28,4)) AS cash_balance_usd,
           CAST(COALESCE(SUM(positions_value_usd), 0) AS DECIMAL(28,4)) AS positions_value_usd,
           CAST(COALESCE(SUM(total_value_usd), 0) AS DECIMAL(28,4)) AS total_value_usd,
           CAST(COALESCE(SUM(cash_balance_chf), 0) AS DECIMAL(28,4)) AS cash_balance_chf,
           CAST(COALESCE(SUM(positions_value_chf), 0) AS DECIMAL(28,4)) AS positions_value_chf,
           CAST(COALESCE(SUM(total_value_chf), 0) AS DECIMAL(28,4)) AS total_value_chf,
           CAST(COALESCE(SUM(cash_balance_eur), 0) AS DECIMAL(28,4)) AS cash_balance_eur,
           CAST(COALESCE(SUM(positions_value_eur), 0) AS DECIMAL(28,4)) AS positions_value_eur,
           CAST(COALESCE(SUM(total_value_eur), 0) AS DECIMAL(28,4)) AS total_value_eur,
           CAST(COALESCE(SUM(cash_balance_gbp), 0) AS DECIMAL(28,4)) AS cash_balance_gbp,
           CAST(COALESCE(SUM(positions_value_gbp), 0) AS DECIMAL(28,4)) AS positions_value_gbp,
           CAST(COALESCE(SUM(total_value_gbp), 0) AS DECIMAL(28,4)) AS total_value_gbp
      FROM report_accounts_multi(p_asof)
);

-- portfolio_lines_base: positions and cash at each account's active
-- snapshot, by portfolio bucket, in the bucket's base currency.
CREATE MACRO portfolio_lines_base(p_asof) AS TABLE (
    WITH lp AS (SELECT src, acct, snap FROM pos_active_at([p_asof])),
    acct_port AS (SELECT * FROM portfolio_acct_map()),
    lines AS (
        SELECT p.silver_source_id AS src, ap.pid, p.currency AS ccy, p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.src AND p.account_external_id = lp.acct
           AND p.snapshot_at = lp.snap
          JOIN acct_port ap ON ap.src = p.silver_source_id AND ap.acct = p.account_external_id
        UNION ALL
        SELECT cc.silver_source_id, ap.pid, cc.currency, cc.amount, cc.snapshot_at, TRUE
          FROM cash_chosen(p_asof) cc JOIN acct_port ap ON ap.src = cc.silver_source_id AND ap.acct = cc.account_external_id)
    SELECT l.src, l.pid, l.ccy, l.amt, l.snap, l.is_cash, bk.base_currency,
           CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
               CAST(COALESCE(CASE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate,
                   l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
      FROM lines l
      JOIN portfolio_buckets() bk ON bk.src = l.src AND bk.pid = l.pid
      ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)
);

-- report_portfolios: one row per portfolio bucket, over portfolio_buckets
-- and portfolio_lines_base.
CREATE MACRO report_portfolios(p_asof, p_ccy) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.pid, l.snap, l.is_cash, l.base_currency, l.base_val,
               CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate,
                   l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val
          FROM portfolio_lines_base(p_asof) l
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)),
    agg AS (
        SELECT src, pid,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(out_val) FILTER (WHERE NOT is_cash) AS n_po, COUNT(out_val) FILTER (WHERE is_cash) AS n_co,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(out_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_o, CAST(SUM(out_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_o,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               MAX(snap) AS max_snap
          FROM conv GROUP BY 1, 2),
    ssnap AS (SELECT src, MAX(snap) AS s FROM conv GROUP BY 1),
    vals AS (
        SELECT bk.src, bk.pid, bk.display_name, bk.base_currency, bk.relationship_id, bk.nickname,
               bk.tax_wrapper, bk.management_style,
               COALESCE(g.max_snap, ss.s, 0) AS snapshot_at,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po=0 THEN NULL ELSE g.pos_o END AS DECIMAL(28,4)) AS pvo,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co=0 THEN NULL ELSE g.cash_o END AS DECIMAL(28,4)) AS cvo
          FROM portfolio_buckets() bk
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

CREATE MACRO report_portfolios_multi(p_asof) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.pid, l.snap, l.is_cash, l.base_currency, l.base_val,
               CAST(fx_reporting_value(l.amt, l.ccy, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS out_usd,
               CAST(fx_reporting_value(l.amt, l.ccy, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS out_chf,
               CAST(fx_reporting_value(l.amt, l.ccy, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS out_eur,
               CAST(fx_reporting_value(l.amt, l.ccy, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS out_gbp
          FROM portfolio_lines_base(p_asof) l
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = l.ccy AND fxl.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (l.snap // 86400)),
    agg AS (
        SELECT src, pid,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               COUNT(out_usd) FILTER (WHERE NOT is_cash) AS n_po_u, COUNT(out_usd) FILTER (WHERE is_cash) AS n_co_u,
               CAST(SUM(out_usd) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_u, CAST(SUM(out_usd) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_u,
               COUNT(out_chf) FILTER (WHERE NOT is_cash) AS n_po_c, COUNT(out_chf) FILTER (WHERE is_cash) AS n_co_c,
               CAST(SUM(out_chf) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_c, CAST(SUM(out_chf) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_c,
               COUNT(out_eur) FILTER (WHERE NOT is_cash) AS n_po_e, COUNT(out_eur) FILTER (WHERE is_cash) AS n_co_e,
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e,
               COUNT(out_gbp) FILTER (WHERE NOT is_cash) AS n_po_g, COUNT(out_gbp) FILTER (WHERE is_cash) AS n_co_g,
               CAST(SUM(out_gbp) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_g, CAST(SUM(out_gbp) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_g,
               MAX(snap) AS max_snap
          FROM conv GROUP BY 1, 2),
    ssnap AS (SELECT src, MAX(snap) AS s FROM conv GROUP BY 1),
    vals AS (
        SELECT bk.src, bk.pid, bk.display_name, bk.base_currency, bk.relationship_id, bk.nickname,
               bk.tax_wrapper, bk.management_style,
               COALESCE(g.max_snap, ss.s, 0) AS snapshot_at,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_u=0 THEN NULL ELSE g.pos_u END AS DECIMAL(28,4)) AS pvu,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_u=0 THEN NULL ELSE g.cash_u END AS DECIMAL(28,4)) AS cvu,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_c=0 THEN NULL ELSE g.pos_c END AS DECIMAL(28,4)) AS pvc,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_c=0 THEN NULL ELSE g.cash_c END AS DECIMAL(28,4)) AS cvc,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_e=0 THEN NULL ELSE g.pos_e END AS DECIMAL(28,4)) AS pve,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_g=0 THEN NULL ELSE g.pos_g END AS DECIMAL(28,4)) AS pvg,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_g=0 THEN NULL ELSE g.cash_g END AS DECIMAL(28,4)) AS cvg
          FROM portfolio_buckets() bk
          LEFT JOIN agg g ON bk.src = g.src AND bk.pid = g.pid
          LEFT JOIN ssnap ss ON bk.src = ss.src)
    SELECT src AS silver_source_id, pid AS portfolio_external_id, display_name, base_currency,
           relationship_id, nickname, tax_wrapper, management_style, snapshot_at,
           pvb AS positions_value_base, cvb AS cash_balance_base,
           CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS total_value_base,
           pvu AS positions_value_usd, cvu AS cash_balance_usd,
           CASE WHEN pvu IS NULL OR cvu IS NULL THEN NULL ELSE pvu + cvu END AS total_value_usd,
           pvc AS positions_value_chf, cvc AS cash_balance_chf,
           CASE WHEN pvc IS NULL OR cvc IS NULL THEN NULL ELSE pvc + cvc END AS total_value_chf,
           pve AS positions_value_eur, cve AS cash_balance_eur,
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur,
           pvg AS positions_value_gbp, cvg AS cash_balance_gbp,
           CASE WHEN pvg IS NULL OR cvg IS NULL THEN NULL ELSE pvg + cvg END AS total_value_gbp
      FROM vals
     ORDER BY src, pid
);

-- source_lines_base: positions and cash at each account's active
-- snapshot, by source, in the source's base currency.
CREATE MACRO source_lines_base(p_asof) AS TABLE (
    WITH lp AS (SELECT src, acct, snap FROM pos_active_at([p_asof])),
    lines AS (
        SELECT p.silver_source_id AS src, p.account_external_id AS acct, p.currency AS ccy, p.market_value AS amt, p.snapshot_at AS snap,
               COALESCE(p.valued_at, p.snapshot_at) AS valued, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.src AND p.account_external_id = lp.acct
           AND p.snapshot_at = lp.snap
        UNION ALL
        SELECT silver_source_id, account_external_id, currency, amount, snapshot_at, snapshot_at, TRUE
          FROM cash_chosen(p_asof))
    SELECT l.src, l.ccy, l.amt, l.snap, l.valued, l.is_cash, bk.base_currency,
           CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
               CAST(COALESCE(CASE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate,
                   l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
      FROM lines l
      JOIN accounts a ON a.silver_source_id = l.src AND a.account_external_id = l.acct
      JOIN source_buckets() bk ON bk.src = l.src
      ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
      ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)
);

-- report_sources / report_sources_multi: one row per source, with the
-- latest instant its lines were valued at as valued_at.
CREATE MACRO report_sources(p_asof, p_ccy) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.valued, l.is_cash, l.base_currency, l.base_val,
               CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate,
                   l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val
          FROM source_lines_base(p_asof) l
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)),
    agg AS (
        SELECT src,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(out_val) FILTER (WHERE NOT is_cash) AS n_po, COUNT(out_val) FILTER (WHERE is_cash) AS n_co,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(out_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_o, CAST(SUM(out_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_o,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               MAX(snap) AS max_snap, MAX(valued) AS max_valued
          FROM conv GROUP BY 1),
    ssnap AS (SELECT src, MAX(snap) AS s FROM conv GROUP BY 1),
    vals AS (
        SELECT bk.src, bk.base_currency, bk.tax_wrapper, bk.management_style,
               COALESCE(g.max_snap, ss.s, 0) AS snapshot_at,
               COALESCE(g.max_valued, ss.s, 0) AS valued_at,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po=0 THEN NULL ELSE g.pos_o END AS DECIMAL(28,4)) AS pvo,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co=0 THEN NULL ELSE g.cash_o END AS DECIMAL(28,4)) AS cvo
          FROM source_buckets() bk
          LEFT JOIN agg g ON bk.src = g.src
          LEFT JOIN ssnap ss ON bk.src = ss.src)
    SELECT src AS silver_source_id, base_currency, tax_wrapper, management_style, snapshot_at, valued_at,
           CAST(pvb AS VARCHAR) AS positions_value_base,
           CAST(cvb AS VARCHAR) AS cash_balance_base,
           CAST(CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS VARCHAR) AS total_value_base,
           CAST(pvo AS VARCHAR) AS positions_value_outccy,
           CAST(cvo AS VARCHAR) AS cash_balance_outccy,
           CAST(CASE WHEN pvo IS NULL OR cvo IS NULL THEN NULL ELSE pvo + cvo END AS VARCHAR) AS total_value_outccy
      FROM vals
     ORDER BY src
);

CREATE MACRO report_sources_multi(p_asof) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.valued, l.is_cash, l.base_currency, l.base_val,
               CAST(fx_reporting_value(l.amt, l.ccy, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS out_usd,
               CAST(fx_reporting_value(l.amt, l.ccy, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS out_chf,
               CAST(fx_reporting_value(l.amt, l.ccy, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS out_eur,
               CAST(fx_reporting_value(l.amt, l.ccy, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS out_gbp
          FROM source_lines_base(p_asof) l
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = l.ccy AND fxl.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (l.snap // 86400)),
    agg AS (
        SELECT src,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b,
               COUNT(out_usd) FILTER (WHERE NOT is_cash) AS n_po_u, COUNT(out_usd) FILTER (WHERE is_cash) AS n_co_u,
               CAST(SUM(out_usd) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_u, CAST(SUM(out_usd) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_u,
               COUNT(out_chf) FILTER (WHERE NOT is_cash) AS n_po_c, COUNT(out_chf) FILTER (WHERE is_cash) AS n_co_c,
               CAST(SUM(out_chf) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_c, CAST(SUM(out_chf) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_c,
               COUNT(out_eur) FILTER (WHERE NOT is_cash) AS n_po_e, COUNT(out_eur) FILTER (WHERE is_cash) AS n_co_e,
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e,
               COUNT(out_gbp) FILTER (WHERE NOT is_cash) AS n_po_g, COUNT(out_gbp) FILTER (WHERE is_cash) AS n_co_g,
               CAST(SUM(out_gbp) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_g, CAST(SUM(out_gbp) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_g,
               MAX(snap) AS max_snap, MAX(valued) AS max_valued
          FROM conv GROUP BY 1),
    ssnap AS (SELECT src, MAX(snap) AS s FROM conv GROUP BY 1),
    vals AS (
        SELECT bk.src, bk.base_currency, bk.tax_wrapper, bk.management_style,
               COALESCE(g.max_snap, ss.s, 0) AS snapshot_at,
               COALESCE(g.max_valued, ss.s, 0) AS valued_at,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_u=0 THEN NULL ELSE g.pos_u END AS DECIMAL(28,4)) AS pvu,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_u=0 THEN NULL ELSE g.cash_u END AS DECIMAL(28,4)) AS cvu,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_c=0 THEN NULL ELSE g.pos_c END AS DECIMAL(28,4)) AS pvc,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_c=0 THEN NULL ELSE g.cash_c END AS DECIMAL(28,4)) AS cvc,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_e=0 THEN NULL ELSE g.pos_e END AS DECIMAL(28,4)) AS pve,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_g=0 THEN NULL ELSE g.pos_g END AS DECIMAL(28,4)) AS pvg,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_g=0 THEN NULL ELSE g.cash_g END AS DECIMAL(28,4)) AS cvg
          FROM source_buckets() bk
          LEFT JOIN agg g ON bk.src = g.src
          LEFT JOIN ssnap ss ON bk.src = ss.src)
    SELECT src AS silver_source_id, base_currency, tax_wrapper, management_style, snapshot_at, valued_at,
           pvb AS positions_value_base, cvb AS cash_balance_base,
           CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS total_value_base,
           pvu AS positions_value_usd, cvu AS cash_balance_usd,
           CASE WHEN pvu IS NULL OR cvu IS NULL THEN NULL ELSE pvu + cvu END AS total_value_usd,
           pvc AS positions_value_chf, cvc AS cash_balance_chf,
           CASE WHEN pvc IS NULL OR cvc IS NULL THEN NULL ELSE pvc + cvc END AS total_value_chf,
           pve AS positions_value_eur, cve AS cash_balance_eur,
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur,
           pvg AS positions_value_gbp, cvg AS cash_balance_gbp,
           CASE WHEN pvg IS NULL OR cvg IS NULL THEN NULL ELSE pvg + cvg END AS total_value_gbp
      FROM vals
     ORDER BY src
);

-- report_positions_multi: the positions in every reporting currency,
-- each account at its own active
-- snapshot.
CREATE MACRO report_positions_multi(p_asof) AS TABLE (
    WITH latest AS (SELECT src, acct, snap FROM pos_active_at([p_asof])),
    base AS (
        SELECT p.silver_source_id, p.snapshot_at, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name,
               p.asset_class, p.vehicle, p.currency, p.quantity, p.market_value
          FROM positions p
          JOIN latest l ON p.silver_source_id = l.src AND p.account_external_id = l.acct
           AND p.snapshot_at = l.snap
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id)
    SELECT b.silver_source_id, b.snapshot_at, b.account_external_id, b.display_name,
           b.relationship_id, b.nickname, b.account_category, b.position_key,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.vehicle, b.currency,
           CAST(b.quantity AS DECIMAL(28,8)) AS quantity, CAST(b.market_value AS DECIMAL(28,4)) AS market_value,
           CAST(fx_reporting_value(b.market_value, b.currency, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS value_usd,
           CAST(fx_reporting_value(b.market_value, b.currency, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS value_chf,
           CAST(fx_reporting_value(b.market_value, b.currency, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS value_eur,
           CAST(fx_reporting_value(b.market_value, b.currency, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS value_gbp
      FROM base b
      ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = b.currency AND fxl.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (b.snapshot_at // 86400)
     ORDER BY b.silver_source_id, b.account_external_id, b.position_key
);

CREATE MACRO account_wrapper_spans() AS TABLE (
    SELECT a.silver_source_id, a.account_external_id, h.tax_wrapper,
           CAST(-9223372036854775808 AS BIGINT) AS from_at, h.until_at
      FROM accounts a
      JOIN account_wrapper_history h
        ON h.silver_source_id    = a.silver_source_id
       AND h.account_external_id = a.account_external_id
    UNION ALL
    SELECT a.silver_source_id, a.account_external_id, a.tax_wrapper,
           COALESCE(h.until_at, CAST(-9223372036854775808 AS BIGINT)) AS from_at,
           CAST(9223372036854775807 AS BIGINT) AS until_at
      FROM accounts a
      LEFT JOIN account_wrapper_history h
             ON h.silver_source_id    = a.silver_source_id
            AND h.account_external_id = a.account_external_id
);

-- ============================================================
-- Each account's cash per currency in the serving history, so cash is
-- a position with its own currency on every Allocation card
-- (web/DESIGN.md §6 and §8).
--
-- report_accounts_history_multi sums an account's cash balances into
-- one figure per day, across their currencies. report_cash_history_multi
-- keeps one row per day, account and currency instead. It reads the
-- same lines, the same day grid and the same balance_kind preference,
-- and converts each balance the same way, so an account's rows sum to
-- the accounts history's cash columns on every day, to the cent.
-- serving_cash_history holds its rows, and holdings_lines_history's
-- cash arm reads it: one cash line per account and currency, with its
-- currency. holdings_lines_latest keeps an account's summed cash.
--
-- web_positions_history lists each cash line as a position named
-- `Cash <CCY>`, one per account: `line_key` names the source and account
-- of a cash line and stays NULL for a position, which keeps its grouping
-- across accounts. web_currencies_history sums every line by its
-- currency and leaves out a line with no currency or no value in any
-- reporting currency (an open FX forward reads `XXX` and no value).
-- ============================================================

CREATE MACRO report_cash_history_multi() AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.amt, l.base_val,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'USD', fxl, fxb) END AS DECIMAL(28,4)) AS out_usd,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'CHF', fxl, fxb) END AS DECIMAL(28,4)) AS out_chf,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'EUR', fxl, fxb) END AS DECIMAL(28,4)) AS out_eur,
               CAST(CASE WHEN l.amt = 0 THEN 0::DOUBLE ELSE fx_reporting_value(l.amt, l.ccy, 'GBP', fxl, fxb) END AS DECIMAL(28,4)) AS out_gbp
          FROM (SELECT * FROM hist_acct_lines_base() WHERE is_cash) l
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = l.ccy AND fxl.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (l.snap // 86400))
    SELECT ac.day * 86400 AS as_of_day, a.silver_source_id, a.account_external_id,
           a.base_currency,
           -- account-level display defaults (see report_accounts_multi).
           COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
           COALESCE(a.management_style, 'self_directed') AS management_style,
           c.ccy AS currency, c.amt AS balance, c.base_val AS balance_base,
           c.out_usd AS balance_usd, c.out_chf AS balance_chf,
           c.out_eur AS balance_eur, c.out_gbp AS balance_gbp
      FROM conv c
      JOIN hist_active_cash() ac
        ON ac.src = c.src AND ac.acct = c.acct AND ac.ccy = c.ccy AND ac.snap = c.snap
      JOIN accounts a ON a.silver_source_id = c.src AND a.account_external_id = c.acct
     ORDER BY as_of_day, silver_source_id, account_external_id, currency
);

-- ============================================================
-- The net worth bridge, fees by portfolio and mark age
-- (docs/RETURNS-NOTES.md, "The net worth bridge"; docs/SPENDING.md §7;
-- docs/DESIGN.md §4.7).
--
--   * report_debt_history / report_debt_fx: each mortgage and card
--     balance in its own currency, by day, and the rate of each such
--     currency. The bridge measures the fall in debt with them, so an
--     FX move on a debt never reads as saving.
--   * report_bridge: derived, like report_returns. `web refresh` writes
--     one row per reporting currency and month. web_bridge serves it.
--   * web_fees: the fee lines, the accounts' daily values and the
--     monthly returns of the entities that hold them. A view over the
--     serving tables and report_returns, with no storage of its own.
--   * report_marks: each month end's share of value marked within five
--     days. serving_marks holds it per reporting currency, web_marks
--     serves it.
-- ============================================================

-- report_debt_history: one row per day, liability account and currency:
-- the balance as the daily history holds it, negative when owed. It
-- reads the lines and the active snapshots report_accounts_history
-- reads, so the two carry and end an account alike. A mortgage is a
-- position and a card is cash, and this sums both.
CREATE MACRO report_debt_history() AS TABLE (
    WITH debt AS (
        SELECT silver_source_id AS src, account_external_id AS acct
          FROM accounts WHERE account_kind IN ('mortgage', 'card')),
    lines AS (
        SELECT l.src, l.snap, l.acct, l.ccy, l.amt, l.is_cash
          FROM hist_acct_lines_base() l
          SEMI JOIN debt d ON d.src = l.src AND d.acct = l.acct),
    daily AS (
        SELECT ap.day, l.src, l.acct, l.ccy, l.amt
          FROM lines l JOIN hist_active_pos() ap
            ON ap.src = l.src AND ap.acct = l.acct AND ap.snap = l.snap WHERE NOT l.is_cash
        UNION ALL
        SELECT ac.day, l.src, l.acct, l.ccy, l.amt
          FROM lines l JOIN hist_active_cash() ac
            ON ac.src = l.src AND ac.acct = l.acct AND ac.ccy = l.ccy AND ac.snap = l.snap WHERE l.is_cash)
    SELECT day, src AS silver_source_id, acct AS account_external_id, ccy AS currency,
           SUM(amt) AS balance
      FROM daily
     GROUP BY 1, 2, 3, 4
     ORDER BY 2, 3, 4, 1
);

-- report_debt_fx: each day's rate from every currency a liability is
-- held in to p_ccy, nearest at or before the day, direct or through CHF
-- or USD as the history macros reach a rate. A day with no path has a
-- NULL rate.
CREATE MACRO report_debt_fx(p_ccy) AS TABLE (
    WITH ccys AS (
        SELECT DISTINCT p.currency AS ccy
          FROM positions p JOIN accounts a USING (silver_source_id, account_external_id)
         WHERE a.account_kind IN ('mortgage', 'card')
        UNION
        SELECT DISTINCT c.currency
          FROM cash_balances c JOIN accounts a USING (silver_source_id, account_external_id)
         WHERE a.account_kind IN ('mortgage', 'card')),
    grid AS (SELECT s.day, c.ccy FROM hist_days() s CROSS JOIN ccys c)
    SELECT g.day, g.ccy AS currency,
           CASE WHEN g.ccy = p_ccy THEN 1::DOUBLE
                ELSE COALESCE(o0.rate, o1.rate * o2.rate, o3.rate * o4.rate) END AS rate
      FROM grid g
      ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = g.ccy AND o0.to_ccy = p_ccy AND o0.day <= g.day
      ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = g.ccy AND o1.to_ccy = 'CHF' AND o1.day <= g.day
      ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= g.day
      ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = g.ccy AND o3.to_ccy = 'USD' AND o3.day <= g.day
      ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= g.day
     ORDER BY 2, 1
);

-- mark_month_ends: each month's last day inside gold's history.
CREATE MACRO mark_month_ends() AS TABLE (
    SELECT day FROM hist_days()
     WHERE DATE '1970-01-01' + CAST(day AS INTEGER) = last_day(DATE '1970-01-01' + CAST(day AS INTEGER))
);

-- ============================================================
-- Mark age sums in any order (docs/DESIGN.md §4.7).
--
--   * report_marks: each line's value is a decimal before the sums, in
--     USD and in the output currency. A float sum rounds in the order
--     it adds, and DuckDB adds in parallel in no fixed order, so a
--     float total could move in its last digit between runs. A decimal
--     sum is exact, so the order does not matter. Each line rounds
--     once, to 1e-10.
-- ============================================================

-- report_marks: per month end, the value of the positions on the daily
-- history that day, and the part marked within five days. A line's age
-- is the month end less coalesce(valued_at, snapshot_at), in days.
-- Liabilities and cash are out, and so is a line of no value. A line is
-- valued as the daily history values it, at its snapshot's rate.
-- marked_pct weighs by USD value whatever p_ccy is, so the figure has
-- one definition. A month end with no line has a NULL marked_pct and
-- says so in quality, and a line with no rate to USD or p_ccy leaves the
-- sums and is counted there.
CREATE MACRO report_marks(p_ccy) AS TABLE (
    WITH lines AS (
        SELECT p.instant // 86400 AS day, p.snapshot_at, p.valued_at, p.currency, p.market_value
          FROM positions_at((SELECT list(day * 86400 + 86399) FROM mark_month_ends())) p
          JOIN accounts a ON a.silver_source_id = p.silver_source_id
           AND a.account_external_id = p.account_external_id
         WHERE p.market_value > 0 AND p.asset_class <> 'cash'
           AND a.account_kind NOT IN ('mortgage', 'card')),
    valued AS (
        SELECT l.day,
               l.day - COALESCE(l.valued_at, l.snapshot_at) // 86400 <= 5 AS marked,
               CAST(fx_reporting_value(l.market_value, l.currency, 'USD', fxl, fxb) AS DECIMAL(38,10)) AS usd,
               CAST(COALESCE(CASE WHEN l.currency = p_ccy THEN l.market_value::DOUBLE END,
                             l.market_value::DOUBLE * o0.rate, l.market_value::DOUBLE * o1.rate * o2.rate,
                             l.market_value::DOUBLE * o3.rate * o4.rate) AS DECIMAL(38,10)) AS out_val
          FROM lines l
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = l.currency AND fxl.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.currency AND o0.to_ccy = p_ccy AND o0.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.currency AND o1.to_ccy = 'CHF' AND o1.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.currency AND o3.to_ccy = 'USD' AND o3.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snapshot_at // 86400))
    SELECT e.day * 86400 AS month_end,
           CAST(100 * COALESCE(SUM(v.usd) FILTER (WHERE v.marked), 0) / NULLIF(SUM(v.usd), 0) AS DOUBLE) AS marked_pct,
           CAST(COALESCE(SUM(v.out_val) FILTER (WHERE v.marked), 0) AS DECIMAL(28,4)) AS value_marked,
           CAST(COALESCE(SUM(v.out_val), 0) AS DECIMAL(28,4)) AS value_total,
           CASE WHEN COUNT(v.day) = 0 THEN 'no_positions'
                WHEN COUNT(*) FILTER (WHERE v.usd IS NULL OR v.out_val IS NULL) > 0
                    THEN 'unpriced_lines=' || COUNT(*) FILTER (WHERE v.usd IS NULL OR v.out_val IS NULL)
                ELSE '' END AS quality
      FROM mark_month_ends() e
      LEFT JOIN valued v ON v.day = e.day
     GROUP BY 1
     ORDER BY 1
);

-- report_marks_multi: report_marks in every reporting currency.
CREATE MACRO report_marks_multi() AS TABLE (
    SELECT 'USD' AS currency, * FROM report_marks('USD')
    UNION ALL SELECT 'CHF', * FROM report_marks('CHF')
    UNION ALL SELECT 'EUR', * FROM report_marks('EUR')
    UNION ALL SELECT 'GBP', * FROM report_marks('GBP')
);
