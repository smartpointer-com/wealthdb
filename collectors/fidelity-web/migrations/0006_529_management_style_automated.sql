-- ============================================================
-- fidelity-web silver, migration 0006 — correct the 529
-- management_style from 'self_directed' to 'automated'.
--
-- Migration 0003 classified 529 accounts as 'self_directed' on the
-- theory that the account holder freely picks investments. That is
-- wrong for a Fidelity 529: the plan offers only percentage-wise
-- allocation across a small menu of funds and age-based strategies —
-- a model-portfolio arrangement, which the canonical taxonomy models
-- as 'automated' (algorithmic / model-driven), not 'self_directed'
-- (free security selection). See DESIGN.md §4.4 / §11.6.
--
-- Only rows still carrying the old default are touched, so a future
-- per-account style signal (if Fidelity ever emits one) that already
-- overrode the default is left alone.
-- ============================================================

UPDATE accounts
   SET management_style = 'automated'
 WHERE management_style = 'self_directed'
   AND portfolio_external_id IN (
       SELECT portfolio_external_id FROM portfolios WHERE kind = '529'
   );

-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (6, CAST(strftime('%s', 'now') AS INTEGER));
