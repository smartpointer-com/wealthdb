-- Multi-currency report variants for Metabase, factored to avoid the
-- redundant work that calling report_x(...) once per currency would incur.
--
-- The expensive part of every aggregate report is currency-agnostic: scan
-- positions + dedup cash + convert each line to its account/portfolio BASE
-- currency. Only the out-currency conversion varies with the requested
-- currency. So we pull the currency-agnostic work into shared "lines base"
-- macros computed ONCE, and layer the (cheap, fx_daily-only) out conversion
-- on top — once per target for the single-currency CLI macros, three times
-- (USD/CHF/EUR) for the report_x_multi macros that back the Metabase models.
--
-- The single-currency macros (report_accounts / report_portfolios and their
-- _history variants) are REFACTORED onto the shared base here; their output
-- columns/values are unchanged (the Go scan + gold tests are unaffected).
-- The _multi macros are new and Metabase-only — they emit DECIMAL money
-- columns directly (no VARCHAR round-trip; the CLI's trailing-zero trimming
-- doesn't apply), so the Metabase wrapper only has to cast epoch -> TIMESTAMP.
--
-- Per-currency conversion shares pivot legs: a line's ccy->CHF and ccy->USD
-- rates feed every target, so we resolve ccy->{CHF,USD,EUR} once plus the
-- four cross legs CHF->{USD,EUR} and USD->{CHF,EUR}. The COALESCE order
-- (identity, direct, via-CHF, via-USD) matches the single-currency macros
-- exactly; the via-self legs they carry (e.g. USD->USD) are always-NULL and
-- are simply dropped here. Reporting currencies are USD/CHF/EUR — to change
-- the set, edit these macros (and web/provision.py) in a new migration.

-- ===========================================================================
-- LATEST: shared line base + refactored single + multi
-- ===========================================================================

-- account_lines_base: positions + cash for the latest snapshot per source,
-- one row per line, with the line converted to the account's BASE currency.
-- Currency-agnostic (the out-currency conversion is layered by the callers).
-- Replaces the extraction+base half of the old account_line_values macro.
CREATE OR REPLACE MACRO account_lines_base(p_asof) AS TABLE (
    WITH lp AS (SELECT silver_source_id, MAX(snapshot_at) AS snap FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    lines AS (
        SELECT p.silver_source_id AS src, p.account_external_id AS acct, p.currency AS ccy,
               p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.silver_source_id AND p.snapshot_at = lp.snap
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

DROP MACRO TABLE IF EXISTS account_line_values;

-- report_accounts: refactored onto account_lines_base. Output identical to
-- migration 0021 (one row per account; base trio NULL when base unknown;
-- empty account -> 0; lines-but-no-FX -> NULL).
CREATE OR REPLACE MACRO report_accounts(p_asof, p_ccy) AS TABLE (
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

-- report_accounts_multi: one row per account with positions/cash/total in
-- USD, CHF and EUR (each column == report_accounts(p_asof, <ccy>) for that
-- currency) plus the base trio. Money columns are DECIMAL for Metabase.
CREATE OR REPLACE MACRO report_accounts_multi(p_asof) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.acct, l.base_currency, l.snap, l.is_cash, l.base_val,
               CAST(COALESCE(CASE WHEN l.ccy = 'USD' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_usd.rate, l.amt::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS out_usd,
               CAST(COALESCE(CASE WHEN l.ccy = 'CHF' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_chf.rate, l.amt::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS out_chf,
               CAST(COALESCE(CASE WHEN l.ccy = 'EUR' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_eur.rate, l.amt::DOUBLE * p_chf.rate * chf_eur.rate,
                   l.amt::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS out_eur
          FROM account_lines_base(p_asof) l
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = l.ccy AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = l.ccy AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = l.ccy AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (l.snap // 86400)),
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
               MAX(snap) AS max_snap
          FROM conv GROUP BY 1, 2),
    ssnap AS (SELECT src, MAX(snap) AS s FROM conv GROUP BY 1),
    vals AS (
        SELECT a.silver_source_id, a.account_external_id, a.account_kind, a.display_name,
               a.base_currency, a.relationship_id, a.nickname, a.account_category,
               a.portfolio_external_id,
               -- account-level display defaults (every account is non-NULL, matching
               -- the `wealthdb accounts` CLI render); the raw accounts table keeps NULL.
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
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve
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
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur
      FROM vals
     ORDER BY silver_source_id, account_external_id
);

-- report_global_multi: whole-portfolio rollup in USD/CHF/EUR (== Σ report_accounts_multi).
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
           CAST(COALESCE(SUM(total_value_eur), 0) AS DECIMAL(28,4)) AS total_value_eur
      FROM report_accounts_multi(p_asof)
);

-- report_positions_multi: one row per held position with market value in
-- USD/CHF/EUR. Per-line (no aggregation); the instrument/symbol joins run
-- once and the three conversions share pivot legs.
CREATE OR REPLACE MACRO report_positions_multi(p_asof) AS TABLE (
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
           CAST(b.quantity AS DECIMAL(28,8)) AS quantity, CAST(b.market_value AS DECIMAL(28,4)) AS market_value,
           CAST(COALESCE(CASE WHEN b.currency = 'USD' THEN b.market_value::DOUBLE END,
               b.market_value::DOUBLE * p_usd.rate, b.market_value::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS value_usd,
           CAST(COALESCE(CASE WHEN b.currency = 'CHF' THEN b.market_value::DOUBLE END,
               b.market_value::DOUBLE * p_chf.rate, b.market_value::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS value_chf,
           CAST(COALESCE(CASE WHEN b.currency = 'EUR' THEN b.market_value::DOUBLE END,
               b.market_value::DOUBLE * p_eur.rate, b.market_value::DOUBLE * p_chf.rate * chf_eur.rate,
               b.market_value::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS value_eur
      FROM base b
      ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = b.currency AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = b.currency AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = b.currency AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (b.snapshot_at // 86400)
      ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (b.snapshot_at // 86400)
     ORDER BY b.silver_source_id, b.account_external_id, b.position_key
);

-- report_transactions_multi: every transaction in [p_from, p_to] with net
-- amount in USD/CHF/EUR at occurred_at.
CREATE OR REPLACE MACRO report_transactions_multi(p_from, p_to) AS TABLE (
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
           CAST(b.gross_amount AS DECIMAL(28,4)) AS gross_amount, CAST(b.net_amount AS DECIMAL(28,4)) AS net_amount,
           CAST(b.quantity AS DECIMAL(28,8)) AS quantity, CAST(b.price AS DECIMAL(28,8)) AS price, b.description,
           CAST(COALESCE(CASE WHEN b.currency = 'USD' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_usd.rate, b.net_amount::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS value_usd,
           CAST(COALESCE(CASE WHEN b.currency = 'CHF' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_chf.rate, b.net_amount::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS value_chf,
           CAST(COALESCE(CASE WHEN b.currency = 'EUR' THEN b.net_amount::DOUBLE END,
               b.net_amount::DOUBLE * p_eur.rate, b.net_amount::DOUBLE * p_chf.rate * chf_eur.rate,
               b.net_amount::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS value_eur
      FROM base b
      ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = b.currency AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = b.currency AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = b.currency AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (b.occurred_at // 86400)
      ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (b.occurred_at // 86400)
     ORDER BY b.occurred_at, b.silver_source_id, b.transaction_external_id
);

-- ===========================================================================
-- PORTFOLIOS: shared buckets/taxonomy + line base + refactored single + multi
-- ===========================================================================

-- portfolio_acct_map: account -> portfolio bucket id, routing orphans (an
-- account whose portfolio_external_id is set but absent from the portfolios
-- table) to the '' catch-all alongside the NULL-portfolio accounts (the
-- migration-0023 orphan fix). The single source of truth for the routing,
-- shared by every portfolio macro so it cannot drift between them.
CREATE OR REPLACE MACRO portfolio_acct_map() AS TABLE (
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
CREATE OR REPLACE MACRO portfolio_buckets() AS TABLE (
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

-- portfolio_lines_base: positions + cash for the latest snapshot per source,
-- bucketed by portfolio, converted to the bucket's base currency. Currency-
-- agnostic out-conversion is layered by the callers.
CREATE OR REPLACE MACRO portfolio_lines_base(p_asof) AS TABLE (
    WITH lp AS (SELECT silver_source_id, MAX(snapshot_at) AS snap FROM positions WHERE snapshot_at <= p_asof GROUP BY 1),
    acct_port AS (SELECT * FROM portfolio_acct_map()),
    lines AS (
        SELECT p.silver_source_id AS src, ap.pid, p.currency AS ccy, p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.silver_source_id AND p.snapshot_at = lp.snap
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

-- report_portfolios: refactored onto portfolio_buckets + portfolio_lines_base.
-- Output identical to migration 0021/0023.
CREATE OR REPLACE MACRO report_portfolios(p_asof, p_ccy) AS TABLE (
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

-- report_portfolios_multi: one row per portfolio with positions/cash/total in
-- USD/CHF/EUR + the base trio. Rollup taxonomy via portfolio_buckets.
CREATE OR REPLACE MACRO report_portfolios_multi(p_asof) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.pid, l.snap, l.is_cash, l.base_currency, l.base_val,
               CAST(COALESCE(CASE WHEN l.ccy = 'USD' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_usd.rate, l.amt::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS out_usd,
               CAST(COALESCE(CASE WHEN l.ccy = 'CHF' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_chf.rate, l.amt::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS out_chf,
               CAST(COALESCE(CASE WHEN l.ccy = 'EUR' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_eur.rate, l.amt::DOUBLE * p_chf.rate * chf_eur.rate,
                   l.amt::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS out_eur
          FROM portfolio_lines_base(p_asof) l
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = l.ccy AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = l.ccy AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = l.ccy AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (l.snap // 86400)),
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
               CAST(CASE WHEN COALESCE(g.n_cash,0)=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve
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
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur
      FROM vals
     ORDER BY src, pid
);

-- ===========================================================================
-- HISTORY: shared all-snapshot line base + refactored single + multi
-- ===========================================================================

-- hist_acct_lines_base: every (src, snapshot, account) line valued once to the
-- account's base currency (account_line_values across ALL snapshots, no latest
-- filter; currency-agnostic out conversion layered by callers).
CREATE OR REPLACE MACRO hist_acct_lines_base() AS TABLE (
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
    SELECT l.src, l.snap, l.acct, l.ccy, l.amt, l.is_cash, a.base_currency,
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

DROP MACRO TABLE IF EXISTS hist_acct_lines;

-- report_accounts_history: refactored onto hist_acct_lines_base. Output identical to 0022.
CREATE OR REPLACE MACRO report_accounts_history(p_ccy) AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.acct, l.is_cash, l.base_currency, l.base_val,
               CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
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

-- report_accounts_history_multi: per-account daily history in USD/CHF/EUR + base.
CREATE OR REPLACE MACRO report_accounts_history_multi() AS TABLE (
    WITH conv AS (
        SELECT l.src, l.snap, l.acct, l.is_cash, l.base_currency, l.base_val,
               CAST(COALESCE(CASE WHEN l.ccy = 'USD' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_usd.rate, l.amt::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS out_usd,
               CAST(COALESCE(CASE WHEN l.ccy = 'CHF' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_chf.rate, l.amt::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS out_chf,
               CAST(COALESCE(CASE WHEN l.ccy = 'EUR' THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * p_eur.rate, l.amt::DOUBLE * p_chf.rate * chf_eur.rate,
                   l.amt::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS out_eur
          FROM hist_acct_lines_base() l
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = l.ccy AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = l.ccy AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = l.ccy AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (l.snap // 86400)),
    daily AS (
        SELECT ap.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_pos() ap ON ap.src = c.src AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.acct, c.base_currency, c.is_cash, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_cash() ac ON ac.src = c.src AND ac.snap = c.snap WHERE c.is_cash),
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
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e
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
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve
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
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur
      FROM vals
     ORDER BY as_of_day, silver_source_id, account_external_id
);

-- report_global_history_multi: Σ report_accounts_history_multi per day, in USD/CHF/EUR.
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
           CAST(COALESCE(SUM(total_value_eur), 0) AS DECIMAL(28,4)) AS total_value_eur
      FROM report_accounts_history_multi()
     GROUP BY as_of_day
     ORDER BY as_of_day
);

-- report_positions_history_multi: per-position daily history in USD/CHF/EUR.
CREATE OR REPLACE MACRO report_positions_history_multi() AS TABLE (
    WITH pv AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, p.account_external_id,
               a.display_name, a.relationship_id, a.nickname, a.account_category,
               p.position_key, p.instrument_external_id,
               COALESCE(i.symbol, sri.symbol) AS symbol, i.name, p.asset_class, p.currency,
               CAST(p.quantity AS DECIMAL(28,8)) AS quantity, CAST(p.market_value AS DECIMAL(28,4)) AS market_value,
               CAST(COALESCE(CASE WHEN p.currency = 'USD' THEN p.market_value::DOUBLE END,
                   p.market_value::DOUBLE * p_usd.rate, p.market_value::DOUBLE * p_chf.rate * chf_usd.rate) AS DECIMAL(28,4)) AS value_usd,
               CAST(COALESCE(CASE WHEN p.currency = 'CHF' THEN p.market_value::DOUBLE END,
                   p.market_value::DOUBLE * p_chf.rate, p.market_value::DOUBLE * p_usd.rate * usd_chf.rate) AS DECIMAL(28,4)) AS value_chf,
               CAST(COALESCE(CASE WHEN p.currency = 'EUR' THEN p.market_value::DOUBLE END,
                   p.market_value::DOUBLE * p_eur.rate, p.market_value::DOUBLE * p_chf.rate * chf_eur.rate,
                   p.market_value::DOUBLE * p_usd.rate * usd_eur.rate) AS DECIMAL(28,4)) AS value_eur
          FROM positions p
          LEFT JOIN accounts a ON p.silver_source_id = a.silver_source_id AND p.account_external_id = a.account_external_id
          LEFT JOIN instruments i ON p.silver_source_id = i.silver_source_id AND p.instrument_external_id = i.instrument_external_id
          LEFT JOIN symbol_resolutions sri ON sri.silver_source_id = p.silver_source_id
               AND sri.lookup_kind = 'instrument_external_id' AND sri.lookup_value = p.instrument_external_id
          ASOF LEFT JOIN fx_daily p_chf   ON p_chf.from_ccy   = p.currency AND p_chf.to_ccy   = 'CHF' AND p_chf.day   <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily p_usd   ON p_usd.from_ccy   = p.currency AND p_usd.to_ccy   = 'USD' AND p_usd.day   <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily p_eur   ON p_eur.from_ccy   = p.currency AND p_eur.to_ccy   = 'EUR' AND p_eur.day   <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily chf_usd ON chf_usd.from_ccy = 'CHF' AND chf_usd.to_ccy = 'USD' AND chf_usd.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily chf_eur ON chf_eur.from_ccy = 'CHF' AND chf_eur.to_ccy = 'EUR' AND chf_eur.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily usd_chf ON usd_chf.from_ccy = 'USD' AND usd_chf.to_ccy = 'CHF' AND usd_chf.day <= (p.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily usd_eur ON usd_eur.from_ccy = 'USD' AND usd_eur.to_ccy = 'EUR' AND usd_eur.day <= (p.snapshot_at // 86400))
    SELECT ap.day * 86400 AS as_of_day, pv.src AS silver_source_id, pv.snap AS snapshot_at,
           pv.account_external_id, pv.display_name, pv.relationship_id, pv.nickname, pv.account_category,
           pv.position_key, pv.instrument_external_id, pv.symbol, pv.name, pv.asset_class, pv.currency,
           pv.quantity, pv.market_value, pv.value_usd, pv.value_chf, pv.value_eur
      FROM pv JOIN hist_active_pos() ap ON ap.src = pv.src AND ap.snap = pv.snap
     ORDER BY as_of_day, pv.src, pv.account_external_id, pv.position_key
);

-- report_portfolios_history: refactored onto portfolio_buckets. Output identical to 0022/0023.
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
    acct_port AS (SELECT * FROM portfolio_acct_map()),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, ap.pid, p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p JOIN acct_port ap ON ap.src = p.silver_source_id AND ap.acct = p.account_external_id
        UNION ALL
        SELECT cc.src, cc.snap, ap.pid, cc.ccy, cc.amt, TRUE
          FROM cash_all cc JOIN acct_port ap ON ap.src = cc.src AND ap.acct = cc.acct),
    conv AS (
        SELECT l.src, l.snap, l.pid, l.is_cash, bk.base_currency, bk.tax_wrapper, bk.management_style,
               bk.display_name, bk.relationship_id, bk.nickname,
               CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate, l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS out_val,
               CASE WHEN bk.base_currency IS NULL THEN NULL ELSE
                   CAST(COALESCE(CASE WHEN l.ccy = bk.base_currency THEN l.amt::DOUBLE END,
                       l.amt::DOUBLE * b0.rate, l.amt::DOUBLE * b1.rate * b2.rate, l.amt::DOUBLE * b3.rate * b4.rate) AS DECIMAL(28,4)) END AS base_val
          FROM lines l
          JOIN portfolio_buckets() bk ON bk.src = l.src AND bk.pid = l.pid
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
          FROM agg g JOIN portfolio_buckets() bk ON bk.src = g.src AND bk.pid = g.pid)
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

-- report_portfolios_history_multi: per-portfolio daily history in USD/CHF/EUR + base.
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
        WHERE rn = 1 AND amt <> 0),
    acct_port AS (SELECT * FROM portfolio_acct_map()),
    lines AS (
        SELECT p.silver_source_id AS src, p.snapshot_at AS snap, ap.pid, p.currency AS ccy, p.market_value AS amt, FALSE AS is_cash
          FROM positions p JOIN acct_port ap ON ap.src = p.silver_source_id AND ap.acct = p.account_external_id
        UNION ALL
        SELECT cc.src, cc.snap, ap.pid, cc.ccy, cc.amt, TRUE
          FROM cash_all cc JOIN acct_port ap ON ap.src = cc.src AND ap.acct = cc.acct),
    conv AS (
        SELECT l.src, l.snap, l.pid, l.is_cash, bk.base_currency,
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
          JOIN portfolio_buckets() bk ON bk.src = l.src AND bk.pid = l.pid
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
        SELECT ap.day, c.src, c.pid, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_pos() ap ON ap.src = c.src AND ap.snap = c.snap WHERE NOT c.is_cash
        UNION ALL
        SELECT ac.day, c.src, c.pid, c.is_cash, c.base_currency, c.out_usd, c.out_chf, c.out_eur, c.base_val
          FROM conv c JOIN hist_active_cash() ac ON ac.src = c.src AND ac.snap = c.snap WHERE c.is_cash),
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
               CAST(SUM(out_eur) FILTER (WHERE NOT is_cash) AS DECIMAL(28,4)) AS pos_e, CAST(SUM(out_eur) FILTER (WHERE is_cash) AS DECIMAL(28,4)) AS cash_e
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
               CAST(CASE WHEN g.n_cash=0 THEN 0 WHEN g.n_co_e=0 THEN NULL ELSE g.cash_e END AS DECIMAL(28,4)) AS cve
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
           CASE WHEN pve IS NULL OR cve IS NULL THEN NULL ELSE pve + cve END AS total_value_eur
      FROM vals
     ORDER BY as_of_day, src, pid
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (24, CAST(epoch(now()) AS BIGINT));
