-- ============================================================
-- viac silver schema, migration 0004 — parser generations.
--
-- Which generation of `pdf_parsers` produced the report-derived rows silver
-- is holding, so a re-parse can REPLACE them instead of landing beside them.
--
-- Nothing here hashes free text: a report row's identity is
-- (snapshot_at, account, ISIN), and the genuinely free-text fields sit
-- outside the key on purpose, so ordinary wording drift converges
-- harmlessly. The exposure is narrower and sharper — every one of those
-- three components is a REGEX CAPTURE off the page. Move the as-of date,
-- the portfolio anchor (which also draws the region boundaries) or the ISIN
-- token, and a whole quarter re-lands under a different key while the old
-- rows stay; gold's latest-snapshot-on-or-before selection will then
-- happily prefer a phantom date. A tightening that makes a holding stop
-- parsing is the other half: with no delete path the pass can never SHED a
-- row, so the ghost holding outlives the grammar that created it.
--
-- The purge is scoped by `source`, which migration 0003 already stamps as
-- 'report:<docid>' on every row this pass writes — the live REST rows carry
-- their own tags and are untouched. `instruments` is deliberately NOT
-- purged: it is master data shared with the REST phases, carries no source
-- marker, and its MIN() first_seen_at cannot be raised back once lowered.
--
-- One limitation, stated plainly: the pass runs per bronze dump, so a moved
-- generation is honoured on the next dump that lands rather than on the
-- next load. `load --force` is the way to re-derive sooner. What the stamp
-- guarantees either way is that the re-parse, whenever it comes, replaces.
-- ============================================================
CREATE TABLE parser_generations (
    scope      TEXT    NOT NULL PRIMARY KEY,  -- the pass, e.g. 'reports'
    generation TEXT    NOT NULL,              -- collectorkit.srcfp fingerprint
    stamped_at INTEGER NOT NULL               -- Unix seconds UTC
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s','now') AS INTEGER));
