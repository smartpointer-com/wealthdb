-- ============================================================
-- migration 0005 — accounts: rename account_type to account_product.
--
-- The column added in migration 0002 carries the per-account product
-- string Swissquote shows in the eBanking #accountOverview/main
-- listing ("Trading", "Säule 3a", "Freizügigkeit", "Savings",
-- "Invest Easy", ...). At the silver/gold boundary, "account_type"
-- has proven ambiguous — wealthdb's gold adapter calls this a
-- product/tax-wrapper distinction, not a "type". Renaming for
-- clarity at no cost:
--
--   "Trading"         → wealthdb tax_wrapper: taxable_personal
--   "Savings"         → wealthdb tax_wrapper: taxable_personal
--   "Säule 3a"        → wealthdb tax_wrapper: pillar_3a
--   "Freizügigkeit"   → wealthdb tax_wrapper: vested_benefits
--   anything else     → wealthdb tax_wrapper: taxable_personal (default)
--
-- The bronze accounts.json artefact also moves to writing
-- `account_product` going forward; load.py accepts either key for
-- backward compat with bronze dumps that predate this rename.
-- ============================================================

ALTER TABLE accounts RENAME COLUMN account_type TO account_product;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (5, CAST(strftime('%s', 'now') AS INTEGER));
