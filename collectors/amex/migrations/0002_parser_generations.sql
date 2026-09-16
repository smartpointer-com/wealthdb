-- ============================================================
-- amex silver schema, migration 0002 — parser generations.
--
-- Which generation of `statement_parser` produced the statement era silver
-- is holding.
--
-- `rebuild_statements` already does the right thing when it runs: it parses
-- every PDF first, then deletes every `source='statement'` row from
-- `transactions` and `statement_balances` and re-imports, all inside one
-- BEGIN IMMEDIATE. What it lacked was a reason to run. Its only caller is
-- gated on `if loaded:` — a bronze run having landed — so editing the parser
-- and re-loading did nothing at all, and the deep era went on being whatever
-- the parser of the day had made of it.
--
-- The stamp is that reason. A generation that no longer matches opens the
-- same gate a new run does.
--
-- Not a duplication guard, unlike the other collectors': a statement row's
-- id is `stmt:<account>:<period_end>:<index>`, which holds no free text, so
-- a re-keying parser edit here mints the same ids and replaces in place.
-- What it fixes is silence — rows that quietly stay as an older parser read
-- them, with nothing saying so.
--
-- Seeded EMPTY. The first load after this migration therefore rebuilds the
-- statement era once, which is what `rebuild_statements` does on any load
-- that ingests a run anyway.
-- ============================================================
CREATE TABLE parser_generations (
    scope      TEXT    NOT NULL PRIMARY KEY,  -- the pass, e.g. 'statement'
    generation TEXT    NOT NULL,              -- collectorkit.srcfp fingerprint
    stamped_at INTEGER NOT NULL               -- Unix seconds UTC
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s','now') AS INTEGER));
