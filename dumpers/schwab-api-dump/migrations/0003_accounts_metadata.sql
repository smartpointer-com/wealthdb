-- ============================================================
-- schwab-api-dump silver schema, migration 0003 — accounts metadata.
--
-- The accounts table previously held only the {accountNumber, hashValue}
-- mapping from /accounts/accountNumbers. This migration widens it with
-- three columns sourced from sibling artefacts in the same bronze dump:
--
--   account_type    — securitiesAccount.type from accounts_positions.json.
--                     Values: CASH or MARGIN. Describes margin
--                     enablement; *not* tax treatment.
--
--   preference_type — userPreference.accounts[].type. Currently only
--                     'BROKERAGE' is observed. Promoted for consistency
--                     and future-proofing.
--
--   nickname        — userPreference.accounts[].nickName. User-set free
--                     text. Schwab does not expose a structured field
--                     for regulatory account type (ESA, UTMA, IRA, Roth,
--                     trust, etc.); a meaningful nickname is the only
--                     signal of those.
--
-- The payload column is widened correspondingly: it now stores the
-- merged per-account dict from all three source artefacts. Content
-- dedup continues against the full payload.
-- ============================================================

ALTER TABLE accounts ADD COLUMN account_type    TEXT;
ALTER TABLE accounts ADD COLUMN preference_type TEXT;
ALTER TABLE accounts ADD COLUMN nickname        TEXT;

-- Existing rows have NULL for the new columns; they'll be superseded
-- by full rows on the next load that finds a richer payload (the dedup
-- path triggers an insert when the canonical payload differs).

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s','now') AS INTEGER));
