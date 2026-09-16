-- ============================================================
-- relevate silver schema, migration 0003 — parser generations.
--
-- Which generation of `pdf_parsers` produced the rows the two PDF passes
-- are holding, so a re-parse can REPLACE them instead of landing beside
-- them.
--
-- `historical_position_snapshots` and `historical_cash_balances` have their
-- ENTIRE primary keys parsed out of the report PDF — the valuation date,
-- the reference number, the ISIN, the currency, the balance kind. An edit
-- to any of those captures re-keys the row, and `INSERT OR REPLACE` has
-- nothing left to replace, so the old row stays and the quarter is counted
-- twice. A tightening that makes a row stop parsing is the mirror image:
-- nothing removes what it can no longer produce, and the ghost holding
-- breaks the reconcile-to-the-printed-balance invariant the pass exists
-- for.
--
-- The credit-note pass shares the stamp because it shares the parser
-- module: one fingerprint moves both. Its own ids cannot duplicate
-- (`credit_note:<doc id>` comes from bronze JSON, not from the page), but a
-- parser change that makes a document raise leaves its row untouched and
-- silently stale, which the stamp is equally the cure for.
--
-- Both passes already re-read the whole document archive on every ordinary
-- load, outside the pending-dump branch, so the stamp needs no gate of its
-- own: a moved generation is honoured on the very next load.
--
-- Seeded EMPTY. The first load after this migration re-derives both passes
-- once, which is what they do on every load anyway.
-- ============================================================
BEGIN;

CREATE TABLE parser_generations (
    scope      TEXT    NOT NULL PRIMARY KEY,  -- the pass, e.g. 'pdf_reports'
    generation TEXT    NOT NULL,              -- collectorkit.srcfp fingerprint
    stamped_at INTEGER NOT NULL               -- Unix seconds UTC
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
