-- ============================================================
-- ubs-web silver schema, migration 0009 — parser generations.
--
-- Which generation of `pdf_parsers` produced the document-derived rows
-- silver is holding, so a re-parse can REPLACE them instead of landing
-- beside them.
--
-- The sharp case is the statement era of `transactions`. Its ids are
-- MINTED, and `description_kind` — the printed booking-type phrase, which
-- the parser assembles from page geometry — is inside the hash, alongside a
-- per-statement occurrence counter. A tokenisation or geometry shift
-- therefore mints a new id and the upsert has nothing to replace, so the
-- row lands beside its predecessor. The purge names that era by its id
-- prefix; the live CSV-export rows carry UBS's own transaction numbers with
-- no prefix and are untouched, exactly as DESIGN.md §3.6 describes the two
-- id spaces.
--
-- The three `historical_*` tables fail the other way. They key on
-- structured tuples and replace in place, so they do not double — but with
-- no delete path they can never SHED a row, and a capture that moves (the
-- as-of date, the IBAN header) strands the old one under a key nothing will
-- write again. Each table is written by exactly one PDF pass and holds
-- nothing else, so the purge is the whole table.
--
-- One limitation, stated plainly: these passes run per bronze dump, so a
-- moved generation is honoured on the next dump that lands rather than on
-- the next load. `load --force` is the way to re-derive sooner. What the
-- stamp guarantees either way is that the re-parse, whenever it comes,
-- replaces.
-- ============================================================
CREATE TABLE parser_generations (
    scope      TEXT    NOT NULL PRIMARY KEY,  -- the pass, e.g. 'documents'
    generation TEXT    NOT NULL,              -- collectorkit.srcfp fingerprint
    stamped_at INTEGER NOT NULL               -- Unix seconds UTC
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (9, CAST(strftime('%s','now') AS INTEGER));
