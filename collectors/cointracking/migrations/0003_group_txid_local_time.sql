-- cointracking silver schema v3: the export's Group, Tx-ID and local time.
--
-- `transactions` gains three columns. Each holds the export's text as
-- printed; a blank cell is NULL.
--
--   trade_group     the `Group` column: CoinTracking's trade-group label.
--   tx_id           the `Tx-ID` column: the transaction id CoinTracking
--                   imported with the row, such as an exchange's trade id
--                   or a chain's transaction hash.
--   occurred_local  the `Date` column. CoinTracking writes it in the
--                   timezone set on the portfolio's account, with no
--                   offset.
--
-- `occurred_at` is `occurred_local` read in `portfolios.timezone` and
-- converted to UTC.
--
-- `portfolios` gains `timezone`: the IANA zone the load read `Date` in.
-- It comes from the collector's config; a portfolio the config does not
-- name is read as UTC. The default records that rows loaded before this
-- column existed were read as UTC.
--
-- Transactions loaded under an older schema hold NULL in the three new
-- columns. The load ingests the newest export again when it was loaded
-- under an older schema version, which fills them.
--
-- ADD COLUMN IF NOT EXISTS keeps the file safe to re-apply on every run.

ALTER TABLE transactions ADD COLUMN IF NOT EXISTS trade_group VARCHAR;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS tx_id VARCHAR;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS occurred_local VARCHAR;

ALTER TABLE portfolios ADD COLUMN IF NOT EXISTS timezone VARCHAR DEFAULT 'UTC';

INSERT INTO schema_meta (silver_schema_version) VALUES (3)
ON CONFLICT DO NOTHING;
