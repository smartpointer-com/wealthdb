-- ============================================================
-- chase silver schema, migration 0006 — which parser generation derived
-- the statement rows from each statement document.
--
--   * document_derivations (collectorkit.derivations) holds one row per
--     document in `documents`: scope 'statement' and the statement
--     parser's generation. Both statement passes derive tree-wide: a
--     deposit row comes from a chain of statements and a card period from
--     its best copy, and no row names its document. So a record holds no
--     row count (0). The passes record every document once all three have
--     returned with no shortfall. A record under another generation is a
--     moved parser: the loader purges the statement rows and re-derives
--     them. A document with no record yet is one a new run brought.
--   * It is seeded from parser_generations. Its stamp is written only by
--     passes that finished, under the generation that derived every
--     document then in silver, so each document is seeded under it. With
--     no stamp, which read as stale, each document is seeded under no
--     generation (''), which reads as stale the same way. parser_generations
--     then goes: nothing else reads it.
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
SELECT d.sha256, 'statement', COALESCE(g.generation, ''),
       COALESCE(g.stamped_at, CAST(strftime('%s','now') AS INTEGER)), 0
  FROM documents d
  LEFT JOIN parser_generations g ON g.scope = 'statement';

DROP TABLE parser_generations;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (6, CAST(strftime('%s','now') AS INTEGER));
