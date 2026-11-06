-- ============================================================
-- gold schema, migration 0177 — the net worth bridge, fees by portfolio
-- and mark age (docs/RETURNS-NOTES.md, "The net worth bridge";
-- docs/SPENDING.md §7; docs/DESIGN.md §4.7).
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
--
-- IF NOT EXISTS and OR REPLACE keep this replayable (see gold.Migrate's
-- REPLAY note). LIMIT 0 takes the macro's columns and computes nothing.
-- ============================================================

-- report_debt_history: one row per day, liability account and currency:
-- the balance as the daily history holds it, negative when owed. It
-- reads the lines and the active snapshots report_accounts_history
-- reads, so the two carry and end an account alike. A mortgage is a
-- position and a card is cash, and this sums both.
CREATE OR REPLACE MACRO report_debt_history() AS TABLE (
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
CREATE OR REPLACE MACRO report_debt_fx(p_ccy) AS TABLE (
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

-- report_bridge: the net worth bridge by month, per reporting currency.
-- Money is in cents' precision, as the rows of `wealthdb returns bridge
-- --period monthly` print it, so a window's months sum to the same
-- figures. Days are epoch seconds. quality is `;`-separated.
CREATE TABLE IF NOT EXISTS report_bridge (
    computed_at      BIGINT        NOT NULL,
    currency         TEXT          NOT NULL,
    period           TEXT          NOT NULL,
    start_day        BIGINT        NOT NULL,
    end_day          BIGINT        NOT NULL,
    start_net_worth  DECIMAL(24,2) NOT NULL,
    end_net_worth    DECIMAL(24,2) NOT NULL,
    net_flow         DECIMAL(24,2) NOT NULL,
    onboarded        DECIMAL(24,2) NOT NULL,
    debt_repaid      DECIMAL(24,2) NOT NULL,
    net_saved        DECIMAL(24,2) NOT NULL,
    gain             DECIMAL(24,2) NOT NULL,
    bridge_gap       DECIMAL(24,2) NOT NULL,
    quality          TEXT          NOT NULL,
    PRIMARY KEY (currency, period)
);

-- web_bridge: the time range lands on month_end, so a picked window
-- takes the whole months that end inside it.
CREATE OR REPLACE VIEW web_bridge AS
    SELECT epoch_ms(end_day * 1000) AS month_end, currency, period,
           start_day, end_day, start_net_worth, end_net_worth, net_flow, onboarded,
           debt_repaid, net_saved, gain, quality
      FROM report_bridge;

-- fee_group: a BANK_FEES detailed value's group, the fee view's split.
CREATE OR REPLACE MACRO fee_group(p_detailed) AS
    CASE WHEN p_detailed = 'BANK_FEES_INVESTMENT_FEES' THEN 'investment'
         WHEN p_detailed = 'BANK_FEES_ATM_FEES' THEN 'atm'
         WHEN p_detailed IN ('BANK_FEES_OTHER_BANK_FEES', 'BANK_FEES_OVERDRAFT_FEES',
                             'BANK_FEES_INSUFFICIENT_FUNDS') THEN 'bank'
         ELSE 'other' END;

-- web_fees: what the fee tiles read, at day grain, so a picked window
-- cuts it where it cuts the other Spending tiles.
--   * line 'fee': a spending line of primary BANK_FEES, its value turned
--     to a cost. A refund is negative. fee_group is its detailed group.
--   * line 'value': an account's value on a day.
--   * line 'return': the entity's return for the month that ends on the
--     day, in `currency`, repeated on each of its accounts so an account
--     picker keeps it.
-- The entity is the account's portfolio, else the account.
CREATE OR REPLACE VIEW web_fees AS
    WITH acct AS (
        SELECT a.silver_source_id, a.account_external_id, a.account_kind,
               account_label(a.display_name, a.account_external_id,
                             a.silver_source_id, a.account_kind) AS account_label,
               account_display_name(a.display_name, a.account_external_id) AS display_name,
               NULLIF(a.portfolio_external_id, '') AS portfolio
          FROM accounts a),
    entity AS (
        SELECT c.*,
               to_json([c.silver_source_id, COALESCE(c.portfolio, c.account_external_id)])::VARCHAR AS entity_key,
               CASE WHEN c.portfolio IS NOT NULL THEN 'portfolio' ELSE 'account' END AS entity_kind,
               COALESCE(p.display_name, c.portfolio, c.display_name) AS entity
          FROM acct c
          LEFT JOIN portfolios p ON p.silver_source_id = c.silver_source_id
               AND p.portfolio_external_id = c.portfolio)
    SELECT epoch_ms(t.occurred_at * 1000) AS day, 'fee' AS line,
           e.silver_source_id, e.account_label, e.entity_key, e.entity_kind, e.entity,
           fee_group(t.spend_detailed) AS fee_group, CAST(NULL AS TEXT) AS currency,
           -t.value_usd AS value_usd, -t.value_chf AS value_chf,
           -t.value_eur AS value_eur, -t.value_gbp AS value_gbp,
           CAST(NULL AS DOUBLE) AS twr
      FROM serving_spending_transactions t
      JOIN entity e ON e.silver_source_id = t.silver_source_id
       AND e.account_external_id = t.account_external_id
     WHERE t.spend_primary = 'BANK_FEES'
    UNION ALL
    SELECT epoch_ms(h.as_of_day * 1000), 'value',
           e.silver_source_id, e.account_label, e.entity_key, e.entity_kind, e.entity,
           NULL, NULL,
           h.total_value_usd, h.total_value_chf, h.total_value_eur, h.total_value_gbp,
           NULL
      FROM serving_accounts_history h
      JOIN entity e ON e.silver_source_id = h.silver_source_id
       AND e.account_external_id = h.account_external_id
    UNION ALL
    SELECT epoch_ms(r.end_day * 1000), 'return',
           e.silver_source_id, e.account_label, e.entity_key, e.entity_kind, e.entity,
           NULL, r.currency, NULL, NULL, NULL, NULL, r.twr
      FROM report_returns r
      JOIN entity e ON e.silver_source_id = r.silver_source_id
       AND ((r.grain = 'portfolios' AND r.entity_id = e.portfolio)
         OR (r.grain = 'accounts' AND e.portfolio IS NULL AND r.entity_id = e.account_external_id))
     WHERE r.granularity = 'monthly' AND r.window_from_year = 0 AND NOT r.is_summary;

-- mark_month_ends: each month's last day inside gold's history.
CREATE OR REPLACE MACRO mark_month_ends() AS TABLE (
    SELECT day FROM hist_days()
     WHERE DATE '1970-01-01' + CAST(day AS INTEGER) = last_day(DATE '1970-01-01' + CAST(day AS INTEGER))
);

-- report_marks: per month end, the value of the positions on the daily
-- history that day, and the part marked within five days. A line's age
-- is the month end less coalesce(valued_at, snapshot_at), in days.
-- Liabilities and cash are out, and so is a line of no value. A line is
-- valued as the daily history values it, at its snapshot's rate.
-- marked_pct weighs by USD value whatever p_ccy is, so the figure has
-- one definition. A month end with no line has a NULL marked_pct and
-- says so in quality, and a line with no rate to USD or p_ccy leaves the
-- sums and is counted there.
CREATE OR REPLACE MACRO report_marks(p_ccy) AS TABLE (
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
               fx_reporting_value(l.market_value, l.currency, 'USD', fxl, fxb) AS usd,
               COALESCE(CASE WHEN l.currency = p_ccy THEN l.market_value::DOUBLE END,
                        l.market_value::DOUBLE * o0.rate, l.market_value::DOUBLE * o1.rate * o2.rate,
                        l.market_value::DOUBLE * o3.rate * o4.rate) AS out_val
          FROM lines l
          ASOF LEFT JOIN fx_reporting_legs fxl    ON fxl.from_ccy = l.currency AND fxl.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_reporting_bridges fxb ON fxb.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.currency AND o0.to_ccy = p_ccy AND o0.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.currency AND o1.to_ccy = 'CHF' AND o1.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.currency AND o3.to_ccy = 'USD' AND o3.day <= (l.snapshot_at // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snapshot_at // 86400))
    SELECT e.day * 86400 AS month_end,
           100 * COALESCE(SUM(v.usd) FILTER (WHERE v.marked), 0) / NULLIF(SUM(v.usd), 0) AS marked_pct,
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
CREATE OR REPLACE MACRO report_marks_multi() AS TABLE (
    SELECT 'USD' AS currency, * FROM report_marks('USD')
    UNION ALL SELECT 'CHF', * FROM report_marks('CHF')
    UNION ALL SELECT 'EUR', * FROM report_marks('EUR')
    UNION ALL SELECT 'GBP', * FROM report_marks('GBP')
);

CREATE TABLE IF NOT EXISTS serving_marks AS SELECT * FROM report_marks_multi() LIMIT 0;

-- web_marks: the Mark age tile reads marked_pct, one bar per month end,
-- so it takes the USD rows: the figure is the same in every currency.
CREATE OR REPLACE VIEW web_marks AS
    SELECT epoch_ms(month_end * 1000) AS month_end, marked_pct
      FROM serving_marks
     WHERE currency = 'USD';

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (177, CAST(epoch(now()) AS BIGINT));
