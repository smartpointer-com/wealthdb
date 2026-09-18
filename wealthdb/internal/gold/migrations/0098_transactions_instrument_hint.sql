-- ============================================================
-- gold schema, migration 0098 —
--   a trade says what it was looked up BY when the lookup failed.
--
-- The adapters resolve a securities trade to an instrument from
-- whatever its feed states — a Swiss valor, a fund name, a ticker. When
-- that resolves to nothing the row reaches gold with no instrument and,
-- until now, no account of itself: the token was known to the adapter,
-- used once, and dropped.
--
-- That token is what a person needs to close the gap. The config link
-- (`transaction_instruments`) is keyed by it, so without it stored the
-- only way to author an entry was to know each feed's payload shape and
-- dig the token out by hand, per source. One column answers for all
-- three.
--
-- NULL is the ordinary value and means the row needed no lookup or the
-- lookup succeeded, so `instrument_external_id IS NULL AND
-- instrument_hint IS NOT NULL` is exactly the set a link can still
-- close — and the set shrinking is the measure of the work.
-- ============================================================

ALTER TABLE transactions ADD COLUMN IF NOT EXISTS instrument_hint TEXT;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (98, CAST(epoch(now()) AS BIGINT));
