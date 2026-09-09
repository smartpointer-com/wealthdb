-- Which values are a primary's own catch-all, as data.
--
-- `canonical.CatchAllSpendDetailed` reads it off the value — every
-- catch-all spells its detail part `OTHER_...` — and the provider tier
-- uses it to decline a row it can only place in one. SQL needs the same
-- fact: a reclassification pass has to find the rows whose verdict says
-- no more than the primary did, and reconstructing the rule in a WHERE
-- clause would give it a second definition to drift from.
--
-- Seeded literally beside the labels, and pinned to the Go predicate by
-- TestSpendCategoryCatchAllMatchesGoTable. The deltas are false: a delta
-- names a movement the taxonomy has no merchant word for, which is a
-- verdict about what the row IS rather than a shrug about a merchant.
ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS catch_all BOOLEAN;

UPDATE spend_categories
   SET catch_all = (spend_primary <> spend_detailed
                    AND starts_with(substr(spend_detailed, length(spend_primary) + 2), 'OTHER_'));

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (60, CAST(epoch(now()) AS BIGINT));
