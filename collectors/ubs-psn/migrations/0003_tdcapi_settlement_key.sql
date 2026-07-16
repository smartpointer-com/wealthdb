-- ============================================================
-- ubs-psn silver schema, migration 0003 —
--   widen cash_account_pricing PK to per-settlement.
--
-- The scaffold in migration 0001 modelled TDCAPI as one row per cash
-- account per snapshot, under the (pre-sample) assumption that TDCAPI
-- carried per-account pricing configuration. Actual TDCAPI content is
-- a stream of individual service-charge / interest booking lines
-- (<CshAcctPricingAndInterestInfo>), several per account per day, each
-- keyed by <SttlmId> + <SttlmBkngNum>. The original PK collapses them.
--
-- The scaffold table is empty in every existing silver DB — the loader
-- for TDCAPI was deferred pending non-empty samples — so DROP+CREATE
-- is safe and matches the reload-from-bronze contract in DESIGN §6.
-- ============================================================

DROP TABLE cash_account_pricing;
CREATE TABLE cash_account_pricing (
    snapshot_at             INTEGER NOT NULL,
    relationship_id         TEXT    NOT NULL,
    account_external_id     TEXT    NOT NULL,   -- IBAN (canonicalised via cash_accounts.payload.AcctId)
    settlement_external_id  TEXT    NOT NULL,   -- <SttlmId>:<SttlmBkngNum> composite
    payload                 TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, relationship_id, account_external_id, settlement_external_id)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (3, CAST(strftime('%s', 'now') AS INTEGER));
