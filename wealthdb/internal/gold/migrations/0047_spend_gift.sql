-- The sixth delta, `gift`: a cash gift or family support paid from a
-- cash account. Like `card_spend`, and unlike `internal_transfer` and
-- `investment`, it stays IN the spending base.
--
-- A household gives money away: a cash gift, an allowance, support for
-- a relative. The bank books it as a plain transfer to a person, and
-- the vendored vocabulary has no honest home for it. Gifts-and-
-- novelties means buying gift items in a shop; donations means a
-- non-profit; and left to itself the model files such a transfer under
-- "other general services", which is false — nothing was bought and
-- no service rendered. `internal_transfer` would be false too, since
-- the money left the household, and `other` explains nothing.
--
-- The policy the value encodes: a cash gift IS spending, and it is
-- visible as what it is. It is a primary in its own right, so every
-- category report shows it as its own line rather than folding it
-- into a plausible-looking service category.
--
-- One statement, and nothing else changes:
--
--   * spend_categories gains the row, seeded from
--     internal/canonical/spendtaxonomy.go exactly as migrations 0040,
--     0045 and 0046 seed theirs; TestSpendCategoriesMatchGoTable pins
--     the dimension to the Go table.
--   * spending_lines_base is NOT re-issued. Its exclusion list names
--     `internal_transfer` and `investment` one by one (migration 0045)
--     rather than "every delta", so a row resolving to `gift` passes
--     it by construction, exactly as `card_spend` does. The schema and
--     report tests pin that a seeded `gift` row reaches the base, the
--     summary and the categories macros at both levels, and that
--     `internal_transfer` still does not.
--
-- It is a delta, so the model tier may never emit it: the gauntlet
-- validates against canonical.VendoredSpendDetailed, which is derived
-- from the vendored rows alone, and refuses the new value with no
-- change to the command. No built-in rule places it either — nothing
-- in a transfer's narrative says a person is family rather than a
-- contractor or an untracked account of the holder's own, and the
-- transfer fence keeps person-shaped narratives from the model in any
-- case. Config rules (`spending.rules`) and pins (`spending.pins`),
-- which carry the holder's own knowledge, are what place it.
--
-- INSERT OR REPLACE keeps this replayable for the DDL-rerun test (see
-- gold.Migrate's REPLAY note).
INSERT OR REPLACE INTO spend_categories (spend_primary, spend_detailed, description) VALUES
    ('gift', 'gift', 'Cash gift or family support — spending, with no merchant behind it; not a gift item bought in a shop, and not a donation to a non-profit');

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (47, CAST(epoch(now()) AS BIGINT));
