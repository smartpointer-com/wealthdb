-- Transaction enrichment columns for spending analytics: who the
-- money went to, and how the provider itself filed the event.
--
--   counterparty      — the merchant / payee the event settled with.
--                       Not merely informational: it is the input to
--                       the merchant signature that groups spend, so
--                       an adapter's formatting of this string is a
--                       stated contract — drift re-keys merchants.
--   provider_category — the provider's own spend category, verbatim
--                       (a card issuer's "Groceries", "Travel", ...).
--                       Supplementary metadata, never normalised here.
--
-- Both nullable: every pre-existing row, and every non-card source,
-- leaves them NULL.
--
-- Additive ALTER on a macro-referenced table, as in 0028. IF NOT
-- EXISTS is mandatory, not decorative: it keeps this replayable for
-- the DDL-rerun test (see gold.Migrate's REPLAY note), where a bare
-- ADD COLUMN would raise "column already exists".
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS counterparty      TEXT;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS provider_category TEXT;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (38, CAST(epoch(now()) AS BIGINT));
