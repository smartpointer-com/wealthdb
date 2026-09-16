-- ============================================================
-- chase silver schema, migration 0005 — parser generations.
--
-- Which generation of the statement parser produced the rows a pass
-- materialised, so a re-parse can REPLACE them instead of landing beside
-- them.
--
-- Every `source='statement'` row is keyed on `_statement_fitid` /
-- `_card_statement_fitid`, whose basis includes the DESCRIPTION the parser
-- read off the PDF. `_insert_transaction` is `INSERT OR IGNORE` on that id
-- and no pass deletes, which is exactly what makes re-loading under one
-- parser converge — and re-loading under a changed one additive. An edit
-- that moves a cheque number out of the description, or stops folding a
-- legend into it, re-keys the row; the next ordinary load then inserts the
-- new row and keeps the old, permanently double-counting that outflow with
-- nothing reporting a problem. The export ledgers are immune: migration
-- 0002 moved them onto structural ids that carry no parsed text.
--
-- The stamp is the loader's own fingerprint of the parsing logic
-- (`collectorkit.srcfp.parser_fingerprint`, the value the other collectors
-- already key their parse caches on), not a hand-maintained number, so it
-- moves on its own when the parser or its import closure changes.
--
-- Seeded EMPTY on purpose. A scope with no row reads as stale, so the first
-- load after this migration purges the statement rows and re-derives them:
-- they were produced by a generation this DB never recorded, and only a
-- re-derivation can say which. That costs one pass over the statement PDFs
-- and converges on the same rows when the parser has not moved.
-- ============================================================
CREATE TABLE parser_generations (
    scope      TEXT    NOT NULL PRIMARY KEY,  -- the pass, e.g. 'statement'
    generation TEXT    NOT NULL,              -- collectorkit.srcfp fingerprint
    stamped_at INTEGER NOT NULL               -- Unix seconds UTC
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (5, CAST(strftime('%s','now') AS INTEGER));
