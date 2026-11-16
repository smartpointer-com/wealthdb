-- ============================================================
-- carta silver schema, migration 0006 — one derive record per document.
--
-- document_derivations records, per document and scope, the parser
-- generation that derived the document's rows and how many it wrote
-- (collectorkit.derivations). A load reads a document again only where
-- the record is missing or names another generation, so a routine run
-- reads nothing it read before.
--
-- Scopes:
--   * k1 — the federal Schedule K-1 face page of a tax document
--     (k1_capital_accounts). A tax document with no face page (a 1042-S)
--     is recorded with row_count 0.
--
-- Backfill: none. A k1_capital_accounts row does not state the reader
-- that wrote it, so the next run that holds a tax document derives it
-- once more and records it.
-- ============================================================

BEGIN;

CREATE TABLE document_derivations (
    content_sha256 TEXT    NOT NULL,   -- the document (documents.content_sha256)
    scope          TEXT    NOT NULL,   -- what was derived from it ('k1')
    generation     TEXT    NOT NULL,   -- the parser generation that derived it
    derived_at     INTEGER NOT NULL,   -- when, unix seconds
    row_count      INTEGER NOT NULL,   -- the rows it derived
    PRIMARY KEY (content_sha256, scope)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (6, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
