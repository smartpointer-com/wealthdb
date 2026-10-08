-- ============================================================
-- ubs-web silver schema, migration 0015 — the NAV date of a
-- private-markets holding.
--
-- A private-markets holding prints its market price, the fund's NAV per
-- unit, on line 1 and the date of that NAV on line 3 (DESIGN.md §3.8).
-- `nav_date` holds that date as ISO 'YYYY-MM-DD'. A listed holding
-- leaves it NULL.
--
-- Line 2 of a holding in a currency other than the portfolio's ends in
-- its accrued interest where it accrues any. That figure goes in
-- `accrued_interest`, the column migration 0002 created. It is printed
-- in the market-value column, so it is in `market_value_currency`.
--
-- The payload keeps neither figure, so nothing is backfilled here. Both
-- fill when the document pass re-derives the archive. That happens on
-- the next load, since the parser generation has moved (migration 0009).
-- ============================================================

ALTER TABLE historical_position_snapshots ADD COLUMN nav_date TEXT;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (15, CAST(strftime('%s','now') AS INTEGER));
