-- ============================================================
-- gold schema, migration 0168 — a withholding rate per account
-- (docs/YIELD.md §8).
--
-- yield_withholding keys a situs country's rate on the account's tax
-- wrapper, and a wrapper's rate does not fit every account of it:
-- holders may differ in treaty status. yield_withholding_accounts holds the
-- accounts whose rate is not their wrapper's: one row per situs
-- country and account, from the config block's `accounts` entries
-- (gold.SyncYield). An account's row wins over its wrapper's, which
-- wins over the country's default.
--
-- IF NOT EXISTS keeps this replayable (see gold.Migrate's REPLAY
-- note).
-- ============================================================

CREATE TABLE IF NOT EXISTS yield_withholding_accounts (
    country              TEXT    NOT NULL,
    silver_source_id     TEXT    NOT NULL,
    account_external_id  TEXT    NOT NULL,
    rate_pct             DOUBLE  NOT NULL,
    PRIMARY KEY (country, silver_source_id, account_external_id)
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (168, CAST(epoch(now()) AS BIGINT));
