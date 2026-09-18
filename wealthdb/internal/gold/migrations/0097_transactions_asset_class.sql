-- ============================================================
-- gold schema, migration 0097 —
--   a securities trade says what it traded.
--
-- `positions` has carried `(instrument_external_id, asset_class)` as a
-- PAIR from the start: the instrument may be null, the class never is,
-- and the same instrument appears under more than one class because
-- what is held and what it is held AS are different questions.
-- `transactions` carried only the instrument, so a trade could not say
-- what it traded — an option on a share and the share itself were the
-- same row shape, and a feed that names neither had nowhere to put
-- what it did know.
--
-- The column is the denormalised half of that, by decision: a
-- transaction points at no position and no lot. Most feeds cannot say
-- which position a trade moved; the one shape that can (a broker
-- position id) is spliced with a feed that cannot, so it reaches only
-- part of its own source's history. A cost basis that answers for some
-- sources and not others is worse than none, because nothing on the
-- page says which. Lot tracking stays a question for when it can be
-- answered warehouse-wide.
--
-- NULL is the ordinary value and means "nothing to add": every row
-- that is not a securities trade, and every trade whose instrument
-- already answers. A reader wanting the exposure of a trade takes this
-- column where it is set and the instrument's where it is not.
-- ============================================================

ALTER TABLE transactions ADD COLUMN IF NOT EXISTS asset_class TEXT;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS vehicle     TEXT;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (97, CAST(epoch(now()) AS BIGINT));
