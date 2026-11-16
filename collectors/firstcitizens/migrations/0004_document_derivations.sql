-- ============================================================
-- firstcitizens silver schema, migration 0004 — which parser generation
-- read each statement's rate.
--
--   * document_derivations (collectorkit.derivations) holds one row per
--     statement document the rate pass read: scope 'statements', the
--     statement parser's generation, and the statement_rates rows it
--     wrote (0 or 1). A load reads only the documents without a row under
--     the current generation. A moved parser reads them all again.
--   * It is seeded from the marks of parsed_statements, under the
--     generation parser_generations stamped for them, so a load under the
--     same parser reads nothing again. Both tables then go: nothing else
--     reads them.
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
SELECT p.sha256, g.scope, g.generation, g.stamped_at,
       (SELECT COUNT(*) FROM statement_rates r WHERE r.source_sha256 = p.sha256)
  FROM parsed_statements p
  JOIN parser_generations g ON g.scope = 'statements';

DROP TABLE parsed_statements;
DROP TABLE parser_generations;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s','now') AS INTEGER));
