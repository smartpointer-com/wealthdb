-- ============================================================
-- gold schema, migration 0002 — accounts metadata columns.
--
-- Two adapter-or-config-populated free-text columns on accounts:
--
--   nickname          — a user-friendly label. Schwab silver
--                       exposes this directly (the user-set
--                       nickname from /userPreference). UBS and
--                       Swissquote silvers don't carry one
--                       today; the future config-side override
--                       (DESIGN.md §13.9) will fill in for
--                       those.
--
--   account_category  — a bank-assigned or user-set category
--                       hinting at the wealth-management wrapper
--                       (personal cash, managed mandate,
--                       advisory mandate, UTMA, ESA, ...).
--                       UBS populates from AcctTpDesc (with
--                       AcctSubTypeDesc for safekeeping
--                       accounts). Swissquote populates from
--                       silver.accounts.account_type. Schwab
--                       leaves it nil today (Schwab's
--                       securitiesAccount.type is CASH/MARGIN,
--                       which is margin enablement, not
--                       category); the config-side override
--                       fills it in.
--
-- Both are NULL by default — backwards-compatible with v1 rows.
-- ============================================================

ALTER TABLE accounts ADD COLUMN nickname         TEXT;
ALTER TABLE accounts ADD COLUMN account_category TEXT;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (2, CAST(epoch(now()) AS BIGINT));
