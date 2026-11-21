-- ============================================================
-- equityzen silver schema, migration 0003 — which K-1 reader derived
-- each K-1 row.
--
--   * document_derivations (collectorkit.derivations) holds one row per
--     K-1 document: scope 'k1', the K-1 reader's generation, and the rows
--     it gave. A run records each K-1 it reads. After the runs, a load
--     reads again each K-1 that an older reader read. It reads the copy
--     in the run the row came from. So a reader change reaches the runs
--     loaded before it.
--   * It is seeded from k1_documents, one row per K-1 under the empty
--     generation: which reader wrote those rows is not recorded. The next
--     load therefore reads each stored K-1 once more, from the copy in
--     the run its row came from.
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

INSERT OR IGNORE INTO document_derivations
    (content_sha256, scope, generation, derived_at, row_count)
SELECT content_hash, 'k1', '', snapshot_at, 1
  FROM k1_documents
 WHERE content_hash IS NOT NULL;

-- Migration-complete marker (second-to-last; COMMIT closes the txn).
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
