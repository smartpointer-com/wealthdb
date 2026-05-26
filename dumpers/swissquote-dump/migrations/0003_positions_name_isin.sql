-- ============================================================
-- migration 0003 — positions: add `name` and `isin` columns.
--
-- The Swissquote Positions XLS export carries only `Symbol` and
-- `CCY`. The long instrument name (the human-readable description
-- that Swissquote attaches in the UI) and the ISIN are surfaced
-- elsewhere in the Portfolio Overview DOM — name via a hover
-- tooltip on the symbol cell, ISIN as the path segment of the
-- row's FullQuote link href.
--
-- download.py scrapes both into a new bronze artefact
-- (position_details.json); load.py reads it and joins by
-- (symbol, currency) to populate these columns.
--
-- Both nullable: pre-migration rows have NULL, and reload of older
-- bronze dumps (which predate position_details.json) leaves them
-- NULL — forward-fill only, per the task spec.
--
-- ISIN is promoted, not just kept in payload, because it is the
-- right cross-bank join key for the gold-layer instruments table.
-- The Swissquote-specific symbol can differ between Positions and
-- Transactions tables for the same instrument (e.g. a bond rendered
-- with its full name in one table and a short ticker in the other);
-- ISIN is stable across both.
-- ============================================================

ALTER TABLE positions ADD COLUMN name TEXT;
ALTER TABLE positions ADD COLUMN isin TEXT;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s', 'now') AS INTEGER));
