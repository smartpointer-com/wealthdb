-- ============================================================
-- fidelity-web silver schema, migration 0008 — parser generations.
--
-- Which generation of a document parser produced the rows silver is
-- holding, recorded once a pass has run so the question can be answered in
-- plain SQL rather than by re-reading the loader.
--
-- The stamp is a record, not the guard. Three passes here materialise rows
-- out of PDFs — the 529 statements, the DAF statements and the supplied
-- monthly statements — and all three key their rows on text read off the
-- page: a holding's description is part of the
-- `historical_position_snapshots` primary key, and an activity row's
-- description is inside its id. Editing a parser therefore re-keys the rows
-- and `INSERT OR REPLACE` has nothing left to replace, so the old rows stay
-- beside the new ones. The scraped feed already paid for this: migration
-- 0005 had to rebuild `transactions` because Fidelity's mutable description
-- text sat inside the identity and a re-label duplicated the rows.
--
-- What prevents that here is finer-grained than a scope stamp, because the
-- hazard is finer-grained: every row carries the `source_sha256` of the PDF
-- it came from, so a pass drops that document's rows before re-deriving
-- them (`_drop_document_holdings`). A document is also the only unit any pass
-- can re-derive, and the three share one table with nothing on a row saying
-- which of them wrote it — so the document, not the pass, is the grain that
-- can actually be addressed. A statement that fails to parse, fails its
-- signature guard or fails reconciliation is never dropped: stale rows beat
-- no rows when nothing can re-derive them.
--
-- Values are the same fingerprints the parse cache is keyed on, each
-- folding in the parser's import closure, the extraction libraries and —
-- for the supplied statements — the signature guard, so they move on their
-- own when the parsing logic does.
-- ============================================================
CREATE TABLE parser_generations (
    scope      TEXT    NOT NULL PRIMARY KEY,  -- the pass, e.g. 'supplied_statement'
    generation TEXT    NOT NULL,              -- collectorkit.srcfp fingerprint
    stamped_at INTEGER NOT NULL               -- Unix seconds UTC
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (8, CAST(strftime('%s','now') AS INTEGER));
