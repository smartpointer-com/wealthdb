-- ============================================================
-- schwab-web-dump silver schema, migration 0003
-- — per-account `account_registration` column.
--
-- The wealthdb gold layer carries a `tax_wrapper` column on its
-- canonical `accounts` (traditional_ira / roth_ira /
-- coverdell_esa / 529 / custodial_utma / custodial_ugma /
-- trust_* / etc.). Schwab's Trader API doesn't surface the
-- wrapper (see schwab-api-dump DESIGN.md §4.10), so the only
-- per-account signal we can derive is the registration line
-- Schwab prints at the top of page 1 of every statement PDF
-- — "Schwab One® International Account", "Contributory IRA",
-- "Schwab One® Custodial Account", "Education Savings", etc.
--
-- ------------------------------------------------------------
-- Column shape
-- ------------------------------------------------------------
-- One value per account, stable across statements. We store
-- the RAW Schwab label verbatim so the wealthdb adapter can do
-- the mapping to its `tax_wrapper` enum in Go where the
-- canonical taxonomy is defined. NULL = no recognisable header
-- in any of the account's statements (the adapter falls back
-- to its default at render time).
--
-- Populated by load.py after each statement PDF is parsed:
-- pdf_parsers.parse_account_registration extracts the label,
-- the loader UPDATEs the most-recent accounts row for that
-- account_external_id. A re-load of newer statements
-- overwrites with the latest seen value — the column drifts
-- only if Schwab themselves restyle the registration line.
-- ============================================================

ALTER TABLE accounts ADD COLUMN account_registration TEXT;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s','now') AS INTEGER));
