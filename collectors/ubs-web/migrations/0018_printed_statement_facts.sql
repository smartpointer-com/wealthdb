-- ============================================================
-- ubs-web silver schema, migration 0018 — the document rows state what
-- the documents print.
--
-- Four readings move from the PDF parsers to the gold adapter, which
-- applies them where it reads these rows:
--
--   * A Statement of assets' relationship overview prints a
--     "Precious metals & commodities" total per portfolio. It is a row
--     of statement_overview_totals, from every copy of the statement,
--     whichever currency the copy is valued in. The adapter turns the
--     USD copy into the portfolio's precious-metals position. It was a
--     synthetic position, from the USD copy only, in
--     historical_position_snapshots under the key 'PM-<portfolio>'.
--   * A maturity notice's principal is stored as printed, a positive
--     amount. The adapter gives the liability its sign.
--   * The loader reads a maturity notice's rate type off its product
--     name, as it does for the positions export. The payload no longer
--     repeats it.
--   * Whether a statement or advice movement is an internal transfer
--     is the adapter's first rule, read off the booking type and the
--     continuation lines the payload carries. The payload no longer
--     carries the parser's verdict, and an advice row names its own
--     document as its source.
--
-- The rows already loaded are rewritten to that shape here, from what
-- they hold. The next load re-derives them all anyway: the parsers'
-- generation has moved (migration 0009).
-- ============================================================

-- One overview total of one statement copy. currency_iso is the
-- currency the copy is valued in; market_value the total, as printed.
-- payload holds the printed line's portfolio number and figure.
CREATE TABLE statement_overview_totals (
    source_doc_token       TEXT    NOT NULL,  -- FK to documents.doc_token
    portfolio_external_id  TEXT    NOT NULL,  -- PSN-aligned, 16 chars
    asset_class            TEXT    NOT NULL,  -- 'precious_metals'
    as_of_date             INTEGER NOT NULL,  -- Unix seconds UTC
    currency_iso           TEXT,
    market_value           REAL    NOT NULL,
    payload                TEXT    NOT NULL,
    PRIMARY KEY (source_doc_token, portfolio_external_id, asset_class)
);

INSERT INTO statement_overview_totals (
    source_doc_token, portfolio_external_id, asset_class, as_of_date,
    currency_iso, market_value, payload)
SELECT source_doc_token, portfolio_external_id, 'precious_metals',
       as_of_date, market_value_currency, market_value, payload
  FROM historical_position_snapshots
 WHERE instrument_isin LIKE 'PM-%';

DELETE FROM historical_position_snapshots WHERE instrument_isin LIKE 'PM-%';

-- The principal was stored negated; the rate type was the payload's
-- last key.
UPDATE historical_mortgages
   SET outstanding_balance = abs(outstanding_balance),
       payload = CASE WHEN instr(payload, ', "rate_type": ') > 0
                      THEN substr(payload, 1,
                                  instr(payload, ', "rate_type": ') - 1) || '}'
                      ELSE payload END;

UPDATE transactions
   SET payload = replace(replace(payload,
                                 '"internal_transfer": false, ', ''),
                         '"internal_transfer": true, ', '')
 WHERE transaction_external_id LIKE 'stmt:%'
    OR payload LIKE '%"document": "payment_advice_pdf"%';

UPDATE transactions
   SET payload = replace(payload, '"source": "account_statement_pdf"',
                         '"source": "payment_advice_pdf"')
 WHERE payload LIKE '%"document": "payment_advice_pdf"%';

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (18, CAST(strftime('%s','now') AS INTEGER));
