-- ============================================================
-- migration 0004 — positions: add `source` column for provenance.
--
-- Until now every positions row came from the live Portfolio
-- Overview XLS export (one row per snapshot_at, the dump's run
-- timestamp). With this migration the table also accepts rows
-- reconstructed from historical Portfolio Performance PDFs (annual
-- year-end snapshots), with snapshot_at set to the document's
-- effective "as-of" date rather than the dump's run time.
--
-- Provenance tags:
--   'live'        — row came from the current Positions XLS export
--                   (default for existing and future XLS-sourced rows)
--   'pp:<doc_id>' — row was parsed out of the Portfolio Performance
--                   PDF with that document GUID. Loader uses this
--                   tag for idempotent re-parses (DELETE … WHERE
--                   source='pp:<doc_id>' before INSERT).
--
-- NOT NULL with DEFAULT 'live' so existing rows are assigned the
-- right provenance retroactively and no backfill is required.
-- ============================================================

ALTER TABLE positions ADD COLUMN source TEXT NOT NULL DEFAULT 'live';

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s', 'now') AS INTEGER));
