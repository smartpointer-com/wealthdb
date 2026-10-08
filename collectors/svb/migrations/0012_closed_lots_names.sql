-- ============================================================
-- fidelity-web silver schema, migration 0012 — closed lots in
-- schwab-web's names, and the positions page's closed lots.
--
-- `closed_lots` takes the column names and values schwab-web's
-- `closed_lots` uses for the same facts, so an adapter reads both
-- with one query:
--
--   * `description` → `security_name`, `date_sold` → `disposed_date`,
--     `gain_loss` → `realized_gain_loss`.
--   * `document_kind` '1099b' → 'form_1099b'.
--   * `term` 'short' / 'long' → 'SHORT' / 'LONG'.
--
-- A third `document_kind`, 'closed_positions', holds the lots the
-- positions page lists for a tax year's closed positions (DESIGN.md
-- §8.3.1). One row per lot: quantity, acquired and sold dates,
-- proceeds, cost basis, and the gain the page prints under its short-
-- or long-term column, which gives `term`. It names no action, so
-- `action` becomes nullable; `security_name` follows schwab-web and is
-- nullable too.
--
-- SQLite cannot alter a CHECK constraint, so the table is rebuilt.
-- Every row keeps its `lot_id`.
-- ============================================================

CREATE TABLE closed_lots_new (
    lot_id                   TEXT    NOT NULL PRIMARY KEY,
    document_kind            TEXT    NOT NULL
        CHECK (document_kind IN ('form_1099b', 'statement',
                                 'closed_positions')),
    account_external_id      TEXT    NOT NULL,  -- 9-digit, no separator
    tax_year                 INTEGER,           -- 1099-B: the form's year; closed_positions: the year queried
    form_prepared            TEXT,              -- 1099-B: ISO date the form was prepared
    security_name            TEXT,              -- as printed
    instrument_key           TEXT,              -- symbol, else CUSIP, as printed
    cusip                    TEXT,              -- 1099-B and closed_positions
    action                   TEXT,              -- 'Sale', 'Cash In Lieu', 'You Sold', …; NULL for closed_positions
    quantity                 REAL,
    acquired_date            TEXT,              -- ISO, or 'Various' / 'Unknown'
    disposed_date            TEXT,              -- ISO; 1099-B and closed_positions
    settlement_date          TEXT,              -- ISO; statement only
    proceeds                 REAL,              -- 1099-B: gross proceeds; statement: transaction amount
    cost_basis               REAL,
    accrued_market_discount  REAL,
    wash_sale_disallowed     REAL,
    realized_gain_loss       REAL,              -- signed
    fees                     REAL,              -- statement 'Transaction Cost', signed as printed
    federal_tax_withheld     REAL,
    term                     TEXT CHECK (term IN ('SHORT', 'LONG')),
    covered                  INTEGER,           -- 1099-B: 1 basis reported to the IRS, 0 not
    form_8949_box            TEXT,              -- 1099-B: 'A' | 'B' | 'D' | 'E'
    specific_share_id        INTEGER,           -- statement: 1 when the sale is marked `s`
    corrected                INTEGER,           -- 1099-B: 1 when the lot is marked `!C`
    currency                 TEXT    NOT NULL DEFAULT 'USD',
    source_sha256            TEXT    NOT NULL,  -- sha256 of the source document
    payload                  TEXT    NOT NULL   -- raw parsed row JSON
);

INSERT INTO closed_lots_new (
    lot_id, document_kind, account_external_id, tax_year, form_prepared,
    security_name, instrument_key, cusip, action, quantity, acquired_date,
    disposed_date, settlement_date, proceeds, cost_basis,
    accrued_market_discount, wash_sale_disallowed, realized_gain_loss, fees,
    federal_tax_withheld, term, covered, form_8949_box, specific_share_id,
    corrected, currency, source_sha256, payload)
SELECT lot_id,
       CASE document_kind WHEN '1099b' THEN 'form_1099b' ELSE document_kind END,
       account_external_id, tax_year, form_prepared,
       description, instrument_key, cusip, action, quantity, acquired_date,
       date_sold, settlement_date, proceeds, cost_basis,
       accrued_market_discount, wash_sale_disallowed, gain_loss, fees,
       federal_tax_withheld, UPPER(term), covered, form_8949_box,
       specific_share_id, corrected, currency, source_sha256, payload
  FROM closed_lots;

DROP TABLE closed_lots;
ALTER TABLE closed_lots_new RENAME TO closed_lots;

CREATE INDEX ix_closed_lots_form
    ON closed_lots(document_kind, account_external_id, tax_year);
CREATE INDEX ix_closed_lots_instrument
    ON closed_lots(instrument_key);
CREATE INDEX ix_closed_lots_source
    ON closed_lots(source_sha256);


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (12, CAST(strftime('%s', 'now') AS INTEGER));
