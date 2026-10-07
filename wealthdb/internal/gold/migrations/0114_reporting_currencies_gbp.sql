-- ============================================================
-- gold schema, migration 0114 —
--   one conversion helper for the reporting currencies, and GBP
--   as the fourth of them.
--
-- The `_multi` report macros value every line in each reporting
-- currency. Each spelled the conversion out itself: seven ASOF joins
-- onto fx_daily and one COALESCE per currency. That is now shared:
--
--   fx_reporting_legs     a currency's rate to each reporting
--                         currency, per day a rate is quoted;
--   fx_reporting_bridges  the CHF and USD rates to each reporting
--                         currency, per day a rate is quoted;
--   fx_reporting_value    one value in one reporting currency.
--
-- A macro joins the two views once each and calls the scalar macro
-- per currency. The conversion order is unchanged: the identity, the
-- direct rate, then through CHF, then through USD — the order the
-- single-currency macros use, so a `_multi` column still equals the
-- matching `-x` report. Each view is laid out on a grid of every day
-- one of its rates is quoted, so an ASOF lookup on the grid finds the
-- same rate an ASOF lookup on fx_daily did. The products keep their
-- left-to-right order, so the values are the same to the bit.
--
-- GBP is the fourth reporting currency. Every `_multi` macro and
-- every web_* serving view gains its `_gbp` columns beside the
-- `_eur` ones. The macros and views are re-issued from their latest
-- text with nothing else changed; the base-currency conversions keep
-- their own joins, the base being a per-row currency rather than a
-- reporting one.
--
-- CREATE OR REPLACE throughout keeps this replayable for the DDL-rerun
-- test (see gold.Migrate's REPLAY note).
-- ============================================================

CREATE OR REPLACE VIEW fx_reporting_legs AS
    WITH d AS MATERIALIZED (
        SELECT from_ccy, to_ccy, day, rate FROM fx_daily
         WHERE to_ccy IN ('USD', 'CHF', 'EUR', 'GBP')),
    grid AS (SELECT DISTINCT from_ccy, day FROM d)
    SELECT g.from_ccy, g.day,
           l_usd.rate AS to_usd, l_chf.rate AS to_chf,
           l_eur.rate AS to_eur, l_gbp.rate AS to_gbp
      FROM grid g
      ASOF LEFT JOIN d l_usd ON l_usd.from_ccy = g.from_ccy AND l_usd.to_ccy = 'USD' AND l_usd.day <= g.day
      ASOF LEFT JOIN d l_chf ON l_chf.from_ccy = g.from_ccy AND l_chf.to_ccy = 'CHF' AND l_chf.day <= g.day
      ASOF LEFT JOIN d l_eur ON l_eur.from_ccy = g.from_ccy AND l_eur.to_ccy = 'EUR' AND l_eur.day <= g.day
      ASOF LEFT JOIN d l_gbp ON l_gbp.from_ccy = g.from_ccy AND l_gbp.to_ccy = 'GBP' AND l_gbp.day <= g.day;

CREATE OR REPLACE VIEW fx_reporting_bridges AS
    WITH d AS MATERIALIZED (
        SELECT from_ccy, to_ccy, day, rate FROM fx_daily
         WHERE from_ccy IN ('CHF', 'USD') AND to_ccy IN ('USD', 'CHF', 'EUR', 'GBP')),
    grid AS (SELECT DISTINCT day FROM d)
    SELECT g.day,
           chf_usd.rate AS chf_usd, chf_eur.rate AS chf_eur, chf_gbp.rate AS chf_gbp,
           usd_chf.rate AS usd_chf, usd_eur.rate AS usd_eur, usd_gbp.rate AS usd_gbp
      FROM grid g
      ASOF LEFT JOIN d chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= g.day
      ASOF LEFT JOIN d chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= g.day
      ASOF LEFT JOIN d chf_gbp ON chf_gbp.from_ccy = 'CHF' AND chf_gbp.to_ccy = 'GBP' AND chf_gbp.day <= g.day
      ASOF LEFT JOIN d usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= g.day
      ASOF LEFT JOIN d usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= g.day
      ASOF LEFT JOIN d usd_gbp ON usd_gbp.from_ccy = 'USD' AND usd_gbp.to_ccy = 'GBP' AND usd_gbp.day <= g.day;

-- `p_amt` in reporting currency `p_tgt`, for a line in currency `p_ccy`.
-- `p_legs` and `p_bridges` are the line's rows of the two views above,
-- passed whole. A target has no bridge through itself, so its CASE arm
-- is NULL and that step drops out of the COALESCE.
CREATE OR REPLACE MACRO fx_reporting_value(p_amt, p_ccy, p_tgt, p_legs, p_bridges) AS
    COALESCE(CASE WHEN p_ccy = p_tgt THEN p_amt::DOUBLE END,
             p_amt::DOUBLE * CASE p_tgt WHEN 'USD' THEN p_legs.to_usd WHEN 'CHF' THEN p_legs.to_chf
                                        WHEN 'EUR' THEN p_legs.to_eur WHEN 'GBP' THEN p_legs.to_gbp END,
             p_amt::DOUBLE * p_legs.to_chf
                 * CASE p_tgt WHEN 'USD' THEN p_bridges.chf_usd WHEN 'EUR' THEN p_bridges.chf_eur
                              WHEN 'GBP' THEN p_bridges.chf_gbp END,
             p_amt::DOUBLE * p_legs.to_usd
                 * CASE p_tgt WHEN 'CHF' THEN p_bridges.usd_chf WHEN 'EUR' THEN p_bridges.usd_eur
                              WHEN 'GBP' THEN p_bridges.usd_gbp END);

CREATE OR REPLACE MACRO report_accounts_multi(p_asof) AS TABLE (
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

CREATE OR REPLACE MACRO report_global_multi(p_asof) AS TABLE (
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

CREATE OR REPLACE MACRO report_portfolios_multi(p_asof) AS TABLE (
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

CREATE OR REPLACE MACRO report_accounts_history_multi() AS TABLE (
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

CREATE OR REPLACE MACRO report_global_history_multi() AS TABLE (
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

CREATE OR REPLACE MACRO report_sources_multi(p_asof) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.is_cash, l.base_currency, l.base_val,
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
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve,
               CAST(CASE WHEN COALESCE(g.n_pos,0)=0 THEN 0 WHEN g.n_po_g=0 THEN NULL ELSE g.pos_g END AS DECIMAL(28,4)) AS pvg,
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_g=0 THEN NULL ELSE g.cash_g END AS DECIMAL(28,4)) AS cvg
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
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur,
           pvg AS positions_value_gbp, cvg AS cash_balance_gbp,
           CASE WHEN pvg IS NULL OR cvg IS NULL THEN NULL ELSE pvg + cvg END AS total_value_gbp
      FROM vals
     ORDER BY src
);

CREATE OR REPLACE MACRO report_positions_multi(p_asof) AS TABLE (
    WITH latest AS (
        SELECT silver_source_id, MAX(snapshot_at) AS snap
          FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    base AS (
        SELECT p.silver_source_id, p.snapshot_at, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name,
               p.asset_class, p.vehicle, p.currency, p.quantity, p.market_value
          FROM positions p
          JOIN latest l ON p.silver_source_id = l.silver_source_id AND p.snapshot_at = l.snap
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

CREATE OR REPLACE MACRO spending_lines_multi(p_from, p_to) AS TABLE (
    SELECT b.*,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS value_usd,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS value_chf,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS value_eur,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS value_gbp
      FROM spending_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = b.currency AND fxl.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (b.occurred_at // 86400)
);

CREATE OR REPLACE MACRO report_spending_summary_multi(p_from, p_to, p_period) AS TABLE (
    WITH agg AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COUNT(*) AS txn_count,
               COUNT(value_usd) AS n_usd, COUNT(value_chf) AS n_chf, COUNT(value_eur) AS n_eur, COUNT(value_gbp) AS n_gbp,
               SUM(CASE WHEN value_usd < 0 THEN -value_usd ELSE 0 END) AS spend_usd_raw,
               SUM(CASE WHEN value_chf < 0 THEN -value_chf ELSE 0 END) AS spend_chf_raw,
               SUM(CASE WHEN value_eur < 0 THEN -value_eur ELSE 0 END) AS spend_eur_raw,
               SUM(CASE WHEN value_gbp < 0 THEN -value_gbp ELSE 0 END) AS spend_gbp_raw,
               SUM(CASE WHEN value_usd > 0 THEN  value_usd ELSE 0 END) AS refunds_usd_raw,
               SUM(CASE WHEN value_chf > 0 THEN  value_chf ELSE 0 END) AS refunds_chf_raw,
               SUM(CASE WHEN value_eur > 0 THEN  value_eur ELSE 0 END) AS refunds_eur_raw,
               SUM(CASE WHEN value_gbp > 0 THEN  value_gbp ELSE 0 END) AS refunds_gbp_raw
          FROM spending_lines_multi(p_from, p_to)
         GROUP BY 1),
    vals AS (
        SELECT period_start, txn_count,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE spend_usd_raw   END AS DECIMAL(28,4)) AS spend_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE spend_chf_raw   END AS DECIMAL(28,4)) AS spend_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE spend_eur_raw   END AS DECIMAL(28,4)) AS spend_eur,
               CAST(CASE WHEN n_gbp = 0 THEN NULL ELSE spend_gbp_raw   END AS DECIMAL(28,4)) AS spend_gbp,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE refunds_usd_raw END AS DECIMAL(28,4)) AS refunds_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE refunds_chf_raw END AS DECIMAL(28,4)) AS refunds_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE refunds_eur_raw END AS DECIMAL(28,4)) AS refunds_eur,
               CAST(CASE WHEN n_gbp = 0 THEN NULL ELSE refunds_gbp_raw END AS DECIMAL(28,4)) AS refunds_gbp
          FROM agg)
    SELECT period_start, txn_count,
           spend_usd, spend_chf, spend_eur, spend_gbp,
           refunds_usd, refunds_chf, refunds_eur, refunds_gbp,
           CAST(spend_usd - refunds_usd AS DECIMAL(28,4)) AS net_spend_usd,
           CAST(spend_chf - refunds_chf AS DECIMAL(28,4)) AS net_spend_chf,
           CAST(spend_eur - refunds_eur AS DECIMAL(28,4)) AS net_spend_eur,
           CAST(spend_gbp - refunds_gbp AS DECIMAL(28,4)) AS net_spend_gbp
      FROM vals
     ORDER BY period_start
);

CREATE OR REPLACE MACRO report_transactions_multi(p_from, p_to) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.account_kind, a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description,
               sc.merchant_name, sc.spend_primary, sc.spend_detailed
          FROM transactions t
          LEFT JOIN accounts a ON t.silver_source_id = a.silver_source_id AND t.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON t.silver_source_id = i.silver_source_id AND t.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = t.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = t.instrument_external_id
          LEFT JOIN symbol_resolutions srn ON srn.silver_source_id = t.silver_source_id
               AND srn.lookup_kind = 'name' AND srn.lookup_value = t.description
          LEFT JOIN spend_txn_categories() sc ON sc.silver_source_id = t.silver_source_id
               AND sc.transaction_external_id = t.transaction_external_id
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.account_kind, b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS DECIMAL(28,4)) AS gross_amount, CAST(b.net_amount AS DECIMAL(28,4)) AS net_amount,
           CAST(b.quantity AS DECIMAL(28,8)) AS quantity, CAST(b.price AS DECIMAL(28,8)) AS price, b.description,
           b.merchant_name, b.spend_primary, b.spend_detailed,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS value_usd,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS value_chf,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS value_eur,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS value_gbp
      FROM base b
      ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = b.currency AND fxl.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (b.occurred_at // 86400)
     ORDER BY b.occurred_at, b.silver_source_id, b.transaction_external_id
);

CREATE OR REPLACE MACRO report_card_balances_history_multi() AS TABLE (
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

CREATE OR REPLACE MACRO report_positions_history_multi() AS TABLE (
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

CREATE OR REPLACE MACRO report_portfolios_history_multi() AS TABLE (
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

CREATE OR REPLACE MACRO report_spending_categories_multi(p_from, p_to, p_period, p_level) AS TABLE (
    WITH labelled AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COALESCE(CASE WHEN p_level = 'primary' THEN spend_primary ELSE spend_detailed END,
                        '(uncategorized)') AS category,
               COALESCE(CASE WHEN p_level = 'primary' THEN spend_primary_label ELSE spend_label END,
                        '(uncategorized)') AS category_label,
               value_usd, value_chf, value_eur, value_gbp
          FROM spending_lines_multi(p_from, p_to)),
    agg AS (
        SELECT period_start, category, category_label,
               COUNT(*) AS txn_count,
               COUNT(value_usd) AS n_usd, COUNT(value_chf) AS n_chf, COUNT(value_eur) AS n_eur, COUNT(value_gbp) AS n_gbp,
               SUM(CASE WHEN value_usd < 0 THEN -value_usd ELSE 0 END) AS spend_usd_raw,
               SUM(CASE WHEN value_chf < 0 THEN -value_chf ELSE 0 END) AS spend_chf_raw,
               SUM(CASE WHEN value_eur < 0 THEN -value_eur ELSE 0 END) AS spend_eur_raw,
               SUM(CASE WHEN value_gbp < 0 THEN -value_gbp ELSE 0 END) AS spend_gbp_raw,
               SUM(CASE WHEN value_usd > 0 THEN  value_usd ELSE 0 END) AS refunds_usd_raw,
               SUM(CASE WHEN value_chf > 0 THEN  value_chf ELSE 0 END) AS refunds_chf_raw,
               SUM(CASE WHEN value_eur > 0 THEN  value_eur ELSE 0 END) AS refunds_eur_raw,
               SUM(CASE WHEN value_gbp > 0 THEN  value_gbp ELSE 0 END) AS refunds_gbp_raw
          FROM labelled
         GROUP BY 1, 2, 3),
    vals AS (
        SELECT period_start, category, category_label, txn_count,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE spend_usd_raw   END AS DECIMAL(28,4)) AS spend_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE spend_chf_raw   END AS DECIMAL(28,4)) AS spend_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE spend_eur_raw   END AS DECIMAL(28,4)) AS spend_eur,
               CAST(CASE WHEN n_gbp = 0 THEN NULL ELSE spend_gbp_raw   END AS DECIMAL(28,4)) AS spend_gbp,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE refunds_usd_raw END AS DECIMAL(28,4)) AS refunds_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE refunds_chf_raw END AS DECIMAL(28,4)) AS refunds_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE refunds_eur_raw END AS DECIMAL(28,4)) AS refunds_eur,
               CAST(CASE WHEN n_gbp = 0 THEN NULL ELSE refunds_gbp_raw END AS DECIMAL(28,4)) AS refunds_gbp
          FROM agg),
    net AS (
        SELECT *,
               CAST(spend_usd - refunds_usd AS DECIMAL(28,4)) AS net_spend_usd,
               CAST(spend_chf - refunds_chf AS DECIMAL(28,4)) AS net_spend_chf,
               CAST(spend_eur - refunds_eur AS DECIMAL(28,4)) AS net_spend_eur,
               CAST(spend_gbp - refunds_gbp AS DECIMAL(28,4)) AS net_spend_gbp
          FROM vals)
    SELECT period_start, category, category_label, txn_count,
           spend_usd, spend_chf, spend_eur, spend_gbp,
           refunds_usd, refunds_chf, refunds_eur, refunds_gbp,
           net_spend_usd, net_spend_chf, net_spend_eur, net_spend_gbp,
           ABS(net_spend_usd)::DOUBLE
               / NULLIF(SUM(ABS(net_spend_usd)) OVER (PARTITION BY period_start), 0) AS share_usd,
           ABS(net_spend_chf)::DOUBLE
               / NULLIF(SUM(ABS(net_spend_chf)) OVER (PARTITION BY period_start), 0) AS share_chf,
           ABS(net_spend_eur)::DOUBLE
               / NULLIF(SUM(ABS(net_spend_eur)) OVER (PARTITION BY period_start), 0) AS share_eur,
           ABS(net_spend_gbp)::DOUBLE
               / NULLIF(SUM(ABS(net_spend_gbp)) OVER (PARTITION BY period_start), 0) AS share_gbp
      FROM net
     ORDER BY period_start, net_spend_usd DESC, category
);

CREATE OR REPLACE MACRO report_spending_transactions_multi(p_from, p_to) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, merchant_signature, merchant_name, spend_primary, spend_detailed,
           spend_label, spend_primary_label,
           provenance, provider_spend_detailed, provider_spend_label, currency,
           CAST(net_amount AS DECIMAL(28,4)) AS net_amount,
           description, counterparty,
           value_usd, value_chf, value_eur, value_gbp
      FROM spending_lines_multi(p_from, p_to)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

CREATE OR REPLACE MACRO income_lines_multi(p_from, p_to) AS TABLE (
    SELECT b.*,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS value_usd,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS value_chf,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS value_eur,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS value_gbp
      FROM income_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = b.currency AND fxl.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (b.occurred_at // 86400)
);

CREATE OR REPLACE MACRO report_income_transactions_multi(p_from, p_to) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, payer_signature, payer_name, income_primary, income_detailed,
           income_label, income_primary_label,
           provenance, provider_income_detailed, provider_income_label, currency,
           CAST(net_amount AS DECIMAL(28,4)) AS net_amount,
           description, counterparty,
           value_usd, value_chf, value_eur, value_gbp
      FROM income_lines_multi(p_from, p_to)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

CREATE OR REPLACE MACRO report_income_types_multi(p_from, p_to, p_period, p_level) AS TABLE (
    WITH labelled AS (
        SELECT spend_period_bucket(p_period, occurred_at) AS period_start,
               COALESCE(CASE WHEN p_level = 'primary' THEN income_primary ELSE income_detailed END,
                        '(uncategorized)') AS type,
               COALESCE(CASE WHEN p_level = 'primary' THEN income_primary_label ELSE income_label END,
                        '(uncategorized)') AS type_label,
               value_usd, value_chf, value_eur, value_gbp
          FROM income_lines_multi(p_from, p_to)),
    agg AS (
        SELECT period_start, type, type_label,
               COUNT(*)         AS txn_count,
               COUNT(value_usd) AS n_usd,
               COUNT(value_chf) AS n_chf,
               COUNT(value_eur) AS n_eur,
               COUNT(value_gbp) AS n_gbp,
               SUM(CASE WHEN value_usd > 0 THEN  value_usd ELSE 0 END) AS income_usd_raw,
               SUM(CASE WHEN value_chf > 0 THEN  value_chf ELSE 0 END) AS income_chf_raw,
               SUM(CASE WHEN value_eur > 0 THEN  value_eur ELSE 0 END) AS income_eur_raw,
               SUM(CASE WHEN value_gbp > 0 THEN  value_gbp ELSE 0 END) AS income_gbp_raw,
               SUM(CASE WHEN value_usd < 0 THEN -value_usd ELSE 0 END) AS reversals_usd_raw,
               SUM(CASE WHEN value_chf < 0 THEN -value_chf ELSE 0 END) AS reversals_chf_raw,
               SUM(CASE WHEN value_eur < 0 THEN -value_eur ELSE 0 END) AS reversals_eur_raw,
               SUM(CASE WHEN value_gbp < 0 THEN -value_gbp ELSE 0 END) AS reversals_gbp_raw
          FROM labelled
         GROUP BY 1, 2, 3),
    vals AS (
        SELECT period_start, type, type_label, txn_count,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE income_usd_raw    END AS DECIMAL(28,4)) AS income_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE income_chf_raw    END AS DECIMAL(28,4)) AS income_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE income_eur_raw    END AS DECIMAL(28,4)) AS income_eur,
               CAST(CASE WHEN n_gbp = 0 THEN NULL ELSE income_gbp_raw    END AS DECIMAL(28,4)) AS income_gbp,
               CAST(CASE WHEN n_usd = 0 THEN NULL ELSE reversals_usd_raw END AS DECIMAL(28,4)) AS reversals_usd,
               CAST(CASE WHEN n_chf = 0 THEN NULL ELSE reversals_chf_raw END AS DECIMAL(28,4)) AS reversals_chf,
               CAST(CASE WHEN n_eur = 0 THEN NULL ELSE reversals_eur_raw END AS DECIMAL(28,4)) AS reversals_eur,
               CAST(CASE WHEN n_gbp = 0 THEN NULL ELSE reversals_gbp_raw END AS DECIMAL(28,4)) AS reversals_gbp
          FROM agg),
    net AS (
        SELECT *,
               CAST(income_usd - reversals_usd AS DECIMAL(28,4)) AS net_income_usd,
               CAST(income_chf - reversals_chf AS DECIMAL(28,4)) AS net_income_chf,
               CAST(income_eur - reversals_eur AS DECIMAL(28,4)) AS net_income_eur,
               CAST(income_gbp - reversals_gbp AS DECIMAL(28,4)) AS net_income_gbp
          FROM vals)
    SELECT period_start, type, type_label, txn_count,
           income_usd, income_chf, income_eur, income_gbp,
           reversals_usd, reversals_chf, reversals_eur, reversals_gbp,
           net_income_usd, net_income_chf, net_income_eur, net_income_gbp,
           ABS(net_income_usd)::DOUBLE
               / NULLIF(SUM(ABS(net_income_usd)) OVER (PARTITION BY period_start), 0) AS share_usd,
           ABS(net_income_chf)::DOUBLE
               / NULLIF(SUM(ABS(net_income_chf)) OVER (PARTITION BY period_start), 0) AS share_chf,
           ABS(net_income_eur)::DOUBLE
               / NULLIF(SUM(ABS(net_income_eur)) OVER (PARTITION BY period_start), 0) AS share_eur,
           ABS(net_income_gbp)::DOUBLE
               / NULLIF(SUM(ABS(net_income_gbp)) OVER (PARTITION BY period_start), 0) AS share_gbp
      FROM net
     ORDER BY period_start, net_income_usd DESC, type
);

CREATE OR REPLACE MACRO cashflow_lines_multi(p_from, p_to) AS TABLE (
    SELECT b.*,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'USD', fxl, fxb) AS DECIMAL(28,4)) AS value_usd,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'CHF', fxl, fxb) AS DECIMAL(28,4)) AS value_chf,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'EUR', fxl, fxb) AS DECIMAL(28,4)) AS value_eur,
           CAST(fx_reporting_value(b.net_amount, b.currency, 'GBP', fxl, fxb) AS DECIMAL(28,4)) AS value_gbp
      FROM cashflow_lines_base(p_from, p_to) b
      ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = b.currency AND fxl.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (b.occurred_at // 86400)
);

CREATE OR REPLACE MACRO report_cashflow_transactions_multi(p_from, p_to) AS TABLE (
    SELECT silver_source_id, transaction_external_id, occurred_at,
           account_external_id, account_kind, display_name, nickname, account_category,
           kind, section, class, class_label, grp, group_label, name,
           verdict, spend_detailed, income_detailed, provenance, currency,
           CAST(net_amount AS DECIMAL(28,4)) AS net_amount,
           description, counterparty,
           value_usd, value_chf, value_eur, value_gbp
      FROM cashflow_lines_multi(p_from, p_to)
     ORDER BY occurred_at, silver_source_id, transaction_external_id
);

CREATE OR REPLACE VIEW web_sources_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id,
           positions_value_usd, cash_balance_usd, total_value_usd,
           positions_value_chf, cash_balance_chf, total_value_chf,
           positions_value_eur, cash_balance_eur, total_value_eur,
           positions_value_gbp, cash_balance_gbp, total_value_gbp
      FROM report_sources_history_multi();

CREATE OR REPLACE VIEW web_transactions AS
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           kind, silver_source_id, account_kind,
           value_usd, value_chf, value_eur, value_gbp
      FROM report_transactions_multi(0, 9223372036854775807);

CREATE OR REPLACE VIEW web_asset_classes_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, asset_class,
           SUM(value_usd) AS value_usd,
           SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur,
           SUM(value_gbp) AS value_gbp
      FROM report_positions_history_multi()
     GROUP BY 1, 2, 3
    UNION ALL
    SELECT epoch_ms(as_of_day * 1000),
           silver_source_id, 'cash',
           cash_balance_usd, cash_balance_chf, cash_balance_eur, cash_balance_gbp
      FROM report_sources_history_multi();

CREATE OR REPLACE VIEW web_vehicles_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, vehicle,
           SUM(value_usd) AS value_usd,
           SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur,
           SUM(value_gbp) AS value_gbp
      FROM report_positions_history_multi()
     GROUP BY 1, 2, 3
    UNION ALL
    SELECT epoch_ms(as_of_day * 1000),
           silver_source_id, 'demand_deposit',
           cash_balance_usd, cash_balance_chf, cash_balance_eur, cash_balance_gbp
      FROM report_sources_history_multi();

CREATE OR REPLACE VIEW web_accounts_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, management_style,
           SUM(total_value_usd) AS total_value_usd,
           SUM(total_value_chf) AS total_value_chf,
           SUM(total_value_eur) AS total_value_eur,
           SUM(total_value_gbp) AS total_value_gbp
      FROM report_accounts_history_multi()
     GROUP BY 1, 2, 3, 4;

CREATE OR REPLACE VIEW web_positions_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, symbol, name, asset_class, vehicle, currency,
           SUM(value_usd) AS value_usd,
           SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur,
           SUM(value_gbp) AS value_gbp
      FROM report_positions_history_multi()
     GROUP BY 1, 2, 3, 4, 5, 6, 7;

CREATE OR REPLACE VIEW web_spending AS
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, account_kind) AS account_label,
           account_kind,
           merchant_name,
           COALESCE(spend_primary,  '(uncategorized)') AS spend_primary,
           COALESCE(spend_detailed, '(uncategorized)') AS spend_detailed,
           COALESCE(spend_primary_label, '(uncategorized)') AS spend_primary_label,
           COALESCE(spend_label,         '(uncategorized)') AS spend_label,
           provider_spend_label,
           value_usd, value_chf, value_eur, value_gbp
      FROM report_spending_transactions_multi(0, 9223372036854775807);

CREATE OR REPLACE VIEW web_card_balances_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, 'card') AS account_label,
           currency,
           balance, balance_usd, balance_chf, balance_eur, balance_gbp
      FROM report_card_balances_history_multi();

CREATE OR REPLACE VIEW web_income AS
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, account_kind) AS account_label,
           account_kind,
           payer_name,
           COALESCE(income_primary,  '(uncategorized)') AS income_primary,
           COALESCE(income_detailed, '(uncategorized)') AS income_detailed,
           COALESCE(income_primary_label, '(uncategorized)') AS income_primary_label,
           COALESCE(income_label,         '(uncategorized)') AS income_label,
           provider_income_label,
           value_usd, value_chf, value_eur, value_gbp
      FROM report_income_transactions_multi(0, 9223372036854775807);

CREATE OR REPLACE VIEW web_cashflow AS
    WITH lines AS (
        SELECT t.*,
               CASE WHEN t.section = 'operating_in'  AND (t.grp = '(uncategorized)' OR sg.family = 'both')
                        THEN t.group_label || ' in'
                    WHEN t.section = 'operating_out' AND (t.grp = '(uncategorized)' OR sg.family = 'both')
                        THEN t.group_label || ' out'
                    ELSE t.group_label END AS group_node,
               CASE WHEN t.section = 'operating_in'  AND t.class = '(uncategorized)'
                        THEN t.class_label || ' in'
                    WHEN t.section = 'operating_out' AND t.class = '(uncategorized)'
                        THEN t.class_label || ' out'
                    ELSE t.class_label END AS class_node
          FROM report_cashflow_transactions_multi(0, 9223372036854775807) t
          LEFT JOIN spend_categories sg ON sg.spend_detailed = t.grp
    )
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, account_kind) AS account_label,
           account_kind, kind,
           section, class, class_label, class_node,
           grp, group_label, group_node,
           name,
           value_usd, value_chf, value_eur, value_gbp
      FROM lines;

CREATE OR REPLACE VIEW web_sources_latest AS
    SELECT epoch_ms(snapshot_at * 1000) AS snapshot_at,
           silver_source_id,
           positions_value_usd, cash_balance_usd, total_value_usd,
           positions_value_chf, cash_balance_chf, total_value_chf,
           positions_value_eur, cash_balance_eur, total_value_eur,
           positions_value_gbp, cash_balance_gbp, total_value_gbp
      FROM report_sources_multi(9223372036854775807);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (114, CAST(epoch(now()) AS BIGINT));
