-- ============================================================
-- fidelity-web silver schema, migration 0016 — which parser generation
-- derived each statement and each Consolidated 1099.
--
--   * document_derivations (collectorkit.derivations) holds one row per
--     document a pass has read: its scope (the pass), the parser
--     generation, and the rows it gave. A pass records each document it
--     reads. A scope holding a document recorded under another generation
--     has a moved parser, and the pass re-derives, as a stale
--     parser_generations stamp made it do.
--   * It is seeded from parser_generations. A stamp there is written once
--     a pass has read its documents under that generation, so each
--     document of the scope is recorded under it. A scope with no stamp
--     was owed a re-derivation: its documents are recorded under the empty
--     generation, which the next load reads as moved, as it would have.
--     The documents of a scope are the ones its pass reads:
--       - statement_529: the retail `Statement*.pdf` of each dump;
--       - daf_statement: the `STATEMENT_*.pdf` of each giving account;
--       - supplied_statement: the PDFs outside the dumps that rows name;
--       - consolidated_1099: the copies whose 1099-B lots silver holds.
--     parser_generations then goes: nothing else reads it.
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
    scope          TEXT NOT NULL,
    row_count      INTEGER NOT NULL
);

INSERT INTO scope_documents
SELECT d.content_sha256, 'statement_529',
       (SELECT COUNT(*) FROM historical_position_snapshots h
         WHERE h.source_sha256 = d.content_sha256)
  FROM documents d
 WHERE d.file_format = 'pdf' AND d.doc_kind NOT GLOB 'daf_*'
   AND d.file_name GLOB 'Statement*.pdf';

INSERT INTO scope_documents
SELECT d.content_sha256, 'daf_statement',
       (SELECT COUNT(*) FROM historical_position_snapshots h
         WHERE h.source_sha256 = d.content_sha256)
  FROM documents d
 WHERE d.doc_kind = 'daf_statement' AND d.file_name GLOB 'STATEMENT_*.pdf';

INSERT INTO scope_documents
SELECT sha, 'supplied_statement', COUNT(*)
  FROM (SELECT source_sha256 AS sha FROM historical_position_snapshots
         UNION ALL
        SELECT source_sha256 FROM transactions
         WHERE activity_id GLOB 'stmt_*'
         UNION ALL
        SELECT source_sha256 FROM closed_lots
         WHERE document_kind = 'statement')
 WHERE sha NOT IN (SELECT content_sha256 FROM documents)
 GROUP BY sha;

INSERT INTO scope_documents
SELECT source_sha256, 'consolidated_1099', COUNT(*)
  FROM closed_lots
 WHERE document_kind = 'form_1099b'
 GROUP BY source_sha256;

INSERT INTO document_derivations
    (content_sha256, scope, generation, derived_at, row_count)
SELECT s.content_sha256, s.scope, COALESCE(g.generation, ''),
       COALESCE(g.stamped_at, CAST(strftime('%s', 'now') AS INTEGER)),
       s.row_count
  FROM scope_documents s
  LEFT JOIN parser_generations g ON g.scope = s.scope;

DROP TABLE scope_documents;
DROP TABLE parser_generations;


-- ------------------------------------------------------------
-- Migration-complete marker. The last statement before the COMMIT.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (16, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
