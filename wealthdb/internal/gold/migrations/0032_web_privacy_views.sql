-- Serving views for the web (Metabase) privacy cards. Each renders a
-- report macro's epoch day as TIMESTAMP and reduces the output to the
-- grain and columns the privacy cards read — the taxonomy and accounts
-- views also group to their breakdown dimension and fold each source's
-- cash in as a class/vehicle of its own. They exist as
-- database VIEWS (rather than more Metabase models) because Metabase
-- syncs views like tables and assigns their columns field ids — which
-- is what lets the privacy dashboards' pickers (time range, as-of day,
-- source, taxonomy) land on native-SQL cards as field filters. The
-- cards then apply those filters to the normalization denominator as
-- well as to the charted rows, so percentages total 100 across
-- whatever subset of sources is selected. See web/DESIGN.md.

-- Daily per-source totals (the net-worth / cash-vs-positions series).
CREATE OR REPLACE VIEW web_sources_history AS
    SELECT CAST(to_timestamp(as_of_day) AS TIMESTAMP) AS as_of_day,
           silver_source_id,
           positions_value_usd, cash_balance_usd, total_value_usd,
           positions_value_chf, cash_balance_chf, total_value_chf,
           positions_value_eur, cash_balance_eur, total_value_eur
      FROM report_sources_history_multi();

-- Per-transaction converted values (the income / cost flow charts).
-- All kinds; the cards filter to the kind families they chart.
CREATE OR REPLACE VIEW web_transactions AS
    SELECT CAST(to_timestamp(occurred_at) AS TIMESTAMP) AS occurred_at,
           kind, silver_source_id,
           value_usd, value_chf, value_eur
      FROM report_transactions_multi(0, 9223372036854775807);

-- Daily value per asset class, with each source's cash balance folded
-- in as a 'cash' class — so the classes sum exactly to net worth
-- (liability classes stay negative). Money-market-fund positions
-- (asset_class = cash) and the cash balances land in the same class.
CREATE OR REPLACE VIEW web_asset_classes_history AS
    SELECT CAST(to_timestamp(as_of_day) AS TIMESTAMP) AS as_of_day,
           silver_source_id, asset_class,
           SUM(value_usd) AS value_usd,
           SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur
      FROM report_positions_history_multi()
     GROUP BY 1, 2, 3
    UNION ALL
    SELECT CAST(to_timestamp(as_of_day) AS TIMESTAMP),
           silver_source_id, 'cash',
           cash_balance_usd, cash_balance_chf, cash_balance_eur
      FROM report_sources_history_multi();

-- Daily value per vehicle (the 2-D wrapper dimension), cash folded in
-- as a 'demand_deposit' vehicle — the wrapper counterpart of
-- web_asset_classes_history, summing to net worth the same way.
CREATE OR REPLACE VIEW web_vehicles_history AS
    SELECT CAST(to_timestamp(as_of_day) AS TIMESTAMP) AS as_of_day,
           silver_source_id, vehicle,
           SUM(value_usd) AS value_usd,
           SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur
      FROM report_positions_history_multi()
     GROUP BY 1, 2, 3
    UNION ALL
    SELECT CAST(to_timestamp(as_of_day) AS TIMESTAMP),
           silver_source_id, 'demand_deposit',
           cash_balance_usd, cash_balance_chf, cash_balance_eur
      FROM report_sources_history_multi();

-- Daily account totals rolled up to the tax-wrapper / management-style
-- grain. Account identifiers and labels are dropped: the privacy cards
-- only break down by the two rollup dimensions.
CREATE OR REPLACE VIEW web_accounts_history AS
    SELECT CAST(to_timestamp(as_of_day) AS TIMESTAMP) AS as_of_day,
           silver_source_id, tax_wrapper, management_style,
           SUM(total_value_usd) AS total_value_usd,
           SUM(total_value_chf) AS total_value_chf,
           SUM(total_value_eur) AS total_value_eur
      FROM report_accounts_history_multi()
     GROUP BY 1, 2, 3, 4;

-- Daily position values at the instrument grain (accounts merged),
-- with the taxonomy and native currency for the breakdown cards and
-- the Top-positions widget's inline pickers.
CREATE OR REPLACE VIEW web_positions_history AS
    SELECT CAST(to_timestamp(as_of_day) AS TIMESTAMP) AS as_of_day,
           silver_source_id, symbol, name, asset_class, vehicle, currency,
           SUM(value_usd) AS value_usd,
           SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur
      FROM report_positions_history_multi()
     GROUP BY 1, 2, 3, 4, 5, 6, 7;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (32, CAST(epoch(now()) AS BIGINT));
