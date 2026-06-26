-- ============================================================
-- gold schema, migration 0003 — accounts parent link.
--
-- Adds a self-referencing parent_account_external_id column so a
-- child account (UBS cash / safekeeping) can name its parent
-- portfolio. NULL for top-level accounts (Schwab brokerage,
-- UBS portfolios themselves, Swissquote brokerage, any account
-- the bank doesn't expose a parent for).
--
-- Semantically references (silver_source_id, parent_account_
-- external_id) → accounts(silver_source_id, account_external_id)
-- but the FK is not declared — DuckDB can't defer FK checks
-- across the same-tx parent+child delete pattern the loader uses
-- (see DESIGN.md note in §7.2).
--
-- The `wealthdb holdings accounts` rollup uses this column to compute
-- aggregate values on portfolio rows: a portfolio row's
-- positions_value and cash_balance include lines from every
-- account whose parent_account_external_id matches the portfolio.
-- Component-account rows still report their own values, so a sum
-- over the column intentionally double-counts. Filter by
-- account_kind to pick a single perspective.
-- ============================================================

ALTER TABLE accounts ADD COLUMN parent_account_external_id TEXT;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (3, CAST(epoch(now()) AS BIGINT));
