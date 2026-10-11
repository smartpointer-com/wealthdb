-- ============================================================
-- gold schema, migration 0123 — a position's mark keeps its date when
-- the snapshot is dated later.
--
-- A portal that marks its holdings on event dates is read later. What
-- the read states is a roster: which holdings the portal still lists.
-- A holding it stopped listing ends at the read, so the adapters for
-- such portals (angellist, carta, equityzen; silver.Removals) emit a
-- snapshot at the dump time without it, each remaining position at the
-- mark it had. `positions.valued_at` is the instant that mark is from;
-- NULL means the snapshot's own instant, as it is for every source
-- that reads live values.
--
-- positions_at carries it, so report_positions (and report_cash, the
-- same shape) end in `valued_at`, and the sources reports gain
-- `valued_at` beside `snapshot_at`: the latest mark the source's
-- latest snapshot rides on. The Data Freshness dashboard counts a
-- source's quiet days from it.
--
-- OR REPLACE and IF NOT EXISTS keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

ALTER TABLE positions ADD COLUMN IF NOT EXISTS valued_at BIGINT;

-- positions_at: 0118's, carrying valued_at.
CREATE OR REPLACE MACRO positions_at(p_instants) AS TABLE (
    WITH b AS (SELECT DISTINCT unnest(p_instants) AS instant),
    snaps AS (SELECT DISTINCT silver_source_id, snapshot_at FROM positions),
    pick AS (
        SELECT b.instant, s.silver_source_id, MAX(s.snapshot_at) AS snapshot_at
          FROM b JOIN snaps s ON s.snapshot_at <= b.instant
         GROUP BY b.instant, s.silver_source_id)
    SELECT pk.instant, p.silver_source_id, p.snapshot_at, p.account_external_id,
           a.display_name, a.relationship_id, a.nickname, a.account_category,
           p.position_key, p.instrument_external_id, il.symbol, il.name,
           p.asset_class, p.vehicle, p.currency, p.quantity, p.market_value,
           p.book_value, p.accrued_interest, p.acquisition_date,
           p.basis_origin, p.basis_method, p.basis_fees,
           p.book_value_known, p.quantity_without_basis, p.open_lots, p.valued_at
      FROM positions p
      JOIN pick pk ON pk.silver_source_id = p.silver_source_id AND pk.snapshot_at = p.snapshot_at
      LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id AND a.account_external_id = p.account_external_id
      LEFT JOIN instrument_labels() il ON il.silver_source_id = p.silver_source_id
           AND il.instrument_external_id = p.instrument_external_id
);

-- report_positions / report_cash: 0118's, ending in valued_at.
CREATE OR REPLACE MACRO report_positions(p_asof, p_ccy, p_missing := 'ignore') AS TABLE (
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

CREATE OR REPLACE MACRO report_cash(p_asof, p_ccy) AS TABLE (
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

-- source_lines_base: 0025's, each line with the instant its value is from.
CREATE OR REPLACE MACRO source_lines_base(p_asof) AS TABLE (
    WITH lp AS (SELECT silver_source_id, MAX(snapshot_at) AS snap FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    lines AS (
        SELECT p.silver_source_id AS src, p.account_external_id AS acct, p.currency AS ccy, p.market_value AS amt, p.snapshot_at AS snap,
               COALESCE(p.valued_at, p.snapshot_at) AS valued, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.silver_source_id AND p.snapshot_at = lp.snap
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

-- report_sources / report_sources_multi: 0025's and 0114's, with the
-- latest such instant per source as valued_at.
CREATE OR REPLACE MACRO report_sources(p_asof, p_ccy) AS TABLE (
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

CREATE OR REPLACE MACRO report_sources_multi(p_asof) AS TABLE (
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

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (123, CAST(epoch(now()) AS BIGINT));
