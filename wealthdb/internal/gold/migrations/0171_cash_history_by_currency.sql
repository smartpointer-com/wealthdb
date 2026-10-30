-- ============================================================
-- gold schema, migration 0171 — each account's cash per currency in
-- the serving history, so cash is a position with its own currency on
-- every Allocation card (web/DESIGN.md §6 and §8).
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
--
-- IF NOT EXISTS and OR REPLACE keep this replayable (see gold.Migrate's
-- REPLAY note). LIMIT 0 takes the macro's columns and computes nothing.
-- ============================================================

CREATE OR REPLACE MACRO report_cash_history_multi() AS TABLE (
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

CREATE TABLE IF NOT EXISTS serving_cash_history AS
    SELECT * FROM report_cash_history_multi() LIMIT 0;

-- holdings_lines_history: 0132's, the cash arm per currency.
CREATE OR REPLACE VIEW holdings_lines_history AS
    SELECT p.as_of_day, p.silver_source_id, p.account_external_id,
           COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
           COALESCE(a.management_style, 'self_directed') AS management_style,
           'position' AS line_kind, p.asset_class, p.vehicle, p.currency, p.symbol, p.name,
           p.value_usd, p.value_chf, p.value_eur, p.value_gbp
      FROM serving_positions_history p
      LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id
           AND a.account_external_id = p.account_external_id
    UNION ALL
    SELECT as_of_day, silver_source_id, account_external_id, tax_wrapper, management_style,
           'cash', 'cash', 'demand_deposit', currency, NULL, NULL,
           balance_usd, balance_chf, balance_eur, balance_gbp
      FROM serving_cash_history;

CREATE OR REPLACE VIEW web_positions_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper,
           CASE WHEN line_kind = 'cash' THEN to_json([silver_source_id, account_external_id])::VARCHAR END AS line_key,
           symbol,
           CASE WHEN line_kind = 'cash' THEN 'Cash ' || currency ELSE name END AS name,
           asset_class, vehicle, currency,
           SUM(value_usd) AS value_usd, SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur, SUM(value_gbp) AS value_gbp
      FROM holdings_lines_history
     GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9;

CREATE OR REPLACE VIEW web_currencies_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, asset_class, currency,
           SUM(value_usd) AS value_usd, SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur, SUM(value_gbp) AS value_gbp
      FROM holdings_lines_history
     WHERE currency IS NOT NULL
       AND COALESCE(value_usd, value_chf, value_eur, value_gbp) IS NOT NULL
     GROUP BY 1, 2, 3, 4, 5;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (171, CAST(epoch(now()) AS BIGINT));
