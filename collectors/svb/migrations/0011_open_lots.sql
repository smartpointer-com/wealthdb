-- ============================================================
-- fidelity-web silver schema, migration 0011 — open lots.
--
-- One row per open tax lot, as the positions page's lot table prints
-- it. The `download` positions phase fetches that table for each new
-- or changed position (DESIGN.md §8.3.1); `snapshot_at` is the dump
-- that fetched it. A position whose quantity and cost basis total are
-- unchanged is not fetched again, so its lots stay under the earlier
-- dump: the lots of a holding at time T are the rows of the latest
-- fetch at or before T.
--
-- Columns follow schwab-web's `open_lots`:
--
--   * `quantity`, `unit_cost` (the table's "Average cost basis"),
--     `cost_basis` ("Cost basis total"), `unrealized_gain_loss`
--     ("$ Total gain/loss") and `current_value`, in USD as printed. A
--     bond's unit cost is per 100 of par, as the page prints it.
--   * `acquired_date` is ISO; text the table prints instead of a
--     date stays as printed.
--   * `term` is 'SHORT' or 'LONG', the holding period printed.
--   * `covered` stays NULL: the lot table does not state it.
--
-- NULL means the table states nothing (`--`). `payload` keeps every
-- cell as printed, the percent gain included.
-- ============================================================

CREATE TABLE open_lots (
    snapshot_at          INTEGER NOT NULL,   -- the dump that fetched the lots
    account_external_id  TEXT    NOT NULL,   -- 9-digit, no separator
    instrument_key       TEXT    NOT NULL,   -- the position's key in `positions`
    lot_index            INTEGER NOT NULL,   -- 0-based print order across the table's pages
    cusip                TEXT,
    quantity             REAL,
    unit_cost            REAL,
    cost_basis           REAL,
    acquired_date        TEXT,               -- ISO YYYY-MM-DD
    unrealized_gain_loss REAL,               -- signed
    current_value        REAL,
    term                 TEXT,               -- 'SHORT' | 'LONG'
    covered              INTEGER,            -- not stated by the source; NULL
    currency             TEXT    NOT NULL DEFAULT 'USD',
    source_sha256        TEXT    NOT NULL,   -- sha256 of the table page the lot came from
    payload              TEXT    NOT NULL,   -- {cells, page}
    PRIMARY KEY (snapshot_at, account_external_id, instrument_key, lot_index)
);

CREATE INDEX ix_open_lots_holding
    ON open_lots(account_external_id, instrument_key, snapshot_at);


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (11, CAST(strftime('%s', 'now') AS INTEGER));
