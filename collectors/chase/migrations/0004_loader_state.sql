-- ============================================================
-- chase silver schema, migration 0004 — loader state.
--
-- One row, one flag: whether the loader's three tree-wide post passes
-- (`load_statement_transactions`, `load_card_statements`,
-- `derive_card_balances`) still owe a run.
--
-- Those passes are not per-run work. They re-read the whole bronze tree and
-- recompute against every account's export seam, so they cannot be resumed
-- part-way and a marker per run or per pass would only re-run the same whole
-- tree while adding bookkeeping. What is worth persisting is the one bit
-- they leave behind.
--
-- Ingest commits each run into `dump_runs` as it lands, and a run already in
-- `dump_runs` is skipped forever after. Gating the post passes on "did this
-- invocation ingest anything" therefore made a load that died inside one of
-- them unrepairable by re-running: the next invocation ingested nothing, so
-- it skipped all three, and silver stayed without its pre-export statement
-- backfill and its reconstructed card running balances with nothing
-- reporting a problem. The same gate also blocked the ordinary heal a card
-- with no export seam waits for, which lands only on a load that happens to
-- ingest a run as well.
--
-- The flag is raised in its own transaction before the passes run and
-- cleared in its own after all three return, so a crash between the two
-- leaves it raised rather than rolling it back with the pass's own writes.
-- A clean load leaves it down, which is what keeps a no-op reload from
-- paying for the PDF parsing again.
--
-- DEFAULT 0 seeds a DB whose passes have never been owed anything. A DB
-- migrating up from 0003 gets the same seed: its last load ran the passes
-- under the old gate, and claiming they are owed a run would only make the
-- next load redo work that is idempotent anyway.
-- ============================================================
CREATE TABLE loader_state (
    -- Exactly one row, pinned by the CHECK: this is a singleton, not a
    -- log, and every read is an unqualified SELECT.
    id                   INTEGER PRIMARY KEY CHECK (id = 1),
    post_passes_pending  INTEGER NOT NULL DEFAULT 0
);

INSERT INTO loader_state (id, post_passes_pending) VALUES (1, 0);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s','now') AS INTEGER));
