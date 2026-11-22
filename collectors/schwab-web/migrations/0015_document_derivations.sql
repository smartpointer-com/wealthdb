-- ============================================================
-- schwab-web silver schema, migration 0015 — which parser generation
-- derived each document.
--
--   * document_derivations (collectorkit.derivations) holds one row per
--     document copy a pass has parsed: its scope (the document kind of
--     load.PASSES), the parser generation, and the rows it wrote. A pass
--     records the copy it parses beside the document's parse marker, and
--     a failed parse with no rows. A scope holding a record under another
--     generation has a moved parser, and the load re-derives that kind,
--     as a stale parser_generations stamp made it do. A load that fails
--     nowhere then drops the kind's records under another generation.
--   * It is seeded from parser_generations. A kind's stamp is written
--     only by a load that failed nowhere, under the generation that
--     derived every document of the kind silver then held. So each
--     document of the kind is seeded under it: the copy each parse marker
--     names, and each copy of the kind in `documents`, which also covers
--     the documents whose markers a moved kind purged before a load that
--     failed. A kind with no stamp was owed a re-derivation: its documents
--     are seeded under the empty generation, which the next load reads as
--     moved, as it would have. A seeded copy that no pass reads again is
--     dropped once its kind moves, as any other record is.
--   * The rows a seeded record holds are the rows silver holds from that
--     copy, in the tables the kind writes.
--   * The `documents` scope stays in parser_generations: it stamps the
--     walk, which is not a set of documents. The kinds' stamps go.
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

CREATE TEMP TABLE scope_documents (
    content_sha256 TEXT NOT NULL,
    scope          TEXT NOT NULL
);

INSERT INTO scope_documents
SELECT source_sha256,
       CASE WHEN document_kind IN ('year_end_summary', 'gain_loss_report')
            THEN 'realized_report' ELSE document_kind END
  FROM parsed_documents
 WHERE document_kind IN ('statement', 'third_party_distribution',
                         'form_1099b', 'year_end_summary',
                         'gain_loss_report');

INSERT INTO scope_documents
SELECT sha256, 'statement' FROM documents
 WHERE doc_kind = 'statement' AND file_format = 'pdf';

INSERT INTO scope_documents
SELECT sha256, 'third_party_distribution' FROM documents
 WHERE doc_kind = 'letter' AND file_format = 'pdf'
   AND filename GLOB '3rd-Party-Distribution*';

INSERT INTO scope_documents
SELECT sha256, 'form_1099b' FROM documents
 WHERE doc_kind = 'tax_form' AND file_format IN ('xml', 'csv');

INSERT INTO scope_documents
SELECT sha256, 'realized_report' FROM documents
 WHERE doc_kind = 'tax_form' AND file_format = 'pdf';

CREATE TEMP TABLE held_rows AS
SELECT sha, scope, COUNT(*) AS n
  FROM (SELECT source_sha256 AS sha, 'statement' AS scope
          FROM transactions WHERE source = 'statement_pdf'
         UNION ALL
        SELECT source_sha256, 'statement' FROM historical_position_snapshots
         UNION ALL
        SELECT source_sha256, 'statement' FROM historical_cash_balances
         UNION ALL
        SELECT source_sha256, 'third_party_distribution'
          FROM transactions WHERE source = 'third_party_distribution'
         UNION ALL
        SELECT source_sha256, 'form_1099b'
          FROM transactions WHERE source = 'form_1099b'
         UNION ALL
        SELECT source_sha256, 'form_1099b'
          FROM closed_lots WHERE document_kind = 'form_1099b'
         UNION ALL
        SELECT source_sha256, 'realized_report'
          FROM closed_lots WHERE document_kind IN ('year_end_summary',
                                                   'gain_loss_report')
         UNION ALL
        SELECT source_sha256, 'realized_report' FROM cost_basis_methods)
 GROUP BY sha, scope;

INSERT INTO document_derivations
    (content_sha256, scope, generation, derived_at, row_count)
SELECT s.content_sha256, s.scope, COALESCE(g.generation, ''),
       COALESCE(g.stamped_at, CAST(strftime('%s', 'now') AS INTEGER)),
       COALESCE(h.n, 0)
  FROM (SELECT DISTINCT content_sha256, scope FROM scope_documents) s
  LEFT JOIN parser_generations g ON g.scope = s.scope
  LEFT JOIN held_rows h ON h.sha = s.content_sha256 AND h.scope = s.scope;

DROP TABLE held_rows;
DROP TABLE scope_documents;
DELETE FROM parser_generations WHERE scope != 'documents';


-- ------------------------------------------------------------
-- Migration-complete marker. The last statement before the COMMIT.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (15, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
