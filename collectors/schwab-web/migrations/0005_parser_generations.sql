-- ============================================================
-- schwab-web silver schema, migration 0005 — parser generations.
--
-- Which generation of the document parsers produced the rows silver is
-- holding, so a re-parse can REPLACE them instead of landing beside them.
--
-- The collector already owns most of the machinery: `--reparse` opens the
-- per-run and per-document gates and deletes a document's transactions by
-- `logical_doc_key` before re-inserting them. What it lacked was anything
-- that noticed the parser had moved — `--reparse` is a flag a human
-- remembers, so a parser edit alone changed nothing and the rows quietly
-- stayed as the older parser had read them.
--
-- A stale generation now implies `--reparse` for the whole invocation.
-- That is the one lever the three gate sites already read, so the walk, the
-- per-document gates and the per-document deletes all follow from it.
--
-- `historical_position_snapshots` and `historical_cash_balances` need more
-- than the flag. Neither has ANY delete path — `--reparse` deletes only
-- from `transactions` — and both key on parser output (`as_of_date` /
-- `period_end`, and `instrument_key`), so a moved capture strands the old
-- row under a key nothing will write again. Both tables are written
-- exclusively by the statement passes, so the purge is the whole table. It
-- is safe precisely because the stale generation has already forced the
-- re-walk that refills them.
--
-- Seeded EMPTY: the first load after this migration re-parses the archive
-- once and stamps what it found.
-- ============================================================
CREATE TABLE parser_generations (
    scope      TEXT    NOT NULL PRIMARY KEY,  -- the pass, e.g. 'documents'
    generation TEXT    NOT NULL,              -- collectorkit.srcfp fingerprint
    stamped_at INTEGER NOT NULL               -- Unix seconds UTC
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (5, CAST(strftime('%s','now') AS INTEGER));
