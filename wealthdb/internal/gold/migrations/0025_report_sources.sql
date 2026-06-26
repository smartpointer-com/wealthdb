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
-- report_sources_history / _history_multi (below) are the daily-series
-- variants for Metabase, the per-source analogues of
-- report_portfolios_history (migration 0022, refactored 0024).

-- source_buckets: one bucket per source (every source with at least one
-- account) carrying its rolled-up taxonomy + effective base currency. The
-- agree-or-NULL rollup over non-overlay accounts mirrors portfolio_buckets;
-- time- and currency-invariant, shared by the value macros below.
CREATE OR REPLACE MACRO source_buckets() AS TABLE (
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

-- source_lines_base: positions + cash for the latest snapshot per source,
-- bucketed by source, converted to the bucket's base currency. Lines are
-- filtered to (src, account) pairs present in `accounts` so the rollup
-- reconciles with report_accounts (which drops orphan-account lines at its
-- final join). Currency-agnostic out-conversion is layered by the callers.
CREATE OR REPLACE MACRO source_lines_base(p_asof) AS TABLE (
    WITH lp AS (SELECT silver_source_id, MAX(snapshot_at) AS snap FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    lines AS (
        SELECT p.silver_source_id AS src, p.account_external_id AS acct, p.currency AS ccy, p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.silver_source_id AND p.snapshot_at = lp.snap
        UNION ALL
        SELECT silver_source_id, account_external_id, currency, amount, snapshot_at, TRUE
          FROM cash_chosen(p_asof))
    SELECT l.src, l.ccy, l.amt, l.snap, l.is_cash, bk.base_currency,
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

-- report_sources: one row per silver source; positions + cash aggregated in
-- the source's rolled base currency and in p_ccy. Same NULL-vs-0 contract as
-- report_portfolios.
CREATE OR REPLACE MACRO report_sources(p_asof, p_ccy) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.is_cash, l.base_currency, l.base_val,
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
               MAX(snap) AS max_snap
          FROM conv GROUP BY 1),
    ssnap AS (SELECT src, MAX(snap) AS s FROM conv GROUP BY 1),
    vals AS (
        SELECT bk.src, bk.base_currency, bk.tax_wrapper, bk.management_style,
               COALESCE(g.max_snap, ss.s, 0) AS snapshot_at,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po=0 THEN NULL ELSE g.pos_o END AS DECIMAL(28,4)) AS pvo,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co=0 THEN NULL ELSE g.cash_o END AS DECIMAL(28,4)) AS cvo
          FROM source_buckets() bk
          LEFT JOIN agg g ON bk.src = g.src
          LEFT JOIN ssnap ss ON bk.src = ss.src)
    SELECT src AS silver_source_id, base_currency, tax_wrapper, management_style, snapshot_at,
           CAST(pvb AS VARCHAR) AS positions_value_base,
           CAST(cvb AS VARCHAR) AS cash_balance_base,
           CAST(CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS VARCHAR) AS total_value_base,
           CAST(pvo AS VARCHAR) AS positions_value_outccy,
           CAST(cvo AS VARCHAR) AS cash_balance_outccy,
           CAST(CASE WHEN pvo IS NULL OR cvo IS NULL THEN NULL ELSE pvo + cvo END AS VARCHAR) AS total_value_outccy
      FROM vals
     ORDER BY src
);

-- report_sources_multi: one row per source with positions/cash/total in
-- USD/CHF/EUR + the base trio. Metabase-facing (DECIMAL money columns). Keeps
-- the strict NULL-on-mixed taxonomy semantics (no display defaults), like
-- report_portfolios_multi.
CREATE OR REPLACE MACRO report_sources_multi(p_asof) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.is_cash, l.base_currency, l.base_val,
               CAST(COALESCE(CASE WHEN l.ccy = 'USD' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_usd.rate, l.amt::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS out_usd,
               CAST(COALESCE(CASE WHEN l.ccy = 'CHF' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_chf.rate, l.amt::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS out_chf,
               CAST(COALESCE(CASE WHEN l.ccy = 'EUR' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_eur.rate, l.amt::DOUBLE * p_chf.rate * chf_eur.rate,
                   l.amt::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS out_eur
          FROM source_lines_base(p_asof) l
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = l.ccy AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = l.ccy AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = l.ccy AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (l.snap // 86400)),
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
               MAX(snap) AS max_snap
          FROM conv GROUP BY 1),
    ssnap AS (SELECT src, MAX(snap) AS s FROM conv GROUP BY 1),
    vals AS (
        SELECT bk.src, bk.base_currency, bk.tax_wrapper, bk.management_style,
               COALESCE(g.max_snap, ss.s, 0) AS snapshot_at,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_u=0 THEN NULL ELSE g.pos_u END AS DECIMAL(28,4)) AS pvu,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_u=0 THEN NULL ELSE g.cash_u END AS DECIMAL(28,4)) AS cvu,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_c=0 THEN NULL ELSE g.pos_c END AS DECIMAL(28,4)) AS pvc,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_c=0 THEN NULL ELSE g.cash_c END AS DECIMAL(28,4)) AS cvc,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_e=0 THEN NULL ELSE g.pos_e END AS DECIMAL(28,4)) AS pve,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve
          FROM source_buckets() bk
          LEFT JOIN agg g ON bk.src = g.src
          LEFT JOIN ssnap ss ON bk.src = ss.src)
    SELECT src AS silver_source_id, base_currency, tax_wrapper, management_style, snapshot_at,
           pvb AS positions_value_base, cvb AS cash_balance_base,
           CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS total_value_base,
           pvu AS positions_value_usd, cvu AS cash_balance_usd,
           CASE WHEN pvu IS NULL OR cvu IS NULL THEN NULL ELSE pvu + cvu END AS total_value_usd,
           pvc AS positions_value_chf, cvc AS cash_balance_chf,
           CASE WHEN pvc IS NULL OR cvc IS NULL THEN NULL ELSE pvc + cvc END AS total_value_chf,
           pve AS positions_value_eur, cve AS cash_balance_eur,
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur
      FROM vals
     ORDER BY src
);

-- report_sources_history: report_sources per (source, day), value carried
-- forward — the per-silver-source analogue of report_portfolios_history
-- (migration 0022, refactored 0024). One row per source per UTC day from the
-- first snapshot to today; positions and cash carried independently via the
-- hist_active_pos / hist_active_cash spines; empty source-days omitted.
-- history@today equals report_sources(MAX, p_ccy). Taxonomy + rolled base
-- come from source_buckets() (time-invariant, like the portfolio version).
CREATE OR REPLACE MACRO report_sources_history(p_ccy) AS TABLE (
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
    acct_src AS (SELECT silver_source_id AS src, account_external_id AS acct FROM accounts),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p JOIN acct_src a ON a.src = p.silver_source_id AND a.acct = p.account_external_id
        UNION ALL
        SELECT cc.src, cc.snap, cc.ccy, cc.amt, TRUE
          FROM cash_all cc JOIN acct_src a ON a.src = cc.src AND a.acct = cc.acct),
    conv AS (
        SELECT l.src, l.snap, l.is_cash, bk.base_currency,
               CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate, l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val,
               CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
                   CAST(COALESCE(CASE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                       l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate, l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
          FROM lines l
          JOIN source_buckets() bk ON bk.src = l.src
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
        SELECT ap.day, c.src, c.is_cash, c.base_currency, c.out_val, c.base_val
          FROM conv c JOIN hist_active_pos() ap ON ap.src = c.src AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.is_cash, c.base_currency, c.out_val, c.base_val
          FROM conv c JOIN hist_active_cash() ac ON ac.src = c.src AND ac.snap = c.snap WHERE c.is_cash),
    agg AS (
        SELECT day, src,
               COUNT(*) FILTER (WHERE NOT is_cash) AS n_pos, COUNT(*) FILTER (WHERE is_cash) AS n_cash,
               COUNT(out_val) FILTER (WHERE NOT is_cash) AS n_po, COUNT(out_val) FILTER (WHERE is_cash) AS n_co,
               COUNT(base_val) FILTER (WHERE NOT is_cash) AS n_pb, COUNT(base_val) FILTER (WHERE is_cash) AS n_cb,
               CAST(SUM(out_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_o, CAST(SUM(out_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_o,
               CAST(SUM(base_val) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_b, CAST(SUM(base_val) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_b
          FROM daily GROUP BY 1, 2),
    vals AS (
        SELECT g.day, bk.src, bk.base_currency, bk.tax_wrapper, bk.management_style,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_pos=0 THEN 0 WHEN g.n_pb=0 THEN NULL ELSE g.pos_b END AS DECIMAL(28,4)) AS pvb,
               CAST(CASE WHEN bk.base_currency IS NULL THEN NULL WHEN g.n_cash=0 THEN 0 WHEN g.n_cb=0 THEN NULL ELSE g.cash_b END AS DECIMAL(28,4)) AS cvb,
               CAST(CASE WHEN g.n_pos=0 THEN 0 WHEN g.n_po=0 THEN NULL ELSE g.pos_o END AS DECIMAL(28,4)) AS pvo,
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co=0 THEN NULL ELSE g.cash_o END AS DECIMAL(28,4)) AS cvo
          FROM agg g JOIN source_buckets() bk ON bk.src = g.src)
    SELECT day * 86400 AS as_of_day, src AS silver_source_id, base_currency, tax_wrapper, management_style,
           CAST(pvb AS VARCHAR) AS positions_value_base,
           CAST(cvb AS VARCHAR) AS cash_balance_base,
           CAST(CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS VARCHAR) AS total_value_base,
           CAST(pvo AS VARCHAR) AS positions_value_outccy,
           CAST(cvo AS VARCHAR) AS cash_balance_outccy,
           CAST(CASE WHEN pvo IS NULL OR cvo IS NULL THEN NULL ELSE pvo + cvo END AS VARCHAR) AS total_value_outccy
      FROM vals
     ORDER BY as_of_day, src
);

-- report_sources_history_multi: per-source daily history in USD/CHF/EUR + the
-- base trio. Metabase-facing (DECIMAL money columns); strict NULL-on-mixed
-- taxonomy. Mirrors report_portfolios_history_multi bucketed by source.
CREATE OR REPLACE MACRO report_sources_history_multi() AS TABLE (
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
    acct_src AS (SELECT silver_source_id AS src, account_external_id AS acct FROM accounts),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p JOIN acct_src a ON a.src = p.silver_source_id AND a.acct = p.account_external_id
        UNION ALL
        SELECT cc.src, cc.snap, cc.ccy, cc.amt, TRUE
          FROM cash_all cc JOIN acct_src a ON a.src = cc.src AND a.acct = cc.acct),
    conv AS (
        SELECT l.src, l.snap, l.is_cash, bk.base_currency,
               CAST(COALESCE(CASE WHEN l.ccy = 'USD' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_usd.rate, l.amt::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS out_usd,
               CAST(COALESCE(CASE WHEN l.ccy = 'CHF' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_chf.rate, l.amt::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS out_chf,
               CAST(COALESCE(CASE WHEN l.ccy = 'EUR' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_eur.rate, l.amt::DOUBLE * p_chf.rate * chf_eur.rate,
                   l.amt::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS out_eur,
               CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
                   CAST(COALESCE(CASE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                       l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate, l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
          FROM lines l
          JOIN source_buckets() bk ON bk.src = l.src
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = l.ccy AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = l.ccy AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = l.ccy AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b0 ON b0.from_ccy = l.ccy AND b0.to_ccy = bk.base_currency AND b0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b1 ON b1.from_ccy = l.ccy AND b1.to_ccy = 'CHF' AND b1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b2 ON b2.from_ccy = 'CHF' AND b2.to_ccy = bk.base_currency AND b2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b3 ON b3.from_ccy = l.ccy AND b3.to_ccy = 'USD' AND b3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily b4 ON b4.from_ccy = 'USD' AND b4.to_ccy = bk.base_currency AND b4.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_pos() ap ON ap.src = c.src AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_cash() ac ON ac.src = c.src AND ac.snap = c.snap WHERE c.is_cash),
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
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e
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
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve
          FROM agg g JOIN source_buckets() bk ON bk.src = g.src)
    SELECT day * 86400 AS as_of_day, src AS silver_source_id, base_currency, tax_wrapper, management_style,
           pvb AS positions_value_base, cvb AS cash_balance_base,
           CASE WHEN pvb IS NULL OR cvb IS NULL THEN NULL ELSE pvb + cvb END AS total_value_base,
           pvu AS positions_value_usd, cvu AS cash_balance_usd,
           CASE WHEN pvu IS NULL OR cvu IS NULL THEN NULL ELSE pvu + cvu END AS total_value_usd,
           pvc AS positions_value_chf, cvc AS cash_balance_chf,
           CASE WHEN pvc IS NULL OR cvc IS NULL THEN NULL ELSE pvc + cvc END AS total_value_chf,
           pve AS positions_value_eur, cve AS cash_balance_eur,
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur
      FROM vals
     ORDER BY as_of_day, src
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (25, CAST(epoch(now()) AS BIGINT));
