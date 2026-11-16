-- ============================================================
-- amex silver schema, migration 0003 — which parser generation derived
-- the statement era from each statement document.
--
--   * document_derivations (collectorkit.derivations) holds one row per
--     statement document in `documents`: scope 'statement', the statement
--     parser's generation, and the statement rows silver holds for the
--     document's card and period. `rebuild_statements` records every
--     statement document once it has committed. A load that ingests a run
--     clears the scope, since the seam that decides the era moves with the
--     runs. A load rebuilds when it ingested a run or when a document has
--     no row under the current generation, which is how a moved parser
--     rebuilds without a new run.
--   * It is seeded from parser_generations. A stamp there is written only
--     by a committed rebuild and cleared by every run after it, so it
--     proves that every statement document was derived under it. With no
--     stamp nothing is seeded, and the next load rebuilds, as it would
--     have. parser_generations then goes: nothing else reads it.
-- ============================================================

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
SELECT d.sha256, g.scope, g.generation, g.stamped_at,
       (SELECT COUNT(*) FROM transactions t
         WHERE t.source = 'statement'
           AND t.account_external_id = d.account_external_id
           AND t.statement_end_at = d.doc_date)
  FROM documents d
  JOIN parser_generations g ON g.scope = 'statement'
 WHERE d.doc_kind = 'statement';

DROP TABLE parser_generations;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s','now') AS INTEGER));
