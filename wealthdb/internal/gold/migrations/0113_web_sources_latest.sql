-- `web_sources_latest`: each source's latest snapshot, for the Wealth
-- Overview's headline figures.
--
-- Net worth, positions value and cash balance read the latest
-- snapshot, the way `wealthdb holdings sources` does, and not the
-- carried-forward history the charts beside them read. The two differ
-- where a run covered only part of a source. The figures have to take
-- the dashboard's Currency picker, which a native card reads as a
-- template variable, and a native card can take the Time-range and
-- Source pickers only as field filters on a synced table or view. So
-- the macro is served here in the 0049 shape: the epoch rendered as a
-- zone-free TIMESTAMP, the reporting trio kept as columns, and the
-- base-currency and taxonomy columns left out, no card reading them.
--
-- CREATE OR REPLACE keeps this replayable for the DDL-rerun test (see
-- gold.Migrate's REPLAY note).

CREATE OR REPLACE VIEW web_sources_latest AS
    SELECT epoch_ms(snapshot_at * 1000) AS snapshot_at,
           silver_source_id,
           positions_value_usd, cash_balance_usd, total_value_usd,
           positions_value_chf, cash_balance_chf, total_value_chf,
           positions_value_eur, cash_balance_eur, total_value_eur
      FROM report_sources_multi(9223372036854775807);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (113, CAST(epoch(now()) AS BIGINT));
