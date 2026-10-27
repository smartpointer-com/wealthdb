-- ============================================================
-- schwab-web silver schema, migration 0011 — the rate a statement's
-- cash earns.
--
-- `historical_cash_balances` gains the interest rate the statement
-- states for the account's cash at the period's end, in percent a year
-- (pdf_parsers.parse_cash_interest_rate), and the line it is read from.
-- Both stay NULL where the statement states no rate: unknown, not zero.
--
-- Clearing the documents parser generation makes the next load stale,
-- so it purges the statement tables and re-parses every statement in
-- bronze, which fills the new columns.
-- ============================================================

ALTER TABLE historical_cash_balances ADD COLUMN interest_rate REAL;
ALTER TABLE historical_cash_balances ADD COLUMN interest_rate_line TEXT;

DELETE FROM parser_generations WHERE scope = 'documents';

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (11, CAST(strftime('%s','now') AS INTEGER));
