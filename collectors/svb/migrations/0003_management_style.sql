-- ============================================================
-- fidelity-web silver, migration 0003 — management_style on accounts.
--
-- Follow-up to the gold-layer punch list note in
-- fidelity-web/DESIGN.md §11.6:
-- Fidelity does NOT emit a per-account management-style indicator
-- on any captured surface, but the surfaced `portfolios.kind` is
-- enough to pin the style for the categories silver currently
-- models:
--
--   529 College Investing Plan accounts            → 'self_directed'
--     (account holder picks investment options from the plan
--     menu; no manager involved).
--   Trust accounts under a third-party manager     → 'discretionary'
--     (manager places trades; custodian executes).
--   Any other portfolios.kind (incl. 'other')      → NULL
--     (gold either infers from a future Fidelity signal or
--     keeps a manual override table on its side).
--
-- The column is on `accounts`, not `portfolios`, so a future
-- Fidelity build that surfaces per-account style indicators can
-- override the portfolio-level default without a schema change.
-- Loader populates the column on new dumps; this migration
-- back-fills existing rows.
-- ============================================================

ALTER TABLE accounts ADD COLUMN management_style TEXT;

UPDATE accounts
   SET management_style = (
       SELECT CASE p.kind
                  WHEN '529'            THEN 'self_directed'
                  WHEN 'trust_managed'  THEN 'discretionary'
                  ELSE NULL
              END
         FROM portfolios p
        WHERE p.snapshot_at           = accounts.snapshot_at
          AND p.portfolio_external_id = accounts.portfolio_external_id
   )
 WHERE management_style IS NULL;

-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s', 'now') AS INTEGER));
