-- ============================================================
-- chase silver schema, migration 0003 — statement transaction coverage.
--
-- `statement_balances` asserts what a billing period opened and closed at.
-- It says nothing about whether that period's TRANSACTIONS reached silver,
-- and for some periods they do not — so a consumer reconciling the rows
-- between two anchors could not tell a genuine residual from a period that
-- was never populated. This column is that distinction.
--
--   1  the period's transactions are in `transactions` — imported from the
--      statement (below the account's export seam) or already carried by the
--      export (at or after it).
--   0  they are not, for one of two reasons:
--        * the period STRADDLES the export seam. The card statement pass
--          gates transactions on the whole billing period, because a
--          statement dates its rows by transaction date while the cycle
--          bills by post date — a row-level gate would let a row transacted
--          before the seam but posted after it land from both sources at
--          once. The straddling period is therefore given up rather than
--          double-counted. It is one period per card and cannot grow (the
--          seam is a MIN over rows already loaded), but its balances are
--          still wanted: its closing figure is what anchors the export era's
--          reconstructed running balance.
--        * the period's own row identity failed (Σ printed rows != closing −
--          opening), so its transactions were refused as a mis-parse while
--          its summary still added up.
--
-- The value is a function of the export seam, which moves as more export
-- history lands, so the card statement pass refreshes it on every load
-- rather than keeping the first observation. The printed balances beside it
-- are refreshed on the same load, from the copy of the period that passes
-- the most parse gates, so a period's anchors and its coverage flag always
-- describe one and the same copy.
--
-- DEFAULT 0 is the safe direction for a row this pass has not written: it
-- asserts nothing about coverage rather than claiming it. No row predates
-- this migration in practice — `statement_balances` was created empty by
-- 0002 and is only written by the card statement pass, which ships with it.
-- ============================================================
ALTER TABLE statement_balances
    ADD COLUMN transactions_covered INTEGER NOT NULL DEFAULT 0;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s','now') AS INTEGER));
