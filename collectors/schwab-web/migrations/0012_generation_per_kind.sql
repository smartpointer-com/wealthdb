-- ============================================================
-- schwab-web silver schema, migration 0012 — one parser generation per
-- document kind.
--
-- The `documents` scope stamped one fingerprint over every document
-- parser. It now stamps the walk alone: how a run's manifest and its
-- exports become rows. Its generation is a constant ('1') that moves
-- with no parser, and a migration that changes the walk clears it as
-- before. Each document kind stamps a scope of its own, so a parser
-- change re-derives that kind's documents and no others.
--
-- A `documents` row proves that a full walk completed after the last
-- migration that cleared it, so the walk is seeded as current. The
-- kinds hold no stamp yet: the first load re-derives each kind's
-- documents once, and does not read the tx-history exports again. An
-- empty silver holds no row and stays unseeded.
-- ============================================================

UPDATE parser_generations SET generation = '1' WHERE scope = 'documents';

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (12, CAST(strftime('%s','now') AS INTEGER));
