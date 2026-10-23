-- ============================================================
-- gold schema, migration 0147 — the dividend calendar, and the
-- settlement cycles that derive a record day from an ex day.
--
-- Loaded, windowed by as_of like the reference tables of 0134:
--
--   * dividend_calendar: one distribution of a fund or a security as a
--     provider states it (the refdata silver's dividend_events). ref is
--     the fund reference or security key the wanted list names it by,
--     or jpx:<code> for a row of the JPX list, by the share's Tokyo
--     Stock Exchange code. kind is dividend, capital_gain,
--     return_of_capital, special or other; each day is NULL where the
--     source states none; amount is gross per share or unit, in
--     currency; status is final, estimated or NULL. as_of is the day the
--     source stated it: a later statement of the same distribution
--     replaces the row, whatever day the earlier one was stated on.
--
-- Config, replaced whole at every load with the rest of the `yield`
-- block (gold.SyncYield):
--
--   * yield_settlement_cycles: each market's settlement cycles, the
--     business days a trade settles after, each from its from_day on
--     (NULL: from the first day on). The built-in defaults with the
--     config's markets over them, seeded here with the defaults where no
--     load has stamped them yet. A market with no row has no cycle.
--
-- IF NOT EXISTS keeps this replayable (see gold.Migrate's REPLAY note).
-- ============================================================

CREATE TABLE IF NOT EXISTS dividend_calendar (
    silver_source_id VARCHAR NOT NULL,
    ref              VARCHAR NOT NULL,
    provider         VARCHAR NOT NULL,
    event_id         VARCHAR NOT NULL,
    as_of            BIGINT  NOT NULL,
    kind             VARCHAR NOT NULL,
    ex_day           DATE,
    record_day       DATE,
    pay_day          DATE,
    declared_day     DATE,
    amount           DECIMAL(28,8),
    currency         VARCHAR,
    status           VARCHAR,
    PRIMARY KEY (silver_source_id, ref, provider, event_id)
);

CREATE INDEX IF NOT EXISTS ix_dividend_calendar_ref ON dividend_calendar(ref);

CREATE TABLE IF NOT EXISTS yield_settlement_cycles (
    market    TEXT     NOT NULL,
    from_day  DATE,
    days      INTEGER  NOT NULL,
    origin    TEXT     NOT NULL
);

-- The built-in cycles (canonical.BuiltinSettlementCycles).
INSERT INTO yield_settlement_cycles (market, from_day, days, origin)
SELECT market, from_day, days, 'builtin' FROM (
    SELECT 'US' AS market, c.from_day, c.days
      FROM (VALUES (NULL::DATE, 3), (DATE '2017-09-05', 2), (DATE '2024-05-28', 1)) c(from_day, days)
    UNION ALL
    SELECT 'CA', c.from_day, c.days
      FROM (VALUES (NULL::DATE, 3), (DATE '2017-09-05', 2), (DATE '2024-05-27', 1)) c(from_day, days)
    UNION ALL
    SELECT m.market, c.from_day, c.days
      FROM (VALUES ('AT'), ('BE'), ('BG'), ('CY'), ('DE'), ('EE'), ('ES'), ('FI'), ('FR'), ('GR'), ('HR'), ('IE'),
                   ('IT'), ('LT'), ('LU'), ('LV'), ('MT'), ('NL'), ('PT'), ('SI'), ('SK'),
                   ('CH'), ('GB'), ('SE'), ('DK'), ('NO')) m(market),
           (VALUES (DATE '2014-10-06', 2), (DATE '2027-10-11', 1)) c(from_day, days)
    UNION ALL
    SELECT 'JP', c.from_day, c.days
      FROM (VALUES (NULL::DATE, 3), (DATE '2019-07-16', 2)) c(from_day, days))
 WHERE NOT EXISTS (SELECT 1 FROM yield_settlement_cycles);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (147, CAST(epoch(now()) AS BIGINT));
