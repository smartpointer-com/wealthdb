-- report_returns.window_from_year: the start year the summary was computed
-- from, so the Returns dashboard can rescope the since-inception scalars to a
-- later start (the early-inception period is often degenerate — a null TWR —
-- and the noise around it swamps the charts).
--
-- 0 = since inception, the default for every row the base matrix writes
-- (the 48 grain x period x currency partitions). window_from_year > 0 rows are
-- ADDITIONAL is_summary / granularity='total' rows: one per (grain, currency,
-- year, entity), each the return SINCE that year's Jan 1 through today — the
-- verbatim output of the OPEN-ENDED CLI window `wealthdb returns <grain>
-- <year>-01-01 - --period total -x <CCY>`. (NOT the bare-year form `wealthdb
-- returns <grain> <year>`, which parses to the single CALENDAR year
-- [Jan 1..Dec 31], a different figure.) Only the since-X summary depends on
-- the window; the per-period bucket returns do not, so they exist only at
-- window_from_year = 0.
--
-- DEFAULT 0 backfills any rows a schema-26 materialization already wrote (and
-- DuckDB rejects a NOT NULL constraint in ADD COLUMN); every materialize run
-- rewrites the table wholesale with the column always set, so the value is
-- never actually null in practice.
ALTER TABLE report_returns ADD COLUMN window_from_year INTEGER DEFAULT 0;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (27, CAST(epoch(now()) AS BIGINT));
