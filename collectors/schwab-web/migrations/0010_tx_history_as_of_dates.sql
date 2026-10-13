-- ============================================================
-- schwab-web silver schema, migration 0010 — tx-history rows dated
-- "MM/DD/YYYY as of MM/DD/YYYY".
--
-- The loader skipped a tx-history row whose date carries an "as of"
-- day, because its date parser read only a plain MM/DD/YYYY. It now
-- reads the first date. Clearing the documents parser generation makes
-- the next load stale, so it re-walks the archive with `--reparse`
-- semantics and every export is read again.
-- ============================================================

DELETE FROM parser_generations WHERE scope = 'documents';

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (10, CAST(strftime('%s','now') AS INTEGER));
