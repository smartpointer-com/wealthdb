-- ============================================================
-- schwab-web silver schema, migration 0006 — open tax lots.
--
-- The 2020-2024 statement layout prints one line per tax lot under
-- each holding: units purchased, cost per share, cost basis, acquired
-- date, unrealized gain and, on most statements, the holding days and
-- the holding period. `open_lots` holds one row per printed lot, keyed
-- like the holding it belongs to in `historical_position_snapshots`
-- (as_of_date, account_external_id, instrument_key) plus the lot's
-- place in print order. Statements of other layouts print no lots.
--
-- Every figure is as printed. NULL means the statement does not print
-- it:
--   * acquired_date is NULL on the reinvested-dividend summary lot
--     (endnote "r"), which prints no date;
--   * unit_cost and cost_basis are NULL on a lot whose basis Schwab
--     does not know ("N/A please provide");
--   * term is NULL on statements without the holding-period column;
--   * covered is NULL throughout: statements do not say whether a lot
--     is covered.
-- A short lot carries a negative quantity and a negative cost basis,
-- as the short holding does. An option lot's unit cost is per share
-- of the underlying, as printed, not per contract.
--
-- The statement passes write this table together with the holding
-- rows, so it follows their life cycle: a statement's lots are
-- replaced whenever its holdings are re-parsed, and the purge on a
-- moved parser generation (migration 0005) clears it with the other
-- statement tables.
--
-- No backfill: silver has never held the lot lines. The parser change
-- that fills this table moves the parser generation, so the next load
-- re-parses every statement.
-- ============================================================

CREATE TABLE open_lots (
    as_of_date           INTEGER NOT NULL,   -- statement period_end, Unix sec UTC midnight
    account_external_id  TEXT    NOT NULL,   -- account suffix, e.g. "NNN"
    instrument_key       TEXT    NOT NULL,   -- the holding's key in historical_position_snapshots
    lot_index            INTEGER NOT NULL,   -- 0-based print order within the holding
    quantity             REAL,               -- units purchased; negative for a short lot
    unit_cost            REAL,               -- cost per share
    cost_basis           REAL,
    acquired_date        TEXT,               -- ISO YYYY-MM-DD
    unrealized_gain_loss REAL,
    term                 TEXT,               -- 'SHORT' | 'LONG', the holding period printed
    covered              INTEGER,            -- 1 covered, 0 noncovered; NULL not stated
    footnotes            TEXT,               -- endnote markers as printed, comma-joined: t, e, r, S
    source_sha256        TEXT    NOT NULL,   -- bronze PDF the lot was parsed from
    payload              TEXT    NOT NULL,   -- {holding_days, raw_line}
    PRIMARY KEY (as_of_date, account_external_id, instrument_key, lot_index)
);
CREATE INDEX ix_open_lots_account_instrument
    ON open_lots(account_external_id, instrument_key, as_of_date);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (6, CAST(strftime('%s','now') AS INTEGER));
