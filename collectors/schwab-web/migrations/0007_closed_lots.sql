-- ============================================================
-- schwab-web silver schema, migration 0007 — realized lots and
-- cost-basis methods.
--
-- `closed_lots` holds one row per realized lot that a year-end tax
-- document prints. Three documents feed it, and `document_kind` says
-- which:
--
--   'form_1099b'         the 1099-B lots of the 1099 Composite XML (or
--                        CSV), the same parse that writes the
--                        `form_1099b` rows of `transactions`;
--   'year_end_summary'   the "Realized Gain or (Loss)" sections of the
--                        Year-End Summary, a PDF of its own or the
--                        second half of the 1099 Composite PDF. They
--                        include the sections "not reported on Form
--                        1099-B", which carry the basis the 1099-B
--                        leaves out;
--   'gain_loss_report'   the Year-End Gain/Loss Report, a PDF that
--                        lists the realized lots of accounts without a
--                        1099-B as well.
--
-- The same lot can appear in more than one document: a 1099-B lot is
-- also in that year's Year-End Summary, and a corrected 1099 repeats
-- the original. Silver keeps every copy; gold reconciles them.
--
-- Every figure is as printed. NULL means the document does not print
-- it:
--   * realized_gain_loss is NULL on 1099-B rows, which print none;
--   * cost_basis is NULL where the document prints no basis ("Missing",
--     or the 1099-B's placeholder 0.00 on a noncovered lot whose basis
--     is not shown);
--   * wash_sale_disallowed is NULL where the document prints "--" or
--     has no such column (Gain/Loss Report);
--   * covered is 1 for covered lots, 0 for noncovered ones (on 1099-B
--     rows, from its noncovered flag), NULL where the document does not
--     say (the Year-End Summary's "not reported" sections, the
--     Gain/Loss Report);
--   * form_8949_box is the box letter as printed, comma-joined when a
--     section names two ("C,F").
-- acquired_date is ISO, or 'Various' as printed. A short sale keeps the
-- printed, unsigned quantity and carries the endnote `S`.
--
-- `cost_basis_methods` holds the cost-basis methods a Gain/Loss Report
-- prints for its account, one row per asset class as printed
-- (asset class "Mutual Funds", method "First In First Out").
--
-- Both tables key on the document's logical_doc_key
-- (account|doc_date|filename, the base filename for the 1099 XML/CSV
-- twins). Every parse of a document replaces its rows, so a
-- re-downloaded copy or a re-parse never adds to them.
--
-- Backfill: the 1099-B lots are already in silver, as the payload of
-- the `form_1099b` rows of `transactions`; the INSERT below copies
-- them, numbered in insertion order, which is print order.
-- `transactions` holds a lot that a corrected 1099 repeats only once,
-- so the copy lacks those repeats until the document is next parsed.
-- The two PDF reports have never been read; their rows come from the
-- re-parse that the moved parser generation forces.
-- ============================================================

CREATE TABLE closed_lots (
    logical_doc_key          TEXT    NOT NULL,   -- the source document's account|doc_date|filename
    document_kind            TEXT    NOT NULL,   -- 'form_1099b' | 'year_end_summary' | 'gain_loss_report'
    lot_index                INTEGER NOT NULL,   -- 0-based print order within the document
    account_external_id      TEXT    NOT NULL,   -- account suffix, e.g. "NNN"
    tax_year                 INTEGER,
    security_name            TEXT,               -- as printed
    cusip                    TEXT,
    instrument_key           TEXT,               -- CUSIP, ticker, or an option's contract in the statements' form
    quantity                 REAL,
    acquired_date            TEXT,               -- ISO YYYY-MM-DD | 'Various'
    disposed_date            TEXT,               -- ISO YYYY-MM-DD
    proceeds                 REAL,
    cost_basis               REAL,
    wash_sale_disallowed     REAL,
    accrued_market_discount  REAL,
    realized_gain_loss       REAL,
    term                     TEXT,               -- 'SHORT' | 'LONG' | another 1099-B label as printed
    covered                  INTEGER,            -- 1 covered, 0 noncovered; NULL not stated
    form_8949_box            TEXT,
    footnotes                TEXT,               -- endnote markers as printed, comma-joined: S, t, ...
    source_sha256            TEXT    NOT NULL,   -- bronze document the lot was parsed from
    payload                  TEXT    NOT NULL,   -- the parsed lot, raw lines included
    PRIMARY KEY (logical_doc_key, document_kind, lot_index)
);
CREATE INDEX ix_closed_lots_account_disposed
    ON closed_lots(account_external_id, disposed_date);
CREATE INDEX ix_closed_lots_tax_year
    ON closed_lots(account_external_id, tax_year, document_kind);

CREATE TABLE cost_basis_methods (
    logical_doc_key      TEXT    NOT NULL,   -- the Gain/Loss Report's account|doc_date|filename
    asset_class          TEXT    NOT NULL,   -- as printed, e.g. "Mutual Funds"
    account_external_id  TEXT    NOT NULL,
    as_of_date           INTEGER NOT NULL,   -- the report's document date, Unix sec UTC midnight
    tax_year             INTEGER,
    method               TEXT    NOT NULL,   -- as printed, e.g. "High Cost"
    source_sha256        TEXT    NOT NULL,
    PRIMARY KEY (logical_doc_key, asset_class)
);
CREATE INDEX ix_cost_basis_methods_account
    ON cost_basis_methods(account_external_id, as_of_date);

INSERT INTO closed_lots (
    logical_doc_key, document_kind, lot_index, account_external_id,
    tax_year, security_name, quantity, acquired_date, disposed_date,
    proceeds, cost_basis, wash_sale_disallowed, accrued_market_discount,
    term, covered, form_8949_box, source_sha256, payload)
SELECT logical_doc_key, 'form_1099b',
       ROW_NUMBER() OVER (PARTITION BY logical_doc_key ORDER BY rowid) - 1,
       account_external_id,
       json_extract(payload, '$.tax_year'),
       json_extract(payload, '$.security_name'),
       json_extract(payload, '$.quantity'),
       json_extract(payload, '$.acquired_date'),
       json_extract(payload, '$.date_sold'),
       json_extract(payload, '$.proceeds'),
       json_extract(payload, '$.cost_basis'),
       json_extract(payload, '$.wash_sale_disallowed'),
       json_extract(payload, '$.accrued_market_discount'),
       json_extract(payload, '$.term'),
       CASE json_extract(payload, '$.noncovered') WHEN 1 THEN 0 WHEN 0 THEN 1 END,
       json_extract(payload, '$.form_8949_code'),
       source_sha256, payload
FROM transactions
WHERE source = 'form_1099b' AND logical_doc_key IS NOT NULL;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (7, CAST(strftime('%s','now') AS INTEGER));
