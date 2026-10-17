-- ============================================================
-- gold schema, migration 0132 — the serving views read tables, not the
-- report macros, and the Wealth Overview's and Allocation's carry the
-- tax wrapper and the asset class their pickers narrow to
-- (web/DESIGN.md §6 and §7).
--
-- A report macro computes its whole output on every read, whatever a
-- query asks for: a filter on the view cannot reach inside it. A
-- history macro computes every day from the first snapshot, for every
-- account and position, so an Allocation tile reading one day would pay
-- for the whole history; a flow macro computes every transaction line,
-- so a Cash Flow tile would pay for all of them. A dashboard fires its
-- tiles at once, into one memory pool. So `web-materialize` writes
-- the macros' output into these tables (gold.MaterializeServingTables)
-- before each snapshot, as it does for returns, gains and exposure, and
-- the views read them. Each table holds its macro's rows verbatim, so a
-- report model that wraps one keeps its shape.
--
-- The views sum the lines a day's net worth sums: each position, and
-- each account's cash as the class `cash` and the vehicle
-- `demand_deposit`. A source's positions, cash and total are the sums
-- of its lines, as report_sources_history_multi and report_sources_multi
-- give them; a line no rate converts adds nothing.
--
-- IF NOT EXISTS and OR REPLACE keep this replayable (see gold.Migrate's
-- REPLAY note). LIMIT 0 takes each macro's columns and computes nothing.
-- ============================================================

CREATE TABLE IF NOT EXISTS serving_sources_history AS
    SELECT * FROM report_sources_history_multi() LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_accounts_history AS
    SELECT * FROM report_accounts_history_multi() LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_positions_history AS
    SELECT * FROM report_positions_history_multi() LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_portfolios_history AS
    SELECT * FROM report_portfolios_history_multi() LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_global_history AS
    SELECT * FROM report_global_history_multi() LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_card_balances_history AS
    SELECT * FROM report_card_balances_history_multi() LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_global_latest AS
    SELECT * FROM report_global_multi(9223372036854775807) LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_sources_latest AS
    SELECT * FROM report_sources_multi(9223372036854775807) LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_portfolios_latest AS
    SELECT * FROM report_portfolios_multi(9223372036854775807) LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_accounts_latest AS
    SELECT * FROM report_accounts_multi(9223372036854775807) LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_positions_latest AS
    SELECT * FROM report_positions_multi(9223372036854775807) LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_transactions AS
    SELECT * FROM report_transactions_multi(0, 9223372036854775807) LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_spending_transactions AS
    SELECT * FROM report_spending_transactions_multi(0, 9223372036854775807) LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_income_transactions AS
    SELECT * FROM report_income_transactions_multi(0, 9223372036854775807) LIMIT 0;
CREATE TABLE IF NOT EXISTS serving_cashflow_transactions AS
    SELECT * FROM report_cashflow_transactions_multi(0, 9223372036854775807) LIMIT 0;

-- holdings_lines_history: every line of every day's net worth, at
-- account grain. An account's tax wrapper and management style take
-- the defaults report_accounts_history_multi gives them.
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
           'cash', 'cash', 'demand_deposit', NULL, NULL, NULL,
           cash_balance_usd, cash_balance_chf, cash_balance_eur, cash_balance_gbp
      FROM serving_accounts_history;

-- holdings_lines_latest: the same lines as of each source's latest
-- snapshot, the snapshot report_sources_multi dates the source by.
CREATE OR REPLACE VIEW holdings_lines_latest AS
    WITH l AS (
        SELECT p.silver_source_id,
               COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
               'position' AS line_kind, p.asset_class,
               p.value_usd, p.value_chf, p.value_eur, p.value_gbp
          FROM serving_positions_latest p
          LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id
               AND a.account_external_id = p.account_external_id
        UNION ALL
        SELECT silver_source_id, tax_wrapper, 'cash', 'cash',
               cash_balance_usd, cash_balance_chf, cash_balance_eur, cash_balance_gbp
          FROM serving_accounts_latest)
    SELECT s.snapshot_at, l.*
      FROM l
      JOIN (SELECT silver_source_id, MAX(snapshot_at) AS snapshot_at
              FROM serving_accounts_latest GROUP BY 1) s USING (silver_source_id);

CREATE OR REPLACE VIEW web_sources_latest AS
    SELECT epoch_ms(snapshot_at * 1000) AS snapshot_at,
           silver_source_id, tax_wrapper, asset_class,
           SUM(value_usd) FILTER (WHERE line_kind = 'position') AS positions_value_usd,
           SUM(value_usd) FILTER (WHERE line_kind = 'cash') AS cash_balance_usd,
           SUM(value_usd) AS total_value_usd,
           SUM(value_chf) FILTER (WHERE line_kind = 'position') AS positions_value_chf,
           SUM(value_chf) FILTER (WHERE line_kind = 'cash') AS cash_balance_chf,
           SUM(value_chf) AS total_value_chf,
           SUM(value_eur) FILTER (WHERE line_kind = 'position') AS positions_value_eur,
           SUM(value_eur) FILTER (WHERE line_kind = 'cash') AS cash_balance_eur,
           SUM(value_eur) AS total_value_eur,
           SUM(value_gbp) FILTER (WHERE line_kind = 'position') AS positions_value_gbp,
           SUM(value_gbp) FILTER (WHERE line_kind = 'cash') AS cash_balance_gbp,
           SUM(value_gbp) AS total_value_gbp
      FROM holdings_lines_latest
     GROUP BY 1, 2, 3, 4;

CREATE OR REPLACE VIEW web_sources_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, asset_class,
           SUM(value_usd) FILTER (WHERE line_kind = 'position') AS positions_value_usd,
           SUM(value_usd) FILTER (WHERE line_kind = 'cash') AS cash_balance_usd,
           SUM(value_usd) AS total_value_usd,
           SUM(value_chf) FILTER (WHERE line_kind = 'position') AS positions_value_chf,
           SUM(value_chf) FILTER (WHERE line_kind = 'cash') AS cash_balance_chf,
           SUM(value_chf) AS total_value_chf,
           SUM(value_eur) FILTER (WHERE line_kind = 'position') AS positions_value_eur,
           SUM(value_eur) FILTER (WHERE line_kind = 'cash') AS cash_balance_eur,
           SUM(value_eur) AS total_value_eur,
           SUM(value_gbp) FILTER (WHERE line_kind = 'position') AS positions_value_gbp,
           SUM(value_gbp) FILTER (WHERE line_kind = 'cash') AS cash_balance_gbp,
           SUM(value_gbp) AS total_value_gbp
      FROM holdings_lines_history
     GROUP BY 1, 2, 3, 4;

CREATE OR REPLACE VIEW web_asset_classes_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, asset_class,
           SUM(value_usd) AS value_usd, SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur, SUM(value_gbp) AS value_gbp
      FROM holdings_lines_history
     GROUP BY 1, 2, 3, 4;

CREATE OR REPLACE VIEW web_vehicles_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, asset_class, vehicle,
           SUM(value_usd) AS value_usd, SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur, SUM(value_gbp) AS value_gbp
      FROM holdings_lines_history
     GROUP BY 1, 2, 3, 4, 5;

CREATE OR REPLACE VIEW web_accounts_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, management_style, asset_class,
           SUM(value_usd) AS total_value_usd, SUM(value_chf) AS total_value_chf,
           SUM(value_eur) AS total_value_eur, SUM(value_gbp) AS total_value_gbp
      FROM holdings_lines_history
     GROUP BY 1, 2, 3, 4, 5;

CREATE OR REPLACE VIEW web_positions_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, symbol, name, asset_class, vehicle, currency,
           SUM(value_usd) AS value_usd, SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur, SUM(value_gbp) AS value_gbp
      FROM holdings_lines_history
     WHERE line_kind = 'position'
     GROUP BY 1, 2, 3, 4, 5, 6, 7, 8;

CREATE OR REPLACE VIEW web_card_balances_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, 'card') AS account_label,
           currency,
           balance, balance_usd, balance_chf, balance_eur, balance_gbp
      FROM serving_card_balances_history;

-- The two flow views the Wealth Overview reads gain the account's tax
-- wrapper, with the same default. A flow keeps no asset class here: a
-- fee or interest line often names no instrument.
CREATE OR REPLACE VIEW web_transactions AS
    SELECT epoch_ms(t.occurred_at * 1000) AS occurred_at,
           t.kind, t.silver_source_id, t.account_kind,
           COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
           t.value_usd, t.value_chf, t.value_eur, t.value_gbp
      FROM serving_transactions t
      LEFT JOIN accounts a ON a.silver_source_id = t.silver_source_id
           AND a.account_external_id = t.account_external_id;

CREATE OR REPLACE VIEW web_income AS
    SELECT epoch_ms(i.occurred_at * 1000) AS occurred_at,
           i.silver_source_id, i.account_external_id,
           account_display_name(i.display_name, i.account_external_id) AS display_name,
           account_label(i.display_name, i.account_external_id,
                         i.silver_source_id, i.account_kind) AS account_label,
           i.account_kind,
           COALESCE(a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
           i.payer_name,
           COALESCE(i.income_primary,  '(uncategorized)') AS income_primary,
           COALESCE(i.income_detailed, '(uncategorized)') AS income_detailed,
           COALESCE(i.income_primary_label, '(uncategorized)') AS income_primary_label,
           COALESCE(i.income_label,         '(uncategorized)') AS income_label,
           i.provider_income_label,
           i.value_usd, i.value_chf, i.value_eur, i.value_gbp
      FROM serving_income_transactions i
      LEFT JOIN accounts a ON a.silver_source_id = i.silver_source_id
           AND a.account_external_id = i.account_external_id;

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
      FROM serving_spending_transactions;

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
          FROM serving_cashflow_transactions t
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

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (132, CAST(epoch(now()) AS BIGINT));
