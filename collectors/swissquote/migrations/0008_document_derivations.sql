-- ============================================================
-- swissquote silver schema, migration 0008 — which parser generation
-- read each Portfolio Performance statement.
--
-- document_derivations (collectorkit.derivations) holds one row per
-- statement whose positions the loader wrote: scope
-- 'portfolio_performance', the parser generation (load.py,
-- PP_GENERATION), and the positions rows it wrote. A load parses only
-- the statements without a row under the current generation. A new
-- generation parses them all again and replaces their `pp:<doc_id>`
-- rows.
--
-- It starts empty. No stamp says which parser wrote the statement rows
-- already in silver, so the first load after this migration parses
-- every statement once.
-- ============================================================

CREATE TABLE document_derivations (
    content_sha256 TEXT    NOT NULL,
    scope          TEXT    NOT NULL,
    generation     TEXT    NOT NULL,
    derived_at     INTEGER NOT NULL,
    row_count      INTEGER NOT NULL,
    PRIMARY KEY (content_sha256, scope)
);


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (8, CAST(strftime('%s', 'now') AS INTEGER));
