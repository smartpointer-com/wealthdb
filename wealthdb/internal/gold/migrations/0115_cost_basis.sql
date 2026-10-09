-- ============================================================
-- gold schema, migration 0115 — cost basis: the stamp on a book
-- value, open lots and realized lots.
--
-- Each adapter that writes `positions.book_value` means something of
-- its own by it: a sum of tax lots or a weighted average, with or
-- without the purchase fees, printed by the source or computed from
-- what it prints. A basis that answers for some sources and not
-- others is worse than none when nothing on the page says which, so a
-- book value carries its stamp, all three columns set exactly when
-- book_value is (the gold writer enforces it, like the other gold
-- enums):
--
--   basis_origin  stated | derived | rebuilt | seeded
--   basis_method  lots | average | paid_in | acquisition_value | unknown
--   basis_fees    included | excluded | none | unknown
--
-- docs/DESIGN.md §7.4 defines each value.
--
-- `position_lots` holds a position's open lots, one row per lot,
-- beside the position row with the same source, snapshot, account and
-- position key. Snapshot grain: the load's windowed delete covers it.
--
-- `realized_lots` holds realized lots, or sales where a document
-- prints no lots, one row per lot per document: the same sale stated
-- by a 1099-B, its correction and a year-end summary is three rows.
-- `is_primary` marks the set that counts each sale once per account
-- and tax year. A sale's document arrives long after the sale, so no
-- change window fits the table; the load replaces a source's rows
-- whole whenever the source has changes.
--
-- Money is in `currency`; quantities in a realized lot, its proceeds
-- and its book value are magnitudes, the gain is signed. NULL means
-- the source does not state the figure.
--
-- IF NOT EXISTS keeps this replayable (see gold.Migrate's REPLAY
-- note).
-- ============================================================

ALTER TABLE positions ADD COLUMN IF NOT EXISTS basis_origin TEXT;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS basis_method TEXT;
ALTER TABLE positions ADD COLUMN IF NOT EXISTS basis_fees   TEXT;

CREATE TABLE IF NOT EXISTS position_lots (
    silver_source_id        TEXT           NOT NULL,
    snapshot_at             BIGINT         NOT NULL,
    account_external_id     TEXT           NOT NULL,
    position_key            TEXT           NOT NULL,
    lot_key                 TEXT           NOT NULL,
    instrument_external_id  TEXT,
    currency                TEXT           NOT NULL,
    quantity                DECIMAL(28, 8),
    book_value              DECIMAL(28, 4),
    market_value            DECIMAL(28, 4),
    acquisition_date        DATE,
    term                    TEXT,
    covered                 BOOLEAN,
    basis_origin            TEXT,
    source_document         TEXT,
    payload                 JSON,
    PRIMARY KEY (silver_source_id, snapshot_at, account_external_id, position_key, lot_key)
);

CREATE TABLE IF NOT EXISTS realized_lots (
    silver_source_id         TEXT           NOT NULL,
    realized_lot_external_id TEXT           NOT NULL,
    account_external_id      TEXT           NOT NULL,
    instrument_external_id   TEXT,
    instrument_hint          TEXT,
    description              TEXT,
    document_kind            TEXT           NOT NULL,
    tax_year                 INTEGER        NOT NULL,
    acquisition_date         DATE,
    acquired_various         BOOLEAN        NOT NULL,
    disposal_date            DATE,
    settlement_date          DATE,
    currency                 TEXT           NOT NULL,
    quantity                 DECIMAL(28, 8),
    proceeds                 DECIMAL(28, 4),
    book_value               DECIMAL(28, 4),
    realized_gain_loss       DECIMAL(28, 4),
    wash_sale_disallowed     DECIMAL(28, 4),
    accrued_market_discount  DECIMAL(28, 4),
    term                     TEXT,
    covered                  BOOLEAN,
    form_8949_box            TEXT,
    basis_origin             TEXT,
    basis_method             TEXT,
    basis_fees               TEXT,
    is_primary               BOOLEAN        NOT NULL,
    source_document          TEXT,
    payload                  JSON,
    PRIMARY KEY (silver_source_id, realized_lot_external_id)
);

CREATE INDEX IF NOT EXISTS ix_realized_lots_year
    ON realized_lots(silver_source_id, account_external_id, tax_year);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (115, CAST(epoch(now()) AS BIGINT));
