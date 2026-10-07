-- ============================================================
-- fidelity-web silver schema, migration 0010 — closed lots.
--
-- One row per realized lot, as a document prints it. Two documents
-- state them, each tagged by `document_kind`:
--
--   * '1099b' — the Form 1099-B pages of a Consolidated Form 1099
--     (`pdf_parsers_1099`). One row per lot: quantity, acquired and
--     sold dates, proceeds, cost basis, accrued market discount, wash
--     sale loss disallowed and gain or loss. The section a lot prints
--     under gives its Form 8949 box (A, B, D, E), and with it the
--     term and whether the basis is reported to the IRS (`covered`).
--     A section whose term is unknown names no single box, so
--     `form_8949_box` and `term` stay NULL there.
--   * 'statement' — the sales in a supplied monthly statement's
--     Securities Bought & Sold section (`pdf_parsers_supplied`). One
--     row per sale, not per lot: the printed basis, the term and gain
--     or loss from the line printed under the sale, the transaction
--     cost, and whether the security's basis follows Specific Share
--     identification. The statement prints the settlement date and no
--     acquired date, so `acquired_date` and `date_sold` stay NULL and
--     `settlement_date` is set. The sale's price stays in `payload`:
--     a bond prints it in percent of par, a share per unit.
--
-- The same sale can appear in both documents; silver keeps both.
--
-- Values are as printed, in USD. NULL means the document states
-- nothing: a blank cell, `-`, or `Unknown`. A lot's acquired date is
-- ISO, or the form's own word where it prints one (`Various`,
-- `Unknown`). Quantities and proceeds are magnitudes; a gain or loss,
-- and the statement's charges, carry the sign printed.
--
-- Identity. `lot_id` is derived from the row's own content plus an
-- occurrence index within its document, so the same lot read twice
-- converges. Fidelity re-renders a Consolidated 1099 on every download,
-- with new bytes, so a form is not keyed on its sha: the loader treats
-- each (account, tax year) as one form and keeps the rows of the one
-- prepared latest, a corrected form over the original. `form_prepared`
-- records that date and `source_sha256` the copy the rows came from.
--
-- The migration also files under `doc_kind = 'tax_form'` any 1099
-- catalogued as a statement: a Consolidated 1099 or a 1099-Q whose
-- name does not follow the `<YYYY>-<nickname>-<NNNN>-…` form, such as
-- `Consolidated_Form_1099.pdf`.
-- ============================================================

CREATE TABLE closed_lots (
    lot_id                   TEXT    NOT NULL PRIMARY KEY,
    document_kind            TEXT    NOT NULL
        CHECK (document_kind IN ('1099b', 'statement')),
    account_external_id      TEXT    NOT NULL,  -- 9-digit, no separator
    tax_year                 INTEGER,           -- 1099-B: the form's year
    form_prepared            TEXT,              -- 1099-B: ISO date the form was prepared
    description              TEXT    NOT NULL,  -- security name as printed
    instrument_key           TEXT,              -- symbol, else CUSIP, as printed
    cusip                    TEXT,              -- 1099-B only
    action                   TEXT    NOT NULL,  -- 'Sale', 'Cash In Lieu', 'You Sold', …
    quantity                 REAL,
    acquired_date            TEXT,              -- ISO, or 'Various' / 'Unknown'
    date_sold                TEXT,              -- ISO; 1099-B only
    settlement_date          TEXT,              -- ISO; statement only
    proceeds                 REAL,              -- 1099-B: gross proceeds; statement: transaction amount
    cost_basis               REAL,
    accrued_market_discount  REAL,
    wash_sale_disallowed     REAL,
    gain_loss                REAL,              -- signed
    fees                     REAL,              -- statement 'Transaction Cost', signed as printed
    federal_tax_withheld     REAL,
    term                     TEXT CHECK (term IN ('short', 'long')),
    covered                  INTEGER,           -- 1099-B: 1 basis reported to the IRS, 0 not
    form_8949_box            TEXT,              -- 1099-B: 'A' | 'B' | 'D' | 'E'
    specific_share_id        INTEGER,           -- statement: 1 when the sale is marked `s`
    corrected                INTEGER,           -- 1099-B: 1 when the lot is marked `!C`
    currency                 TEXT    NOT NULL DEFAULT 'USD',
    source_sha256            TEXT    NOT NULL,  -- sha256 of the source PDF
    payload                  TEXT    NOT NULL   -- raw parsed row JSON
);

CREATE INDEX ix_closed_lots_form
    ON closed_lots(document_kind, account_external_id, tax_year);
CREATE INDEX ix_closed_lots_instrument
    ON closed_lots(instrument_key);
CREATE INDEX ix_closed_lots_source
    ON closed_lots(source_sha256);

UPDATE documents
   SET doc_kind = 'tax_form'
 WHERE doc_kind = 'statement'
   AND file_name LIKE '%1099%';


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (10, CAST(strftime('%s', 'now') AS INTEGER));
