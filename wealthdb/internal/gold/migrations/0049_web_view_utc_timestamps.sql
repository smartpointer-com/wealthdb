-- The serving views' epoch columns land in UTC, whatever the session's
-- time zone is.
--
-- Every web_* view renders a report macro's epoch column as a
-- TIMESTAMP. `CAST(to_timestamp(x) AS TIMESTAMP)` does that in two
-- steps: to_timestamp yields TIMESTAMP WITH TIME ZONE, and the cast to
-- TIMESTAMP drops the zone by rendering the instant in the SESSION's
-- zone. A reader connected from a zone that is not UTC therefore sees
-- every day shifted by that zone's offset — midnight becomes another
-- hour of the same instant, an as-of-day equality misses, and a
-- day-grain series buckets across the boundary.
--
-- `epoch_ms(x * 1000)` yields a zone-free TIMESTAMP directly from the
-- epoch seconds the macros carry, which is the choice migration 0042
-- already documents for the period bucketing. The views are re-issued
-- with that expression and nothing else: same names, same column
-- lists, same types, same sources — so a card, a saved question or a
-- field id built on any of them keeps working, and CREATE OR REPLACE
-- suffices without dropping the view (migration 0043's header note).
--
-- Each view is re-issued from its LATEST shape: web_transactions from
-- migration 0042 (0032's issue predates the account_kind column),
-- web_spending and web_card_balances_history from 0043, the rest from
-- 0032.
--
-- CREATE OR REPLACE throughout keeps this replayable for the DDL-rerun
-- test (see gold.Migrate's REPLAY note).

CREATE OR REPLACE VIEW web_sources_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id,
           positions_value_usd, cash_balance_usd, total_value_usd,
           positions_value_chf, cash_balance_chf, total_value_chf,
           positions_value_eur, cash_balance_eur, total_value_eur
      FROM report_sources_history_multi();

CREATE OR REPLACE VIEW web_transactions AS
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           kind, silver_source_id, account_kind,
           value_usd, value_chf, value_eur
      FROM report_transactions_multi(0, 9223372036854775807);

CREATE OR REPLACE VIEW web_asset_classes_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, asset_class,
           SUM(value_usd) AS value_usd,
           SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur
      FROM report_positions_history_multi()
     GROUP BY 1, 2, 3
    UNION ALL
    SELECT epoch_ms(as_of_day * 1000),
           silver_source_id, 'cash',
           cash_balance_usd, cash_balance_chf, cash_balance_eur
      FROM report_sources_history_multi();

CREATE OR REPLACE VIEW web_vehicles_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, vehicle,
           SUM(value_usd) AS value_usd,
           SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur
      FROM report_positions_history_multi()
     GROUP BY 1, 2, 3
    UNION ALL
    SELECT epoch_ms(as_of_day * 1000),
           silver_source_id, 'demand_deposit',
           cash_balance_usd, cash_balance_chf, cash_balance_eur
      FROM report_sources_history_multi();

CREATE OR REPLACE VIEW web_accounts_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, tax_wrapper, management_style,
           SUM(total_value_usd) AS total_value_usd,
           SUM(total_value_chf) AS total_value_chf,
           SUM(total_value_eur) AS total_value_eur
      FROM report_accounts_history_multi()
     GROUP BY 1, 2, 3, 4;

CREATE OR REPLACE VIEW web_positions_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, symbol, name, asset_class, vehicle, currency,
           SUM(value_usd) AS value_usd,
           SUM(value_chf) AS value_chf,
           SUM(value_eur) AS value_eur
      FROM report_positions_history_multi()
     GROUP BY 1, 2, 3, 4, 5, 6, 7;

CREATE OR REPLACE VIEW web_spending AS
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           silver_source_id, account_external_id, display_name, account_kind,
           merchant_name,
           COALESCE(spend_primary,  '(uncategorized)') AS spend_primary,
           COALESCE(spend_detailed, '(uncategorized)') AS spend_detailed,
           value_usd, value_chf, value_eur
      FROM report_spending_transactions_multi(0, 9223372036854775807);

CREATE OR REPLACE VIEW web_card_balances_history AS
    SELECT epoch_ms(as_of_day * 1000) AS as_of_day,
           silver_source_id, account_external_id, display_name, currency,
           balance, balance_usd, balance_chf, balance_eur
      FROM report_card_balances_history_multi();

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (49, CAST(epoch(now()) AS BIGINT));
