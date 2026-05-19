-- ============================================================
-- ubs-web-dump silver schema, migration 0003 — backfill
-- historical_position_snapshots.portfolio_external_id
--
-- Before this migration, the PDF parser in pdf_parsers.py
-- assembled the PSN-style portfolio_external_id without
-- zero-padding the branch code. UBS PDF labels render the
-- branch as 'BBB-AAAAAAAA-NN' (leading zero stripped) instead
-- of the PSN-canonical 'BBBB-AAAAAAAA-NN'. Result: silver rows
-- carried a 15-char ID (BBBAAAAAAAANNNN) instead of the 16-char
-- PSN-aligned form (0BBBAAAAAAAANNNN), so the gold-layer join
-- against PSN.portfolios came up empty and historical positions
-- showed up as a separate portfolio "twin" of every real one.
--
-- The parser fix prepends the leading zero on every new row.
-- This migration backfills any pre-existing 15-char rows so an
-- already-loaded silver picks up the fix without a full reload.
-- Idempotent: the WHERE guard means re-applying is a no-op.
-- ============================================================

PRAGMA foreign_keys = ON;

UPDATE historical_position_snapshots
   SET portfolio_external_id = '0' || portfolio_external_id
 WHERE LENGTH(portfolio_external_id) = 15;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s', 'now') AS INTEGER));
