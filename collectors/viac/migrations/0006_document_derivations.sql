-- ============================================================
-- viac silver schema, migration 0006 — which parser generation
-- derived each Reporting PDF.
--
--   * document_derivations (collectorkit.derivations) holds one row per
--     report the historical-reports pass read: scope 'reports', the
--     `pdf_parsers` generation, and the rows it wrote (0 for a report
--     that does not parse). Each dump's pass reads the reports the dump
--     carries, as before, and records them. After the dumps, a load
--     derives the indexed reports without a row under the current
--     generation from their stored PDF, so a moved parser re-reads the
--     whole archive at the next load, not only the reports later dumps
--     carry.
--   * It is not seeded. parser_generations held one stamp per scope,
--     overwritten by each dump, so it names the generation of the last
--     dump's reports only: a report only an older dump carried may have
--     been read by an older parser. The first load after this migration
--     derives every indexed report once.
--   * parser_generations then goes: nothing reads it.
-- ============================================================
BEGIN;

CREATE TABLE document_derivations (
    content_sha256 TEXT    NOT NULL,
    scope          TEXT    NOT NULL,
    generation     TEXT    NOT NULL,
    derived_at     INTEGER NOT NULL,
    row_count      INTEGER NOT NULL,
    PRIMARY KEY (content_sha256, scope)
);

DROP TABLE parser_generations;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (6, CAST(strftime('%s','now') AS INTEGER));

COMMIT;
