-- Holdings: the account buckets, the lines held at an instant or on
-- each day, valued, and the holdings reports over them.

-- ===========================================================================
-- ACCOUNTS: the wrapper and style an account reads
-- ===========================================================================

-- account_wrapper / account_style: an account's tax wrapper and
-- management style as every report reads them.
--
--   * `accounts` keeps NULL where no adapter and no override classified
--     an account, so gold still tells it from one stated as the default.
--   * account_wrapper reads that NULL as taxable_personal,
--     account_style as self_directed.
--
-- Scalar macros, so a statement keeps its plan. They also cover a line
-- whose account gold does not hold: a LEFT JOIN `accounts` that finds
-- no row reads the default too.
CREATE MACRO account_wrapper(w) AS COALESCE(w, 'taxable_personal');

CREATE MACRO account_style(s) AS COALESCE(s, 'self_directed');

-- account_is_debt: an account of the kind is a debt
-- (account_kinds.liability). A scalar macro, so it expands in place to
-- the IN-list it holds, and a test holds it to the stamped roles
-- (TestFlowPredicatesHoldTheRoles).
CREATE MACRO account_is_debt(p_kind) AS (
    p_kind IN ('mortgage', 'card')
);

-- ===========================================================================
-- FX: one line's value in another currency
-- ===========================================================================

-- fx_pick: `p_amt` in currency `p_ccy`, valued in `p_tgt`, as a DOUBLE.
--
--   * The first path that has a rate wins: the same currency, the direct
--     rate `p_d`, then via CHF (`p_c1` * `p_c2`), then via USD
--     (`p_u1` * `p_u2`). With no path it reads NULL.
--   * The rates are the `rate` columns of the five ASOF joins on
--     `fx_daily` that each caller keeps. The products run left to right,
--     so a value is the same to the bit at every caller.
--
-- A scalar macro, so it expands in place and a statement keeps its plan.
CREATE MACRO fx_pick(p_amt, p_ccy, p_tgt, p_d, p_c1, p_c2, p_u1, p_u2) AS
    COALESCE(CASE WHEN p_ccy = p_tgt THEN p_amt::DOUBLE END,
        p_amt::DOUBLE * p_d, p_amt::DOUBLE * p_c1 * p_c2,
        p_amt::DOUBLE * p_u1 * p_u2);

-- fx_pick_keep_zero: fx_pick, except that a zero amount reads zero in
-- every currency, with or without a rate. The zero arm sits in the same
-- COALESCE as the paths: a second COALESCE around fx_pick costs a CASE
-- per row on the history reads, and a CASE around the call hides the
-- repeated cast from the planner's common-subexpression pass.
CREATE MACRO fx_pick_keep_zero(p_amt, p_ccy, p_tgt, p_d, p_c1, p_c2, p_u1, p_u2) AS
    COALESCE(CASE WHEN p_amt = 0 THEN 0::DOUBLE WHEN p_ccy = p_tgt THEN p_amt::DOUBLE END,
        p_amt::DOUBLE * p_d, p_amt::DOUBLE * p_c1 * p_c2,
        p_amt::DOUBLE * p_u1 * p_u2);

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
                  AND COUNT(DISTINCT CASE WHEN a.account_kind != 'overlay' THEN account_wrapper(a.tax_wrapper) END) = 1
                 THEN MAX(CASE WHEN a.account_kind != 'overlay' THEN account_wrapper(a.tax_wrapper) END) END AS tax_wrapper,
            CASE WHEN COUNT(*) FILTER (WHERE a.account_kind != 'overlay') > 0
                  AND COUNT(DISTINCT CASE WHEN a.account_kind != 'overlay' THEN account_style(a.management_style) END) = 1
                 THEN MAX(CASE WHEN a.account_kind != 'overlay' THEN account_style(a.management_style) END) END AS management_style,
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
              AND COUNT(DISTINCT CASE WHEN a.account_kind != 'overlay' THEN account_wrapper(a.tax_wrapper) END) = 1
             THEN MAX(CASE WHEN a.account_kind != 'overlay' THEN account_wrapper(a.tax_wrapper) END) END AS tax_wrapper,
        CASE WHEN COUNT(*) FILTER (WHERE a.account_kind != 'overlay') > 0
              AND COUNT(DISTINCT CASE WHEN a.account_kind != 'overlay' THEN account_style(a.management_style) END) = 1
             THEN MAX(CASE WHEN a.account_kind != 'overlay' THEN account_style(a.management_style) END) END AS management_style,
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
               CAST(fx_pick_keep_zero(l.amt, l.ccy, a.base_currency,
                   b0.rate, b1.rate, b2.rate, b3.rate, b4.rate) AS DECIMAL(28,4)) END AS base_val
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
               CAST(fx_pick_keep_zero(l.amt, l.ccy, p_ccy,
                   o0.rate, o1.rate, o2.rate, o3.rate, o4.rate) AS DECIMAL(28,4)) AS out_val
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
               account_wrapper(a.tax_wrapper) AS tax_wrapper, account_style(a.management_style) AS management_style,
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
                   CAST(fx_pick_keep_zero(l.amt, l.ccy, bk.base_currency,
                       b0.rate, b1.rate, b2.rate, b3.rate, b4.rate) AS DECIMAL(28,4)) END AS base_val
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
                   CAST(fx_pick_keep_zero(l.amt, l.ccy, bk.base_currency,
                       b0.rate, b1.rate, b2.rate, b3.rate, b4.rate) AS DECIMAL(28,4)) END AS base_val
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
               CAST(fx_pick(l.amt, l.ccy, a.base_currency,
                   b0.rate, b1.rate, b2.rate, b3.rate, b4.rate) AS DECIMAL(28,4)) END AS base_val
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
               CAST(fx_pick(l.amt, l.ccy, p_ccy,
                   o0.rate, o1.rate, o2.rate, o3.rate, o4.rate) AS DECIMAL(28,4)) AS out_val
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
               a.portfolio_external_id, account_wrapper(a.tax_wrapper) AS tax_wrapper, account_style(a.management_style) AS management_style,
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
               account_wrapper(a.tax_wrapper) AS tax_wrapper, account_style(a.management_style) AS management_style,
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
               CAST(fx_pick(l.amt, l.ccy, bk.base_currency,
                   b0.rate, b1.rate, b2.rate, b3.rate, b4.rate) AS DECIMAL(28,4)) END AS base_val
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
               CAST(fx_pick(l.amt, l.ccy, p_ccy,
                   o0.rate, o1.rate, o2.rate, o3.rate, o4.rate) AS DECIMAL(28,4)) AS out_val
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
               CAST(fx_pick(l.amt, l.ccy, bk.base_currency,
                   b0.rate, b1.rate, b2.rate, b3.rate, b4.rate) AS DECIMAL(28,4)) END AS base_val
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
               CAST(fx_pick(l.amt, l.ccy, p_ccy,
                   o0.rate, o1.rate, o2.rate, o3.rate, o4.rate) AS DECIMAL(28,4)) AS out_val
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
           account_wrapper(a.tax_wrapper) AS tax_wrapper, account_style(a.management_style) AS management_style,
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
          FROM accounts WHERE account_is_debt(account_kind)),
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
         WHERE account_is_debt(a.account_kind)
        UNION
        SELECT DISTINCT c.currency
          FROM cash_balances c JOIN accounts a USING (silver_source_id, account_external_id)
         WHERE account_is_debt(a.account_kind)),
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
           AND NOT account_is_debt(a.account_kind)),
    valued AS (
        SELECT l.day,
               l.day - COALESCE(l.valued_at, l.snapshot_at) // 86400 <= 5 AS marked,
               CAST(fx_reporting_value(l.market_value, l.currency, 'USD', fxl, fxb) AS DECIMAL(38,10)) AS usd,
               CAST(fx_pick(l.market_value, l.currency, p_ccy,
                             o0.rate, o1.rate, o2.rate, o3.rate, o4.rate) AS DECIMAL(38,10)) AS out_val
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

-- ============================================================
-- Private markets (docs/RETURNS-NOTES.md, "Private markets").
--
--   * A private holding is a source's instrument whose asset class is
--     private equity or private debt, in any vehicle but a listed fund
--     (private_class), or one a contribution line names: a capital call
--     or an SPV subscription. The class is the instrument's, else the
--     holding's latest line's, else its latest leg's.
--   * One holding is one instrument identity (its ISIN, else its id) in
--     one portfolio, or in one account outside any portfolio: a fund a
--     custody account holds and a cash account of the same portfolio
--     pays for is one holding (private_holdings).
--   * Paid in is the calls, buys and fees on the holding. A holding with
--     none takes its stated cost: each line's book value on its
--     acquisition date.
--   * Every leg converts at its own day's rate, and the NAV at its
--     snapshot's day, so each reporting currency reads at historic FX.
--   * Sums are decimals, so they do not follow the order rows add in. A
--     holding's money rounds once, to the cent, and the total adds the
--     cents, so it is the sum of the rows as they print.
--   * The macros read a list of instants, and each row names its own in
--     as_of: a leg counts at every instant on or after it, a NAV is the
--     lines held at the instant.
--   * report_private_markets holds `wealthdb returns private`'s rows at
--     every quarter end and today, the money-weighted return with them.
--     web_private serves it.
--   * serving_private_flows holds the legs per holding and quarter, in
--     every reporting currency. web_private_flows serves it.
-- ============================================================

-- private_class: whether an instrument of asset class p_class, held
-- through p_vehicle, is a private holding's: private equity or private
-- debt, in any vehicle but a listed fund.
CREATE MACRO private_class(p_class, p_vehicle) AS
    p_class IN ('private_equity', 'private_debt') AND p_vehicle IS DISTINCT FROM 'etf';

-- private_group: the group a source's account p_account belongs to for
-- the private holdings, its portfolio p_portfolio, else the account.
CREATE MACRO private_group(p_portfolio, p_account) AS
    COALESCE('p:' || p_portfolio, 'a:' || p_account);

-- private_holdings: every key of a private holding at p_asof, a source's
-- instrument in one account, with the holding it belongs to. A leg up to
-- p_asof names a key (a contribution, buy, fee, distribution, sale or
-- transfer out), or a line of a snapshot up to it. The keys of one
-- holding share a group (private_group) and an identity, the instrument's
-- ISIN, else its id (identity). The holding is named by its key that holds the
-- latest line, else by its key with the first leg: holding_account and
-- holding_instrument. Its asset class and vehicle are that key's: the
-- instrument row's, else the latest line's, else the latest leg's that
-- states one. A line of a class that is no private one is read only for
-- a key a contribution names, for its class. The legs and lines read are
-- only those of an instrument whose identity some instrument row, line or
-- leg states a private class for, or a contribution names (cand): no
-- other one can be a private holding's.
CREATE MACRO private_holdings(p_asof) AS TABLE (
    WITH named AS (
        SELECT silver_source_id, instrument_external_id FROM instruments
         WHERE asset_class IN ('private_equity', 'private_debt')
        UNION
        SELECT silver_source_id, instrument_external_id FROM positions
         WHERE instrument_external_id IS NOT NULL AND snapshot_at <= p_asof
           AND asset_class IN ('private_equity', 'private_debt')
        UNION
        SELECT silver_source_id, instrument_external_id FROM transactions
         WHERE instrument_external_id IS NOT NULL AND occurred_at <= p_asof
           AND (kind = 'contribution' OR asset_class IN ('private_equity', 'private_debt')
                AND kind IN ('buy', 'fee', 'distribution', 'sell', 'transfer_out'))),
    ids AS (
        SELECT DISTINCT n.silver_source_id, COALESCE(i.isin, n.instrument_external_id) AS identity
          FROM named n
          LEFT JOIN instruments i ON i.silver_source_id = n.silver_source_id
           AND i.instrument_external_id = n.instrument_external_id),
    cand AS (
        SELECT silver_source_id, identity AS instrument_external_id FROM ids
        UNION
        SELECT i.silver_source_id, i.instrument_external_id
          FROM instruments i
          SEMI JOIN ids d ON d.silver_source_id = i.silver_source_id AND d.identity = i.isin),
    tx AS (
        SELECT t.silver_source_id, t.account_external_id, t.instrument_external_id,
               bool_or(t.kind = 'contribution') AS called, MIN(t.occurred_at) AS first_at,
               arg_max(t.asset_class, t.occurred_at) FILTER (WHERE t.asset_class IS NOT NULL) AS asset_class,
               arg_max(t.vehicle, t.occurred_at) FILTER (WHERE t.vehicle IS NOT NULL) AS vehicle
          FROM transactions t
          SEMI JOIN cand c ON c.silver_source_id = t.silver_source_id
           AND c.instrument_external_id = t.instrument_external_id
         WHERE t.occurred_at <= p_asof
           AND t.kind IN ('contribution', 'buy', 'fee', 'distribution', 'sell', 'transfer_out')
         GROUP BY ALL),
    ln AS (
        SELECT p.silver_source_id, p.account_external_id, p.instrument_external_id, MAX(p.snapshot_at) AS last_at,
               arg_max(p.asset_class, p.snapshot_at) AS asset_class, arg_max(p.vehicle, p.snapshot_at) AS vehicle
          FROM positions p
          SEMI JOIN cand d ON d.silver_source_id = p.silver_source_id
           AND d.instrument_external_id = p.instrument_external_id
          LEFT JOIN instruments i ON i.silver_source_id = p.silver_source_id
           AND i.instrument_external_id = p.instrument_external_id
          LEFT JOIN tx c ON c.called AND c.silver_source_id = p.silver_source_id
           AND c.account_external_id = p.account_external_id AND c.instrument_external_id = p.instrument_external_id
         WHERE p.snapshot_at <= p_asof
           AND (c.instrument_external_id IS NOT NULL
                OR private_class(COALESCE(i.asset_class, p.asset_class), COALESCE(i.vehicle, p.vehicle)))
         GROUP BY ALL),
    k AS (
        SELECT COALESCE(t.silver_source_id, l.silver_source_id) AS silver_source_id,
               COALESCE(t.account_external_id, l.account_external_id) AS account_external_id,
               COALESCE(t.instrument_external_id, l.instrument_external_id) AS instrument_external_id,
               COALESCE(t.called, FALSE) AS called, t.first_at, l.last_at,
               COALESCE(l.asset_class, t.asset_class) AS asset_class, COALESCE(l.vehicle, t.vehicle) AS vehicle
          FROM tx t
          FULL JOIN ln l ON l.silver_source_id = t.silver_source_id AND l.account_external_id = t.account_external_id
           AND l.instrument_external_id = t.instrument_external_id),
    m AS (
        SELECT k.silver_source_id, k.account_external_id, k.instrument_external_id, k.called, k.first_at, k.last_at,
               private_group(a.portfolio_external_id, k.account_external_id) AS holding_group,
               COALESCE(i.isin, k.instrument_external_id) AS identity,
               COALESCE(i.asset_class, k.asset_class) AS asset_class, COALESCE(i.vehicle, k.vehicle) AS vehicle
          FROM k
          LEFT JOIN instruments i ON i.silver_source_id = k.silver_source_id
           AND i.instrument_external_id = k.instrument_external_id
          LEFT JOIN accounts a ON a.silver_source_id = k.silver_source_id
           AND a.account_external_id = k.account_external_id),
    r AS (
        SELECT *,
               first_value(account_external_id) OVER h AS holding_account,
               first_value(instrument_external_id) OVER h AS holding_instrument,
               first_value(asset_class) OVER h AS holding_class, first_value(vehicle) OVER h AS holding_vehicle,
               bool_or(called OR private_class(asset_class, vehicle))
                   OVER (PARTITION BY silver_source_id, holding_group, identity) AS private
          FROM m
        WINDOW h AS (PARTITION BY silver_source_id, holding_group, identity
                     ORDER BY last_at DESC NULLS LAST, first_at NULLS LAST, account_external_id, instrument_external_id
                     ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING))
    SELECT silver_source_id, account_external_id, instrument_external_id, holding_group, identity,
           holding_account, holding_instrument, holding_class AS asset_class, holding_vehicle AS vehicle
      FROM r
     WHERE private
);

-- private_legs: every leg of a private holding, and every exit in kind
-- out of a private holding's group that is no holding's, in p_ccy, a row
-- for each instant of p_instants it falls on or before (as_of). A leg
-- names its holding by the holding's account and instrument
-- (private_holdings), and its group. leg is call (money in: a
-- contribution, a buy or a fee on the holding, a refund of one negative),
-- distribution (cash back: a distribution or a sale, and the interest,
-- dividends and coupons of a private debt holding) or in_kind (a transfer
-- out: an exit in kind, at the value its row states). An exit in kind is
-- the holding's when it moves the holding, or when its payload's holding
-- names it: an SPV's exit in the listed shares it received names the
-- shares as its instrument. A transfer out that a transfer in of the same
-- identity into another account of its group answers on its day, of the
-- same quantity where both state one, is a move inside the group and no
-- leg. paid is what the holder paid in: positive on a call, negative on
-- what came back; paid_own is the same in the leg's own currency. paid is
-- NULL with no rate to p_ccy, except that a zero reads zero in every
-- currency. unvalued marks an exit in kind that states no value: its paid
-- is zero. unattributed marks an equity-transfer ledger out-row out of a
-- group that holds a private holding, which is no holding's: its account
-- and instrument are its own, and only the total counts it. A ledger exit
-- of an instrument the group bought or took in moves a direct holding, and
-- one of cash names no instrument: neither is a leg, nor is any other
-- transfer out that no holding claims. from_cost marks the calls of a
-- holding with no call of its own up to the instant: one per line it
-- holds at the instant, else per line of its last snapshot, the line's
-- book value on its acquisition date, where every one of those lines
-- states both.
CREATE MACRO private_legs(p_instants, p_ccy) AS TABLE (
    WITH ins AS (SELECT DISTINCT unnest(p_instants) AS as_of),
    held AS (SELECT * FROM private_holdings(list_max(p_instants))),
    groups AS (SELECT DISTINCT silver_source_id, holding_group FROM held),
    t0 AS (
        SELECT t.*, private_group(a.portfolio_external_id, t.account_external_id) AS holding_group,
               COALESCE(i.isin, t.instrument_external_id) AS identity
          FROM transactions t
          LEFT JOIN accounts a ON a.silver_source_id = t.silver_source_id
           AND a.account_external_id = t.account_external_id
          LEFT JOIN instruments i ON i.silver_source_id = t.silver_source_id
           AND i.instrument_external_id = t.instrument_external_id
          SEMI JOIN groups g ON g.silver_source_id = t.silver_source_id
           AND g.holding_group = private_group(a.portfolio_external_id, t.account_external_id)
         WHERE t.occurred_at <= list_max(p_instants)),
    direct AS (
        SELECT DISTINCT silver_source_id, holding_group, instrument_external_id
          FROM t0
         WHERE kind IN ('buy', 'transfer_in') AND instrument_external_id IS NOT NULL),
    moved_in AS (
        SELECT silver_source_id, holding_group, account_external_id, identity, occurred_at // 86400 AS day,
               abs(quantity) AS quantity
          FROM t0
         WHERE kind = 'transfer_in' AND instrument_external_id IS NOT NULL),
    tx AS (
        SELECT t.silver_source_id, COALESCE(h.holding_account, t.account_external_id) AS account_external_id,
               COALESCE(h.holding_instrument, t.instrument_external_id) AS instrument_external_id,
               t.holding_group, t.transaction_external_id, t.occurred_at, t.occurred_at // 86400 AS day,
               CASE WHEN t.kind IN ('contribution', 'buy', 'fee') THEN 'call' WHEN t.kind = 'transfer_out' THEN 'in_kind'
                    ELSE 'distribution' END AS leg,
               t.currency,
               CASE WHEN t.kind = 'transfer_out' THEN -abs(COALESCE(t.net_amount, 0))
                    ELSE -t.net_amount END AS paid_own,
               t.kind = 'transfer_out' AND COALESCE(t.net_amount, 0) = 0 AS unvalued,
               h.holding_instrument IS NULL AS unattributed,
               FALSE AS from_cost
          FROM t0 t
          LEFT JOIN held h ON h.silver_source_id = t.silver_source_id
           AND h.account_external_id = t.account_external_id
           AND h.instrument_external_id = CASE WHEN t.kind = 'transfer_out'
                   THEN COALESCE(json_extract_string(t.payload, '$.holding'), t.instrument_external_id)
                   ELSE t.instrument_external_id END
          LEFT JOIN direct d ON d.silver_source_id = t.silver_source_id
           AND d.holding_group = t.holding_group
           AND d.instrument_external_id = t.instrument_external_id
         WHERE (t.kind IN ('contribution', 'buy', 'fee', 'distribution', 'sell', 'transfer_out')
                OR t.kind IN ('interest', 'dividend', 'coupon') AND h.asset_class = 'private_debt')
           AND (h.holding_instrument IS NOT NULL
                OR t.kind = 'transfer_out' AND t.instrument_external_id IS NOT NULL
                   AND d.instrument_external_id IS NULL
                   AND COALESCE(json_extract_string(t.payload, '$.equity_transfer_ledger'), '') = 'true')
           AND NOT (t.kind = 'transfer_out' AND EXISTS (
                   SELECT 1 FROM moved_in m
                    WHERE m.silver_source_id = t.silver_source_id AND m.holding_group = t.holding_group
                      AND m.account_external_id <> t.account_external_id AND m.identity = t.identity
                      AND m.day = t.occurred_at // 86400
                      AND (m.quantity IS NULL OR t.quantity IS NULL OR m.quantity = abs(t.quantity))))),
    -- Each holding's first call, and the keys of the holdings with none
    -- up to an instant, with their lines.
    called AS (
        SELECT silver_source_id, account_external_id, instrument_external_id, MIN(occurred_at) AS first_call
          FROM tx
         WHERE leg = 'call' AND NOT unattributed
         GROUP BY ALL),
    bare AS (
        SELECT i.as_of, h.*
          FROM ins i CROSS JOIN held h
          LEFT JOIN called c ON c.silver_source_id = h.silver_source_id AND c.account_external_id = h.holding_account
           AND c.instrument_external_id = h.holding_instrument
         WHERE c.first_call IS NULL OR c.first_call > i.as_of),
    lines AS (
        SELECT p.silver_source_id, p.account_external_id AS key_account, p.instrument_external_id AS key_instrument,
               b.holding_account AS account_external_id, b.holding_instrument AS instrument_external_id,
               b.holding_group, p.snapshot_at, p.account_external_id || ':' || p.position_key AS position_key,
               p.currency, p.book_value, p.acquisition_date
          FROM positions p
          JOIN (SELECT DISTINCT silver_source_id, account_external_id, instrument_external_id, holding_account,
                       holding_instrument, holding_group FROM bare) b
            ON b.silver_source_id = p.silver_source_id AND b.account_external_id = p.account_external_id
           AND b.instrument_external_id = p.instrument_external_id
         WHERE p.snapshot_at <= list_max(p_instants)),
    -- The lines a bare holding holds at the instant: each of its keys'
    -- lines in that key's account's snapshot at the instant.
    account_snaps AS (
        SELECT DISTINCT p.silver_source_id, p.account_external_id, p.snapshot_at
          FROM positions p
          SEMI JOIN lines l ON l.silver_source_id = p.silver_source_id AND l.key_account = p.account_external_id
         WHERE p.snapshot_at <= list_max(p_instants)),
    at_snap AS (
        SELECT b.as_of, b.silver_source_id, b.account_external_id, b.instrument_external_id, s.snapshot_at
          FROM bare b
          ASOF JOIN account_snaps s ON s.silver_source_id = b.silver_source_id
           AND s.account_external_id = b.account_external_id AND s.snapshot_at <= b.as_of),
    now_lines AS (
        SELECT a.as_of, l.*
          FROM at_snap a
          JOIN lines l ON l.silver_source_id = a.silver_source_id AND l.key_account = a.account_external_id
           AND l.key_instrument = a.instrument_external_id AND l.snapshot_at = a.snapshot_at),
    -- A bare holding that holds no line at the instant reads the lines of
    -- its last snapshot before it.
    snaps AS (
        SELECT DISTINCT silver_source_id, account_external_id, instrument_external_id, snapshot_at FROM lines),
    away AS (
        SELECT DISTINCT b.as_of, b.silver_source_id, b.holding_account AS account_external_id,
               b.holding_instrument AS instrument_external_id
          FROM bare b
          ANTI JOIN now_lines c ON c.as_of = b.as_of AND c.silver_source_id = b.silver_source_id
           AND c.account_external_id = b.holding_account AND c.instrument_external_id = b.holding_instrument),
    last_lines AS (
        SELECT g.as_of, l.*
          FROM away g
          ASOF JOIN snaps s ON s.silver_source_id = g.silver_source_id
           AND s.account_external_id = g.account_external_id
           AND s.instrument_external_id = g.instrument_external_id AND s.snapshot_at <= g.as_of
          JOIN lines l ON l.silver_source_id = s.silver_source_id AND l.account_external_id = s.account_external_id
           AND l.instrument_external_id = s.instrument_external_id AND l.snapshot_at = s.snapshot_at),
    cost AS (
        SELECT l.as_of, l.silver_source_id, l.account_external_id, l.instrument_external_id, l.holding_group,
               'cost:' || l.position_key AS transaction_external_id,
               CAST(epoch(l.acquisition_date) AS BIGINT) AS occurred_at,
               CAST(epoch(l.acquisition_date) AS BIGINT) // 86400 AS day, 'call' AS leg, l.currency,
               l.book_value AS paid_own, FALSE AS unvalued, FALSE AS unattributed, TRUE AS from_cost
          FROM (SELECT * FROM now_lines UNION ALL SELECT * FROM last_lines) l
        QUALIFY bool_and(l.book_value IS NOT NULL AND l.acquisition_date IS NOT NULL)
                    OVER (PARTITION BY l.as_of, l.silver_source_id, l.account_external_id, l.instrument_external_id)),
    every AS (
        SELECT i.as_of, t.* FROM ins i JOIN tx t ON t.occurred_at <= i.as_of
        UNION ALL BY NAME
        SELECT * FROM cost)
    SELECT l.as_of, l.silver_source_id, l.account_external_id, l.instrument_external_id, l.holding_group,
           l.transaction_external_id, l.day, l.leg, l.currency, l.unvalued, l.unattributed, l.from_cost, l.paid_own,
           CAST(fx_pick_keep_zero(l.paid_own, l.currency, p_ccy,
                        o0.rate, o1.rate, o2.rate, o3.rate, o4.rate) AS DECIMAL(38,10)) AS paid
      FROM every l
      ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.currency AND o0.to_ccy = p_ccy AND o0.day <= l.day
      ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.currency AND o1.to_ccy = 'CHF' AND o1.day <= l.day
      ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= l.day
      ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.currency AND o3.to_ccy = 'USD' AND o3.day <= l.day
      ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= l.day
     ORDER BY l.as_of, l.silver_source_id, l.account_external_id, l.instrument_external_id, l.day,
              l.transaction_external_id
);

-- at_cost marks a NAV whose every line the source carries at cost, not
-- at a valuation of its own: what moved in before any mark. AngelList
-- states it as a position's valuation_basis cost, Carta's fund carry as
-- called_capital. Its multiples read 1 and its IRR 0 by construction.
-- A mark plus what moved in since (mark_plus_cost) is a mark.
--
-- report_private: one row per private holding at each instant of
-- p_instants (as_of), in p_ccy, then the instant's total. A holding is
-- named by its account and instrument (private_holdings), and its lines
-- are those of every key it has. A holding with no money in, out or
-- marked is left out. paid_in is the calls, net of refunds, blank where
-- the holding has none (no_cost); distributed the cash and the exits in
-- kind that came back; the total's also the unattributed exits out of
-- the groups of the holdings it lists. nav is the value of the holding's
-- lines at the instant. It is 0 where the holding had lines before and
-- has none now, and NULL where it never had one, or where its lines never
-- stated a value but zero and nothing came back, which no_mark says.
-- marked_at is the oldest mark of its lines at the instant,
-- coalesce(valued_at, snapshot_at), else the mark of its last line
-- before it left. first_call is the day of the first call, else of the
-- first leg, else of the first line. status is exited where the NAV is 0
-- after something came back or once the holding left, partially_exited
-- where something came back and it is still held, else held. dpi is
-- distributed over paid_in, tvpi distributed plus nav over paid_in.
-- end_at is the day a money-weighted return ends: the later of the NAV's
-- snapshot and the last leg, else the last line. commitment, unfunded
-- and the stated_ figures are the holding's latest capital account on
-- or before the instant (capital_accounts), stated_as_of its day, NULL
-- where it has none: commitment and unfunded in p_ccy at that day's
-- rate, stated_tvpi and stated_dpi as the source states them, else the
-- multiples its stated figures give. Where the account states no
-- remainder and no call or payment, unfunded is the commitment less the
-- holding's calls, each in the commitment's currency at its own day's
-- rate: 0 where that is below 1% of the commitment, NULL with no
-- paid_in. An exited holding's unfunded is 0: a holding sold or wound up
-- whole owes no further calls. The total
-- adds what the holdings state: a blank adds nothing, and no_cost and
-- no_mark count the holdings it leaves out. Its dpi pools the holdings
-- with a paid_in, its tvpi those with a paid_in and a nav, the
-- unattributed exits in both; its marked_at is the oldest mark; its
-- stated_tvpi, stated_dpi and stated_as_of are blank. holding_group is
-- the holding's group, '' on the total. quality names what a figure
-- misses: no_mark, no_cost, paid_in_from_cost, exited, at_cost,
-- call_after_mark, distribution_after_mark, exit_unvalued=N,
-- unpriced=N; on the total, each tag with the count of holdings it holds
-- on, unattributed_exit=N the exits it counts, and
-- unmatched_commitment=N the commitments of a private holding stated at
-- the instant that no listed holding takes, which it leaves out. The
-- total's exit_unvalued and unpriced count those exits too: the holdings
-- plus the unattributed exits that state no value, or have no rate. A
-- stated commitment with no rate counts as unpriced, and so does an
-- unfunded its calls give where one of them has no rate.
CREATE MACRO report_private(p_instants, p_ccy) AS TABLE (
    WITH ins AS (SELECT DISTINCT unnest(p_instants) AS as_of),
    legs AS (SELECT * FROM private_legs(p_instants, p_ccy)),
    held AS (SELECT * FROM private_holdings(list_max(p_instants))),
    holdings AS (
        SELECT DISTINCT silver_source_id, holding_account AS account_external_id,
               holding_instrument AS instrument_external_id, holding_group, identity, asset_class, vehicle
          FROM held),
    f AS (
        SELECT as_of, silver_source_id, account_external_id, instrument_external_id,
               MIN(day) FILTER (WHERE leg = 'call') AS first_call,
               MIN(day) AS first_leg,
               MAX(day) FILTER (WHERE leg = 'call') AS last_call,
               MAX(day) FILTER (WHERE leg <> 'call') AS last_back,
               MAX(day) AS last_leg,
               COUNT(*) FILTER (WHERE leg = 'call') > 0 AS called,
               COUNT(*) FILTER (WHERE leg <> 'call') > 0 AS backed,
               bool_or(from_cost) AS from_cost,
               COALESCE(SUM(paid) FILTER (WHERE leg = 'call'), 0) AS paid_in,
               COALESCE(-SUM(paid) FILTER (WHERE leg <> 'call'), 0) AS distributed,
               COALESCE(SUM(paid_own) FILTER (WHERE leg = 'call'), 0) <> 0
                   OR COALESCE(SUM(paid_own) FILTER (WHERE leg <> 'call'), 0) <> 0 AS moved,
               COUNT(*) FILTER (WHERE unvalued) AS exits_unvalued,
               COUNT(*) FILTER (WHERE paid IS NULL) AS legs_unpriced
          FROM legs
         WHERE NOT unattributed
         GROUP BY ALL),
    -- Each holding's snapshots up to the last instant, each with its
    -- oldest mark, the first, and whether a line up to it ever stated a
    -- value other than zero.
    marks AS (
        SELECT p.silver_source_id, h.holding_account AS account_external_id,
               h.holding_instrument AS instrument_external_id, p.snapshot_at,
               MIN(COALESCE(p.valued_at, p.snapshot_at)) AS mark,
               MIN(p.snapshot_at) OVER (PARTITION BY p.silver_source_id, h.holding_account,
                                                     h.holding_instrument) AS first_line,
               bool_or(bool_or(COALESCE(p.market_value, 0) <> 0))
                   OVER (PARTITION BY p.silver_source_id, h.holding_account, h.holding_instrument
                         ORDER BY p.snapshot_at ROWS UNBOUNDED PRECEDING) AS valued_by
          FROM positions p
          JOIN held h ON h.silver_source_id = p.silver_source_id
           AND h.account_external_id = p.account_external_id AND h.instrument_external_id = p.instrument_external_id
         WHERE p.snapshot_at <= list_max(p_instants)
         GROUP BY 1, 2, 3, 4),
    seen AS (
        SELECT i.as_of, m.silver_source_id, m.account_external_id, m.instrument_external_id,
               m.snapshot_at AS last_line, m.mark AS last_mark, m.first_line, m.valued_by
          FROM ins i CROSS JOIN holdings h
          ASOF JOIN marks m ON m.silver_source_id = h.silver_source_id
           AND m.account_external_id = h.account_external_id
           AND m.instrument_external_id = h.instrument_external_id AND m.snapshot_at <= i.as_of),
    base AS (
        SELECT as_of, silver_source_id, account_external_id, instrument_external_id FROM f
        UNION
        SELECT as_of, silver_source_id, account_external_id, instrument_external_id FROM seen),
    lines AS (
        SELECT pk.instant AS as_of, p.silver_source_id, h.holding_account AS account_external_id,
               h.holding_instrument AS instrument_external_id,
               p.snapshot_at, COALESCE(p.valued_at, p.snapshot_at) AS marked_at, p.market_value,
               COALESCE(json_extract_string(p.payload, '$.valuation_basis') IN ('cost', 'called_capital'),
                        FALSE) AS at_cost,
               CAST(fx_pick_keep_zero(p.market_value, p.currency, p_ccy,
                            o0.rate, o1.rate, o2.rate, o3.rate, o4.rate) AS DECIMAL(38,10)) AS value_x
          FROM pos_active_at(p_instants) pk
          JOIN positions p ON p.silver_source_id = pk.src AND p.account_external_id = pk.acct
           AND p.snapshot_at = pk.snap
          JOIN held h ON h.silver_source_id = p.silver_source_id
           AND h.account_external_id = p.account_external_id AND h.instrument_external_id = p.instrument_external_id
          JOIN base b ON b.as_of = pk.instant AND b.silver_source_id = p.silver_source_id
           AND b.account_external_id = h.holding_account AND b.instrument_external_id = h.holding_instrument
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = p.currency AND o0.to_ccy = p_ccy AND o0.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = p.currency AND o1.to_ccy = 'CHF' AND o1.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = p.currency AND o3.to_ccy = 'USD' AND o3.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (p.snapshot_at // 86400)),
    navs AS (
        SELECT as_of, silver_source_id, account_external_id, instrument_external_id,
               MAX(snapshot_at) AS nav_at, MIN(marked_at) AS marked_at, SUM(value_x) AS nav,
               SUM(market_value) <> 0 AS valued, bool_and(at_cost) AS at_cost,
               COUNT(*) FILTER (WHERE value_x IS NULL) AS lines_unpriced
          FROM lines
         GROUP BY ALL),
    -- A holding whose lines never stated a value other than zero up to
    -- the instant, and from which nothing came back, was never valued: a
    -- zero that was never anything else is no mark. One marked before
    -- and at zero now is written off, and keeps its zero.
    v AS (
        SELECT b.*, s.silver_source_id IS NULL
                    OR NOT s.valued_by AND NOT COALESCE(f.backed, FALSE) AS no_mark
          FROM base b
          LEFT JOIN f ON f.as_of = b.as_of AND f.silver_source_id = b.silver_source_id
           AND f.account_external_id = b.account_external_id AND f.instrument_external_id = b.instrument_external_id
          LEFT JOIN seen s ON s.as_of = b.as_of AND s.silver_source_id = b.silver_source_id
           AND s.account_external_id = b.account_external_id AND s.instrument_external_id = b.instrument_external_id),
    h AS (
        SELECT b.as_of, b.silver_source_id, b.account_external_id, b.instrument_external_id,
               COALESCE(f.first_call, f.first_leg, s.first_line // 86400) AS first_day,
               COALESCE(f.called, FALSE) AS called, COALESCE(f.backed, FALSE) AS backed,
               COALESCE(f.from_cost, FALSE) AS from_cost,
               CASE WHEN f.called THEN ROUND(f.paid_in, 2) END AS paid_in,
               ROUND(COALESCE(f.distributed, 0), 2) AS distributed,
               n.nav_at, CASE WHEN NOT b.no_mark THEN COALESCE(n.marked_at, s.last_mark) END AS marked_at,
               GREATEST(COALESCE(n.nav_at, 0), COALESCE(f.last_leg * 86400, s.last_line, 0)) AS end_at,
               CASE WHEN b.no_mark THEN NULL WHEN n.nav_at IS NOT NULL THEN ROUND(n.nav, 2) ELSE 0 END AS nav,
               COALESCE(f.legs_unpriced, 0) + COALESCE(n.lines_unpriced, 0) AS unpriced,
               COALESCE(f.exits_unvalued, 0) AS exits_unvalued,
               b.no_mark,
               n.nav_at IS NULL AND s.silver_source_id IS NOT NULL AS gone,
               COALESCE(n.at_cost, FALSE) AND NOT b.no_mark AS at_cost,
               COALESCE(f.moved, FALSE) OR COALESCE(n.valued, FALSE) AS invested,
               NOT b.no_mark AND n.marked_at IS NOT NULL AND f.last_call > n.marked_at // 86400 AS call_after_mark,
               NOT b.no_mark AND n.marked_at IS NOT NULL AND f.last_back > n.marked_at // 86400 AS back_after_mark
          FROM v b
          LEFT JOIN f ON f.as_of = b.as_of AND f.silver_source_id = b.silver_source_id
           AND f.account_external_id = b.account_external_id AND f.instrument_external_id = b.instrument_external_id
          LEFT JOIN navs n ON n.as_of = b.as_of AND n.silver_source_id = b.silver_source_id
           AND n.account_external_id = b.account_external_id AND n.instrument_external_id = b.instrument_external_id
          LEFT JOIN seen s ON s.as_of = b.as_of AND s.silver_source_id = b.silver_source_id
           AND s.account_external_id = b.account_external_id AND s.instrument_external_id = b.instrument_external_id),
    -- A holding with no money in, out or marked, in its own currency, is
    -- no investment: a call refunded whole, one of no amount, or a line
    -- of no cost and no value, or one that has left with nothing moved.
    k AS (
        SELECT h.*, h.gone OR COALESCE(h.nav = 0 AND h.backed, FALSE) AS exited, c.holding_group
          FROM h
          JOIN holdings c ON c.silver_source_id = h.silver_source_id AND c.account_external_id = h.account_external_id
           AND c.instrument_external_id = h.instrument_external_id
         WHERE h.invested),
    -- The capital accounts the sources state up to the last instant, each
    -- with its identity and the holding it states: the one of its
    -- identity in its account's group, or in its source where it names
    -- no account. One that matches two holdings, or none, states no
    -- holding. Its commitment and unfunded convert at its day's rate
    -- (rate); unfunded is the stated remainder, else the commitment less
    -- what was called, else paid in, never below zero; none where it
    -- states no remainder and no commitment with a call or a payment.
    -- commitment_own is the commitment in its currency, stated_ccy. Its
    -- multiples are the source's own, else the ratios of its stated
    -- figures over what was paid in, else called.
    cap AS (
        SELECT s.silver_source_id, s.account_external_id AS stated_account,
               s.instrument_external_id AS stated_instrument, s.as_of AS stated_as_of,
               s.source_ref AS stated_ref, COALESCE(i.isin, s.instrument_external_id) AS identity,
               s.commitment IS NOT NULL AS committed, s.currency AS stated_ccy, s.commitment AS commitment_own,
               fx_pick(1, s.currency, p_ccy, o0.rate, o1.rate, o2.rate, o3.rate, o4.rate) AS rate,
               CAST(fx_pick_keep_zero(s.commitment, s.currency, p_ccy,
                            o0.rate, o1.rate, o2.rate, o3.rate, o4.rate) AS DECIMAL(38,10)) AS commitment,
               CAST(fx_pick_keep_zero(COALESCE(s.unfunded,
                                               CASE WHEN s.commitment IS NOT NULL AND COALESCE(s.called, s.paid_in) IS NOT NULL
                                                    THEN GREATEST(s.commitment - COALESCE(s.called, s.paid_in), 0) END),
                            s.currency, p_ccy, o0.rate, o1.rate, o2.rate, o3.rate, o4.rate) AS DECIMAL(38,10)) AS unfunded,
               COALESCE(s.tvpi, CASE WHEN COALESCE(s.paid_in, s.called) > 0
                                     THEN CAST((s.distributed + s.nav) / COALESCE(s.paid_in, s.called) AS DOUBLE) END)
                   AS stated_tvpi,
               COALESCE(s.dpi, CASE WHEN COALESCE(s.paid_in, s.called) > 0
                                    THEN CAST(s.distributed / COALESCE(s.paid_in, s.called) AS DOUBLE) END) AS stated_dpi,
               CASE WHEN count(*) OVER w = 1 THEN d.account_external_id END AS account_external_id,
               CASE WHEN count(*) OVER w = 1 THEN d.instrument_external_id END AS instrument_external_id
          FROM capital_accounts s
          LEFT JOIN instruments i ON i.silver_source_id = s.silver_source_id
           AND i.instrument_external_id = s.instrument_external_id
          LEFT JOIN accounts a ON a.silver_source_id = s.silver_source_id
           AND a.account_external_id = s.account_external_id
          LEFT JOIN holdings d ON d.silver_source_id = s.silver_source_id
           AND d.identity = COALESCE(i.isin, s.instrument_external_id)
           AND (s.account_external_id = ''
                OR d.holding_group = private_group(a.portfolio_external_id, s.account_external_id))
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = s.currency AND o0.to_ccy = p_ccy AND o0.day <= (s.as_of // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = s.currency AND o1.to_ccy = 'CHF' AND o1.day <= (s.as_of // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (s.as_of // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = s.currency AND o3.to_ccy = 'USD' AND o3.day <= (s.as_of // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (s.as_of // 86400)
         WHERE s.as_of <= list_max(p_instants)
        WINDOW w AS (PARTITION BY s.silver_source_id, s.account_external_id, s.instrument_external_id, s.as_of,
                                  s.source_ref)
        QUALIFY row_number() OVER (w ORDER BY d.account_external_id, d.instrument_external_id) = 1),
    -- At each instant, each account's latest statements of an instrument
    -- on or before it, every document of that day, and each holding's
    -- latest of those. Two accounts of one source that state one
    -- instrument keep a statement each.
    cur AS (
        SELECT i.as_of, c.*
          FROM ins i
          JOIN cap c ON c.stated_as_of <= i.as_of
        QUALIFY rank() OVER (PARTITION BY i.as_of, c.silver_source_id, c.stated_account, c.stated_instrument
                             ORDER BY c.stated_as_of DESC) = 1),
    st AS (
        SELECT *
          FROM cur
         WHERE instrument_external_id IS NOT NULL
        QUALIFY row_number() OVER (PARTITION BY as_of, silver_source_id, account_external_id, instrument_external_id
                                   ORDER BY stated_as_of DESC, stated_account, stated_instrument, stated_ref) = 1),
    -- The exits in kind no holding names, out of a listed holding's
    -- group: what they return rounds once, to the cent.
    u AS (
        SELECT l.as_of, COUNT(*) AS exits, ROUND(COALESCE(-SUM(l.paid), 0), 2) AS back, MAX(l.day) AS last_leg,
               COUNT(*) FILTER (WHERE l.unvalued) AS exits_unvalued,
               COUNT(*) FILTER (WHERE l.paid IS NULL) AS exits_unpriced
          FROM legs l
          SEMI JOIN k ON k.as_of = l.as_of AND k.silver_source_id = l.silver_source_id
           AND k.holding_group = l.holding_group
         WHERE l.unattributed
         GROUP BY l.as_of),
    -- The calls of each holding whose capital account at the instant
    -- states a commitment and no unfunded, in the commitment's currency
    -- at each call's day's rate: paid_in, rounded once to the cent, and
    -- unpriced, the calls with no rate.
    kc AS (
        SELECT as_of, silver_source_id, account_external_id, instrument_external_id,
               ROUND(SUM(paid), 2) AS paid_in, COUNT(*) FILTER (WHERE paid IS NULL) AS unpriced
          FROM (SELECT l.as_of, l.silver_source_id, l.account_external_id, l.instrument_external_id,
                       CAST(fx_pick_keep_zero(l.paid_own, l.currency, t.stated_ccy,
                                    o0.rate, o1.rate, o2.rate, o3.rate, o4.rate) AS DECIMAL(38,10)) AS paid
                  FROM legs l
                  JOIN st t ON t.as_of = l.as_of AND t.silver_source_id = l.silver_source_id
                   AND t.account_external_id = l.account_external_id
                   AND t.instrument_external_id = l.instrument_external_id
                  ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.currency AND o0.to_ccy = t.stated_ccy AND o0.day <= l.day
                  ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.currency AND o1.to_ccy = 'CHF' AND o1.day <= l.day
                  ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = t.stated_ccy AND o2.day <= l.day
                  ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.currency AND o3.to_ccy = 'USD' AND o3.day <= l.day
                  ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = t.stated_ccy AND o4.day <= l.day
                 WHERE l.leg = 'call' AND NOT l.unattributed AND t.committed AND t.unfunded IS NULL)
         GROUP BY ALL),
    -- Each listed holding with its capital account at the instant. An
    -- exited one owes no further calls. Where the account gives no
    -- unfunded, unfunded is the commitment less the holding's calls
    -- (kc), converted once at the account's day's rate. Fees and
    -- rounding part a stated amount from a computed one, so a rest below
    -- 1% of the commitment reads 0. With no call it is NULL, and so it is
    -- where a call has no rate, which is unpriced. A stated commitment
    -- with no rate to p_ccy is unpriced.
    ks AS (
        SELECT k.*, ROUND(t.commitment, 2) AS commitment,
               CASE WHEN k.exited AND (t.committed OR t.unfunded IS NOT NULL) THEN 0
                    WHEN t.unfunded IS NOT NULL THEN ROUND(t.unfunded, 2)
                    WHEN c.unpriced > 0 THEN NULL
                    WHEN t.commitment_own - c.paid_in < 0.01 * t.commitment_own THEN 0
                    ELSE ROUND(CAST((t.commitment_own - c.paid_in) * t.rate AS DECIMAL(38,10)), 2) END AS unfunded,
               t.stated_tvpi,
               t.stated_dpi, t.stated_as_of,
               k.unpriced + CASE WHEN t.committed AND t.commitment IS NULL
                                      OR NOT k.exited AND c.unpriced > 0 THEN 1 ELSE 0 END AS unpriced_all
          FROM k
          LEFT JOIN st t ON t.as_of = k.as_of AND t.silver_source_id = k.silver_source_id
           AND t.account_external_id = k.account_external_id AND t.instrument_external_id = k.instrument_external_id
          LEFT JOIN kc c ON c.as_of = k.as_of AND c.silver_source_id = k.silver_source_id
           AND c.account_external_id = k.account_external_id AND c.instrument_external_id = k.instrument_external_id),
    -- The commitments stated at each instant that no listed holding
    -- takes, of a private holding's identity: its holding is not listed
    -- at the instant, or the account would state two holdings.
    um AS (
        SELECT c.as_of, COUNT(*) AS n
          FROM cur c
          SEMI JOIN holdings d ON d.silver_source_id = c.silver_source_id AND d.identity = c.identity
          ANTI JOIN k ON k.as_of = c.as_of AND k.silver_source_id = c.silver_source_id
           AND k.account_external_id = c.account_external_id AND k.instrument_external_id = c.instrument_external_id
         WHERE c.committed
         GROUP BY c.as_of),
    pr AS (
        SELECT as_of, FALSE AS is_total, silver_source_id, account_external_id, instrument_external_id, holding_group,
               first_day * 86400 AS first_call, paid_in, commitment, unfunded, distributed, nav, marked_at, end_at,
               CASE WHEN exited THEN 'exited' WHEN backed THEN 'partially_exited' ELSE 'held' END AS status,
               paid_in AS dpi_paid, distributed AS dpi_back, paid_in AS tvpi_paid, distributed + nav AS tvpi_value,
               stated_tvpi, stated_dpi, stated_as_of,
               concat_ws(';', CASE WHEN no_mark THEN 'no_mark' END,
                              CASE WHEN NOT called THEN 'no_cost' END,
                              CASE WHEN from_cost THEN 'paid_in_from_cost' END,
                              CASE WHEN exited THEN 'exited' END,
                              CASE WHEN at_cost THEN 'at_cost' END,
                              CASE WHEN call_after_mark THEN 'call_after_mark' END,
                              CASE WHEN back_after_mark THEN 'distribution_after_mark' END,
                              CASE WHEN exits_unvalued > 0 THEN 'exit_unvalued=' || exits_unvalued END,
                              CASE WHEN unpriced_all > 0 THEN 'unpriced=' || unpriced_all END) AS quality
          FROM ks
        UNION ALL
        SELECT k.as_of, TRUE, '', '', '', '', MIN(first_day) * 86400,
               SUM(paid_in), SUM(commitment), SUM(unfunded),
               SUM(distributed) + COALESCE(ANY_VALUE(u.back), 0),
               SUM(nav), MIN(marked_at),
               GREATEST(MAX(end_at), COALESCE(ANY_VALUE(u.last_leg) * 86400, 0)), NULL,
               SUM(paid_in), SUM(distributed) FILTER (WHERE paid_in IS NOT NULL) + COALESCE(ANY_VALUE(u.back), 0),
               SUM(paid_in) FILTER (WHERE nav IS NOT NULL),
               SUM(distributed + nav) FILTER (WHERE paid_in IS NOT NULL) + COALESCE(ANY_VALUE(u.back), 0),
               NULL, NULL, NULL,
               concat_ws(';', CASE WHEN count_if(no_mark) > 0 THEN 'no_mark=' || count_if(no_mark) END,
                              CASE WHEN count_if(NOT called) > 0 THEN 'no_cost=' || count_if(NOT called) END,
                              CASE WHEN count_if(from_cost) > 0 THEN 'paid_in_from_cost=' || count_if(from_cost) END,
                              CASE WHEN count_if(exited) > 0 THEN 'exited=' || count_if(exited) END,
                              CASE WHEN count_if(at_cost) > 0 THEN 'at_cost=' || count_if(at_cost) END,
                              CASE WHEN count_if(call_after_mark) > 0
                                   THEN 'call_after_mark=' || count_if(call_after_mark) END,
                              CASE WHEN count_if(back_after_mark) > 0
                                   THEN 'distribution_after_mark=' || count_if(back_after_mark) END,
                              CASE WHEN count_if(k.exits_unvalued > 0) + COALESCE(ANY_VALUE(u.exits_unvalued), 0) > 0
                                   THEN 'exit_unvalued=' || (count_if(k.exits_unvalued > 0)
                                                             + COALESCE(ANY_VALUE(u.exits_unvalued), 0)) END,
                              CASE WHEN ANY_VALUE(u.exits) > 0 THEN 'unattributed_exit=' || ANY_VALUE(u.exits) END,
                              CASE WHEN ANY_VALUE(m.n) > 0 THEN 'unmatched_commitment=' || ANY_VALUE(m.n) END,
                              CASE WHEN count_if(unpriced_all > 0) + COALESCE(ANY_VALUE(u.exits_unpriced), 0) > 0
                                   THEN 'unpriced=' || (count_if(unpriced_all > 0)
                                                        + COALESCE(ANY_VALUE(u.exits_unpriced), 0)) END)
          FROM ks k
          LEFT JOIN u ON u.as_of = k.as_of
          LEFT JOIN um m ON m.as_of = k.as_of
         GROUP BY k.as_of)
    SELECT r.as_of, r.is_total, r.silver_source_id, r.account_external_id,
           a.display_name, a.nickname, r.instrument_external_id,
           CASE WHEN r.is_total THEN 'Total' ELSE COALESCE(il.name, il.symbol, r.instrument_external_id) END AS holding,
           c.asset_class, c.vehicle, r.status, r.holding_group,
           r.first_call,
           CAST(r.paid_in AS DECIMAL(28,4)) AS paid_in,
           CAST(r.commitment AS DECIMAL(28,4)) AS commitment,
           CAST(r.unfunded AS DECIMAL(28,4)) AS unfunded,
           CAST(r.distributed AS DECIMAL(28,4)) AS distributed,
           CAST(r.nav AS DECIMAL(28,4)) AS nav,
           r.marked_at, r.end_at,
           CASE WHEN r.dpi_paid > 0 THEN CAST(r.dpi_back / r.dpi_paid AS DOUBLE) END AS dpi,
           CASE WHEN r.tvpi_paid > 0 THEN CAST(r.tvpi_value / r.tvpi_paid AS DOUBLE) END AS tvpi,
           r.stated_tvpi, r.stated_dpi, r.stated_as_of,
           r.quality
      FROM pr r
      LEFT JOIN holdings c ON c.silver_source_id = r.silver_source_id AND c.account_external_id = r.account_external_id
           AND c.instrument_external_id = r.instrument_external_id
      LEFT JOIN accounts a ON a.silver_source_id = r.silver_source_id AND a.account_external_id = r.account_external_id
      LEFT JOIN instrument_labels() il ON il.silver_source_id = r.silver_source_id
           AND il.instrument_external_id = r.instrument_external_id
     ORDER BY r.as_of, r.is_total, r.silver_source_id, r.account_external_id, r.first_call, r.instrument_external_id
);

-- report_private_flows_multi: the private holdings' cash flows per
-- holding and calendar quarter at p_asof, in every reporting currency, a
-- row per currency, quarter and holding a leg falls in. The legs are
-- those report_private's total counts: the legs of the holdings it lists
-- and the exits in kind out of their groups that name no holding, which
-- a row per account and quarter marked unattributed holds, with no
-- holding, class or vehicle. It lists the same holdings in every
-- currency, as whether one is listed reads its own currency's money, so
-- the list is read once. called is the calls net of refunds, distributed
-- the cash and the exits in kind that came back, net what came back less
-- what was called. Each rounds once, to the cent, so the rows add up to
-- the total's figures but for the cents each row rounds. quarter is the
-- epoch second that opens the quarter, in UTC. quality names what a
-- figure misses: unpriced=N legs with no rate to the currency, which add
-- nothing, and exit_unvalued=N exits in kind that state no value.
CREATE MACRO report_private_flows_multi(p_asof) AS TABLE (
    WITH listed AS (
        SELECT silver_source_id, account_external_id, instrument_external_id, holding_group, holding,
               asset_class, vehicle
          FROM report_private([p_asof], 'USD')
         WHERE NOT is_total),
    legs AS (
        SELECT 'USD' AS out_ccy, * FROM private_legs([p_asof], 'USD')
        UNION ALL SELECT 'CHF', * FROM private_legs([p_asof], 'CHF')
        UNION ALL SELECT 'EUR', * FROM private_legs([p_asof], 'EUR')
        UNION ALL SELECT 'GBP', * FROM private_legs([p_asof], 'GBP')),
    counted AS (
        SELECT l.out_ccy, l.day, l.leg, l.paid, l.unvalued, l.unattributed, l.silver_source_id, l.account_external_id,
               k.instrument_external_id, k.holding, k.asset_class, k.vehicle
          FROM legs l
          SEMI JOIN listed a ON a.silver_source_id = l.silver_source_id AND a.holding_group = l.holding_group
          LEFT JOIN listed k ON NOT l.unattributed AND k.silver_source_id = l.silver_source_id
           AND k.account_external_id = l.account_external_id AND k.instrument_external_id = l.instrument_external_id
         WHERE l.unattributed OR k.instrument_external_id IS NOT NULL),
    q AS (
        SELECT out_ccy, CAST(epoch(date_trunc('quarter', epoch_ms(day * 86400000))) AS BIGINT) AS quarter,
               silver_source_id, account_external_id, instrument_external_id, holding, asset_class, vehicle,
               unattributed,
               ROUND(COALESCE(SUM(paid) FILTER (WHERE leg = 'call'), 0), 2) AS called,
               ROUND(COALESCE(-SUM(paid) FILTER (WHERE leg <> 'call'), 0), 2) AS distributed,
               COUNT(*) FILTER (WHERE paid IS NULL) AS unpriced,
               COUNT(*) FILTER (WHERE unvalued) AS unvalued
          FROM counted
         GROUP BY ALL)
    SELECT out_ccy AS currency, quarter, silver_source_id, account_external_id, instrument_external_id, holding,
           asset_class, vehicle, unattributed,
           CAST(called AS DECIMAL(28,4)) AS called,
           CAST(distributed AS DECIMAL(28,4)) AS distributed,
           CAST(distributed - called AS DECIMAL(28,4)) AS net,
           concat_ws(';', CASE WHEN unpriced > 0 THEN 'unpriced=' || unpriced END,
                          CASE WHEN unvalued > 0 THEN 'exit_unvalued=' || unvalued END) AS quality
      FROM q
     ORDER BY currency, quarter, silver_source_id, account_external_id, unattributed, instrument_external_id
);
