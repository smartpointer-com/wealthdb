-- Collapse the four per-facet window columns on dump_runs into one pair.
--
-- The CLI used to expose a transactions window and a separate documents
-- window, so a run recorded two independent windows. There is now one
-- --lookback flag naming a single window that every facet is fetched
-- over, which left four columns holding two values.
--
-- transactions_* carries forward as window_* because it is the pair the
-- loader always populated from the transactions block; documents_* is
-- dropped. Rows written before this migration where the two windows
-- genuinely differed keep the transactions pair and lose the documents
-- one — accepted deliberately: nothing reads either (the gold adapter
-- ignores dump_runs windows entirely; they are run provenance).
--
-- Done as a table rebuild rather than ALTER TABLE ... DROP COLUMN:
-- DROP COLUMN re-parses the stored CREATE TABLE text, and the trailing
-- `--` comments on this table's column list leave the rewritten DDL
-- truncated ("incomplete input"). The rebuild is also the form SQLite
-- documents for schema changes. Safe here because no index, view or
-- foreign key references dump_runs.

CREATE TABLE dump_runs_new (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL,
    window_since          INTEGER,
    window_until          INTEGER
);

INSERT INTO dump_runs_new (
    snapshot_at, silver_schema_version, run_dir, window_since, window_until
)
SELECT snapshot_at, silver_schema_version, run_dir,
       transactions_since, transactions_until
FROM dump_runs;

DROP TABLE dump_runs;

ALTER TABLE dump_runs_new RENAME TO dump_runs;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (6, CAST(strftime('%s', 'now') AS INTEGER));
