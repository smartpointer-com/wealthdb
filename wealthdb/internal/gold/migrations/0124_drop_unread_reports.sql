-- ============================================================
-- gold schema, migration 0124 — drop the reports nothing reads.
--
-- Every reader of the daily series asks for every reporting currency
-- at once: the Metabase models wrap the `_multi` history macros, and
-- the returns engine reads report_accounts_history. The
-- single-currency global, sources, portfolios and positions series
-- have no reader, so they go; report_accounts_history stays.
--
-- The same holds for three multi-currency period reports: the
-- spending summary and categories, and the income types. The web reads
-- spending and income one line per row, through web_spending and
-- web_income (report_spending_transactions_multi,
-- report_income_transactions_multi), and buckets them in its cards.
-- The CLI reads the single-currency reports.
--
-- `asset_class_new` on instruments and positions is the transitional
-- column of the taxonomy cutover (0028, 0031): nothing has written or
-- read it since. DuckDB will not drop a column of a table that carries
-- an index, so instruments' two indexes are dropped around it and
-- re-created as 0001 made them.
--
-- IF EXISTS and IF NOT EXISTS keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

DROP MACRO TABLE IF EXISTS report_global_history;
DROP MACRO TABLE IF EXISTS report_sources_history;
DROP MACRO TABLE IF EXISTS report_portfolios_history;
DROP MACRO TABLE IF EXISTS report_positions_history;

DROP MACRO TABLE IF EXISTS report_spending_summary_multi;
DROP MACRO TABLE IF EXISTS report_spending_categories_multi;
DROP MACRO TABLE IF EXISTS report_income_types_multi;

ALTER TABLE positions DROP COLUMN IF EXISTS asset_class_new;

DROP INDEX IF EXISTS ix_instruments_isin;
DROP INDEX IF EXISTS ix_instruments_symbol;
ALTER TABLE instruments DROP COLUMN IF EXISTS asset_class_new;
CREATE INDEX IF NOT EXISTS ix_instruments_isin   ON instruments(isin);
CREATE INDEX IF NOT EXISTS ix_instruments_symbol ON instruments(symbol);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (124, CAST(epoch(now()) AS BIGINT));
