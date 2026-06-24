-- The five report commands as parameterised DuckDB table macros, computed
-- entirely in SQL (FX conversion via the fx_daily view from migration 0020).
-- The Go *AsOf functions and the Metabase models both `SELECT * FROM
-- report_x(...)`, so CLI output == model output by construction. Conversion
-- is per-line at each line's own snapshot (flat nearest-rate, source
-- priority, CHF then USD triangulation); the COALESCE order picks
-- direct/reciprocal, else via-CHF, else via-USD.
--
-- Money columns are emitted as VARCHAR (CAST of DECIMAL(28,4)); the Go layer
-- trims scale-padding zeros (trimmedDecimalPtr), as it did for the old
-- CAST-to-VARCHAR query columns. Aggregate NULL-vs-0 semantics mirror the
-- old rollup.go: an empty bucket is 0; a bucket with lines but no FX path is
-- NULL; a base trio is NULL when the bucket's base currency is unknown.

-- cash_chosen: latest snapshot per source, one row per (account, currency)
-- by balance_kind precedence (current > closing > available > ...), non-zero.
CREATE OR REPLACE MACRO cash_chosen(p_asof) AS TABLE (
    WITH latest AS (
        SELECT silver_source_id, MAX(snapshot_at) AS snap
          FROM cash_balances WHERE snapshot_at <= p_asof GROUP BY 1),
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
          JOIN latest l ON cb.silver_source_id = l.silver_source_id AND cb.snapshot_at = l.snap)
    SELECT silver_source_id, account_external_id, currency, amount, snapshot_at
      FROM ranked WHERE rn = 1 AND amount <> 0
);

-- report_positions: latest snapshot per source, joined to accounts/
-- instruments/symbol_resolutions, market_value converted to p_ccy.
CREATE OR REPLACE MACRO report_positions(p_asof, p_ccy) AS TABLE (
    WITH latest AS (
        SELECT silver_source_id, MAX(snapshot_at) AS snap
          FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    base AS (
        SELECT p.silver_source_id, p.snapshot_at, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name,
               p.asset_class, p.currency, p.quantity, p.market_value
          FROM positions p
          JOIN latest l ON p.silver_source_id = l.silver_source_id AND p.snapshot_at = l.snap
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id)
    SELECT b.silver_source_id, b.snapshot_at, b.account_external_id, b.display_name,
           b.relationship_id, b.nickname, b.account_category, b.position_key,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.currency,
           CAST(b.quantity AS VARCHAR) AS quantity, CAST(b.market_value AS VARCHAR) AS market_value,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.market_value::DOUBLE END,
               b.market_value::DOUBLE * d.rate, b.market_value::DOUBLE * c1.rate * c2.rate,
               b.market_value::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy
      FROM base b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy = b.currency AND d.to_ccy = p_ccy AND d.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = p_ccy AND c2.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = p_ccy AND u2.day <= (b.snapshot_at // 86400)
     ORDER BY b.silver_source_id, b.account_external_id, b.position_key
);

-- report_cash: synthetic position-shaped rows for cash (same columns as
-- report_positions) so the Go scan + column registry are shared.
CREATE OR REPLACE MACRO report_cash(p_asof, p_ccy) AS TABLE (
    SELECT cc.silver_source_id, cc.snapshot_at, cc.account_external_id, a.display_name,
           a.relationship_id, a.nickname, a.account_category,
           'cash:' || cc.currency AS position_key, CAST(NULL AS VARCHAR) AS instrument_external_id,
           cc.currency AS symbol, 'Cash ' || cc.currency AS name, 'cash' AS asset_class, cc.currency,
           CAST(NULL AS VARCHAR) AS quantity, CAST(cc.amount AS VARCHAR) AS market_value,
           CAST(COALESCE(CASE WHEN cc.currency = p_ccy THEN cc.amount::DOUBLE END,
               cc.amount::DOUBLE * d.rate, cc.amount::DOUBLE * c1.rate * c2.rate,
               cc.amount::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy
      FROM cash_chosen(p_asof) cc
      LEFT JOIN accounts a ON cc.silver_source_id = a.silver_source_id AND cc.account_external_id = a.account_external_id
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy = cc.currency AND d.to_ccy = p_ccy AND d.day <= (cc.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = cc.currency AND c1.to_ccy = 'CHF' AND c1.day <= (cc.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = p_ccy AND c2.day <= (cc.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = cc.currency AND u1.to_ccy = 'USD' AND u1.day <= (cc.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = p_ccy AND u2.day <= (cc.snapshot_at // 86400)
     ORDER BY cc.silver_source_id, cc.account_external_id, cc.currency
);

-- report_transactions: every transaction in [p_from, p_to], net_amount
-- converted to p_ccy at occurred_at. Always ascending; the caller re-orders
-- for `-r`.
CREATE OR REPLACE MACRO report_transactions(p_from, p_to, p_ccy) AS TABLE (
    WITH base AS (
        SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at, t.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               t.instrument_external_id, COALESCE(i.symbol, sri.symbol, srn.symbol) AS symbol,
               i.name, i.asset_class, t.kind, t.currency,
               t.gross_amount, t.net_amount, t.quantity, t.price, t.description
          FROM transactions t
          LEFT JOIN accounts a ON t.silver_source_id = a.silver_source_id AND t.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON t.silver_source_id = i.silver_source_id AND t.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = t.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = t.instrument_external_id
          LEFT JOIN symbol_resolutions srn ON srn.silver_source_id = t.silver_source_id
               AND srn.lookup_kind = 'name' AND srn.lookup_value = t.description
         WHERE t.occurred_at BETWEEN p_from AND p_to)
    SELECT b.silver_source_id, b.transaction_external_id, b.occurred_at, b.account_external_id,
           b.display_name, b.relationship_id, b.nickname, b.account_category,
           b.instrument_external_id, b.symbol, b.name, b.asset_class, b.kind, b.currency,
           CAST(b.gross_amount AS VARCHAR) AS gross_amount, CAST(b.net_amount AS VARCHAR) AS net_amount,
           CAST(b.quantity AS VARCHAR) AS quantity, CAST(b.price AS VARCHAR) AS price, b.description,
           CAST(COALESCE(CASE WHEN b.currency = p_ccy THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * d.rate, b.net_amount::DOUBLE * c1.rate * c2.rate,
               b.net_amount::DOUBLE * u1.rate * u2.rate)::DECIMAL(28,4) AS VARCHAR) AS value_outccy
      FROM base b
      ASOF LEFT JOIN fx_daily d  ON d.from_ccy = b.currency AND d.to_ccy = p_ccy AND d.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c1 ON c1.from_ccy = b.currency AND c1.to_ccy = 'CHF' AND c1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily c2 ON c2.from_ccy = 'CHF' AND c2.to_ccy = p_ccy AND c2.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = b.currency AND u1.to_ccy = 'USD' AND u1.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = p_ccy AND u2.day <= (b.occurred_at // 86400)
     ORDER BY b.occurred_at, b.silver_source_id, b.transaction_external_id
);

-- account_lines: positions + cash for the latest snapshot per source, with
-- each line converted to BOTH the requested out-currency and a per-line
-- target currency (p_base_mode = 'account' uses the line's account base).
-- Shared by report_accounts / report_portfolios. Emits one row per line.
CREATE OR REPLACE MACRO account_line_values(p_asof, p_ccy) AS TABLE (
    WITH lp AS (SELECT silver_source_id, MAX(snapshot_at) AS snap FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    lines AS (
        SELECT p.silver_source_id AS src, p.account_external_id AS acct, p.currency AS ccy,
               p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.silver_source_id AND p.snapshot_at = lp.snap
        UNION ALL
        SELECT silver_source_id, account_external_id, currency, amount, snapshot_at, TRUE
          FROM cash_chosen(p_asof))
    SELECT l.src, l.acct, a.base_currency, l.snap, l.is_cash,
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

-- report_accounts: one row per gold account; positions + cash aggregated in
-- the account's own base currency and in p_ccy. Base trio NULL when base
-- unknown; empty account -> 0; lines-but-no-FX -> NULL.
CREATE OR REPLACE MACRO report_accounts(p_asof, p_ccy) AS TABLE (
    WITH conv AS (SELECT * FROM account_line_values(p_asof, p_ccy)),
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

-- report_portfolios: aggregate accounts by portfolio (orphans -> '' sentinel
-- per source, only where a source has orphans). Base/tax/management rolled
-- up from the portfolio's non-overlay accounts; portfolios.base_currency
-- wins when present.
CREATE OR REPLACE MACRO report_portfolios(p_asof, p_ccy) AS TABLE (
    WITH lp AS (SELECT silver_source_id, MAX(snapshot_at) AS snap FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
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
    -- one bucket per portfolios-table row + a sentinel '' per source with orphans
    buckets AS (
        SELECT silver_source_id AS src, portfolio_external_id AS pid, display_name, relationship_id, nickname, base_currency
          FROM portfolios
        UNION ALL
        SELECT DISTINCT src, '' AS pid, CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR)
          FROM acct_port WHERE pid = ''),
    bk AS (  -- bucket + effective base currency (portfolio wins, else rollup) + taxonomy
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

-- report_global: Σ over report_accounts (guarantees global == Σ accounts).
CREATE OR REPLACE MACRO report_global(p_asof, p_ccy) AS TABLE (
    SELECT COALESCE(MIN(snapshot_at) FILTER (WHERE snapshot_at > 0), 0) AS min_snapshot_at,
           COALESCE(MAX(snapshot_at) FILTER (WHERE snapshot_at > 0), 0) AS max_snapshot_at,
           CAST(COALESCE(SUM(cash_balance_outccy::DECIMAL(28,4)), 0) AS VARCHAR) AS cash_balance_outccy,
           CAST(COALESCE(SUM(positions_value_outccy::DECIMAL(28,4)), 0) AS VARCHAR) AS positions_value_outccy,
           CAST(COALESCE(SUM(total_value_outccy::DECIMAL(28,4)), 0) AS VARCHAR) AS total_value_outccy
      FROM report_accounts(p_asof, p_ccy)
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (21, CAST(epoch(now()) AS BIGINT));
