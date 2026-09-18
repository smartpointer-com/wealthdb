-- ============================================================
-- viac silver schema, migration 0005 — a trade names its instrument.
--
-- VIAC's transaction feed carries the fund in `description` and nothing
-- else: no ISIN, no symbol, no id of any kind. Silver stored the
-- payload verbatim and promoted nothing, so every buy and sell reached
-- gold with no instrument — and gold, having no way to tell what was
-- traded, filed the lot under its untracked-destination class.
--
-- The link is free. `description` IS the instrument's `name` in this
-- silver's own `instruments` table, so the resolution is a join rather
-- than a parse, and it is done HERE, once, rather than by every
-- consumer.
--
-- A name that matches more than one instrument resolves to NOTHING.
-- The shape that produces one is a fund reissued under a second ISIN,
-- where both rows keep the name: guessing between them would put a
-- trade on the wrong line of a portfolio that holds both, and an
-- unresolved row is visible where a wrong one is not.
--
-- The exposure and wrapper are deliberately NOT stored here. Silver's
-- `asset_class` is VIAC's own coarse word; the canonical 2-D pair is
-- derived by the gold adapter's `taxonomyFor`, which already does it
-- for positions. One mapping, in one place, rather than a copy that can
-- drift from it.
-- ============================================================

ALTER TABLE transactions ADD COLUMN instrument_external_id TEXT;

-- Backfill every row silver already holds. A row whose description
-- names no instrument, or names an ambiguous one, keeps NULL.
UPDATE transactions
   SET instrument_external_id = (
       SELECT i.instrument_external_id
         FROM instruments i
        WHERE i.name = json_extract(transactions.payload, '$.description')
          AND (SELECT COUNT(*) FROM instruments j WHERE j.name = i.name) = 1
   );

CREATE INDEX ix_transactions_instrument
    ON transactions(instrument_external_id);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (5, CAST(strftime('%s','now') AS INTEGER));
