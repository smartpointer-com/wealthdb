-- ============================================================
-- relevate silver schema, migration 0004 — which parser generation
-- derived each PDF.
--
--   * document_derivations (collectorkit.derivations) holds one row per
--     quarterly report and credit note the two PDF passes derived:
--     scope 'pdf_reports', the `pdf_parsers` generation, and the rows the
--     document wrote. A load derives only the documents without a row
--     under the current generation. A moved generation drops both
--     passes' rows and derives every document again.
--   * It is seeded from parser_generations, under the generation it
--     stamped. Both passes read every indexed report and credit note at
--     each load, and a load stamps its own generation only when its
--     re-derivation came up whole. So a stamp that matches the code
--     names the parser that read every document indexed today, and one
--     that does not makes the next load derive them all, as before. A
--     silver with no stamp gets the generation '', which no parser has:
--     its next load drops the rows and derives them all, as a missing
--     stamp did.
--   * parser_generations then goes: nothing else reads it.
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

INSERT INTO document_derivations
    (content_sha256, scope, generation, derived_at, row_count)
SELECT d.content_sha256, 'pdf_reports',
       COALESCE(g.generation, ''),
       COALESCE(g.stamped_at, CAST(strftime('%s', 'now') AS INTEGER)),
       (SELECT COUNT(*) FROM historical_position_snapshots h
         WHERE h.source_sha256 = d.content_sha256)
     + (SELECT COUNT(*) FROM historical_cash_balances h
         WHERE h.source_sha256 = d.content_sha256)
     + (SELECT COUNT(*) FROM transactions t
         WHERE t.source = 'credit_note_pdf'
           AND t.transaction_external_id = 'credit_note:' || d.relevate_doc_id)
  FROM documents d
  LEFT JOIN parser_generations g ON g.scope = 'pdf_reports'
 WHERE d.doc_kind IN ('quarterly_report', 'credit_note');

DROP TABLE parser_generations;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
