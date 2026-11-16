-- ============================================================
-- schwab-api silver schema, migration 0006 — promote what a reader
-- of positions and transactions needs from the payload.
--
-- positions gains the instrument and the figures of each holding:
--
--   asset_type, instrument_type — `instrument.assetType` and
--                     `instrument.type`, verbatim
--   cusip, symbol, description — `instrument.cusip`, `.symbol`,
--                     `.description`, verbatim
--   maturity_date   — `instrument.maturityDate`: the day it names, as
--                     unix seconds at 00:00 UTC
--   variable_rate   — `instrument.variableRate`, the coupon in percent
--   long_quantity, short_quantity, market_value — `longQuantity`,
--                     `shortQuantity`, `marketValue`
--
-- transaction_legs holds one row per element of a transaction's
-- `transferItems`, at its place in the array (`leg`, from 0):
-- `instrument.assetType`, `.cusip`, `.symbol` verbatim, and `amount`,
-- `cost`, `price`. A leg goes with its transaction.
--
-- A code is the source's text. A number is decimal text: the digits
-- the payload states, written out where the payload has an exponent.
-- A date that names no real day reads as no date. NULL means the
-- payload states no such field, or one of another JSON type.
--
-- Rows loaded before this migration are filled from payload here; the
-- loader writes the same values on every new row.
-- ============================================================

ALTER TABLE positions ADD COLUMN asset_type      TEXT;
ALTER TABLE positions ADD COLUMN instrument_type TEXT;
ALTER TABLE positions ADD COLUMN cusip           TEXT;
ALTER TABLE positions ADD COLUMN symbol          TEXT;
ALTER TABLE positions ADD COLUMN description     TEXT;
ALTER TABLE positions ADD COLUMN maturity_date   INTEGER;
ALTER TABLE positions ADD COLUMN variable_rate   TEXT;
ALTER TABLE positions ADD COLUMN long_quantity   TEXT;
ALTER TABLE positions ADD COLUMN short_quantity  TEXT;
ALTER TABLE positions ADD COLUMN market_value    TEXT;

CREATE TABLE transaction_legs (
    activity_id TEXT    NOT NULL
        REFERENCES transactions(activity_id) ON DELETE CASCADE,
    leg         INTEGER NOT NULL,
    asset_type  TEXT,
    cusip       TEXT,
    symbol      TEXT,
    amount      TEXT,
    cost        TEXT,
    price       TEXT,
    PRIMARY KEY (activity_id, leg)
);

-- The text of a code; the JSON literal of a number (`->` keeps it as
-- the payload states it). maturity_date holds the stamp's text until
-- the day is read from it below.
UPDATE positions SET
    asset_type = CASE WHEN json_type(payload, '$.instrument.assetType') = 'text'
        THEN json_extract(payload, '$.instrument.assetType') END,
    instrument_type = CASE WHEN json_type(payload, '$.instrument.type') = 'text'
        THEN json_extract(payload, '$.instrument.type') END,
    cusip = CASE WHEN json_type(payload, '$.instrument.cusip') = 'text'
        THEN json_extract(payload, '$.instrument.cusip') END,
    symbol = CASE WHEN json_type(payload, '$.instrument.symbol') = 'text'
        THEN json_extract(payload, '$.instrument.symbol') END,
    description = CASE WHEN json_type(payload, '$.instrument.description') = 'text'
        THEN json_extract(payload, '$.instrument.description') END,
    maturity_date = CASE WHEN json_type(payload, '$.instrument.maturityDate') = 'text'
        THEN json_extract(payload, '$.instrument.maturityDate') END,
    variable_rate = CASE WHEN json_type(payload, '$.instrument.variableRate') IN ('integer', 'real')
        THEN payload -> '$.instrument.variableRate' END,
    long_quantity = CASE WHEN json_type(payload, '$.longQuantity') IN ('integer', 'real')
        THEN payload -> '$.longQuantity' END,
    short_quantity = CASE WHEN json_type(payload, '$.shortQuantity') IN ('integer', 'real')
        THEN payload -> '$.shortQuantity' END,
    market_value = CASE WHEN json_type(payload, '$.marketValue') IN ('integer', 'real')
        THEN payload -> '$.marketValue' END;

UPDATE positions SET maturity_date = CASE
        WHEN substr(maturity_date, 1, 10) GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'
         AND substr(maturity_date, 1, 4) <> '0000'
         AND date(substr(maturity_date, 1, 10)) = substr(maturity_date, 1, 10)
        THEN CAST(strftime('%s', substr(maturity_date, 1, 10)) AS INTEGER) END
 WHERE maturity_date IS NOT NULL;

INSERT INTO transaction_legs
    (activity_id, leg, asset_type, cusip, symbol, amount, cost, price)
SELECT t.activity_id, j.key,
       CASE WHEN json_type(j.value, '$.instrument.assetType') = 'text'
           THEN json_extract(j.value, '$.instrument.assetType') END,
       CASE WHEN json_type(j.value, '$.instrument.cusip') = 'text'
           THEN json_extract(j.value, '$.instrument.cusip') END,
       CASE WHEN json_type(j.value, '$.instrument.symbol') = 'text'
           THEN json_extract(j.value, '$.instrument.symbol') END,
       CASE WHEN json_type(j.value, '$.amount') IN ('integer', 'real')
           THEN j.value -> '$.amount' END,
       CASE WHEN json_type(j.value, '$.cost') IN ('integer', 'real')
           THEN j.value -> '$.cost' END,
       CASE WHEN json_type(j.value, '$.price') IN ('integer', 'real')
           THEN j.value -> '$.price' END
  FROM transactions t, json_each(t.payload, '$.transferItems') j
 WHERE json_type(t.payload, '$.transferItems') = 'array'
   AND j.type = 'object';

-- A literal with an exponent (`1e-05`, the form the payload's JSON
-- gives a very small or very large float) written out as plain decimal
-- text, the digits unchanged: `0.00001`.
CREATE TEMP TABLE exponent_form (
    lit      TEXT PRIMARY KEY,
    mantissa TEXT,
    ex       INTEGER,
    digits   TEXT,
    point    INTEGER,   -- where the decimal point falls in digits
    plain    TEXT
);
INSERT OR IGNORE INTO exponent_form (lit)
          SELECT variable_rate  FROM positions        WHERE variable_rate  LIKE '%e%'
    UNION SELECT long_quantity  FROM positions        WHERE long_quantity  LIKE '%e%'
    UNION SELECT short_quantity FROM positions        WHERE short_quantity LIKE '%e%'
    UNION SELECT market_value   FROM positions        WHERE market_value   LIKE '%e%'
    UNION SELECT amount         FROM transaction_legs WHERE amount         LIKE '%e%'
    UNION SELECT cost           FROM transaction_legs WHERE cost           LIKE '%e%'
    UNION SELECT price          FROM transaction_legs WHERE price          LIKE '%e%';
UPDATE exponent_form SET
    mantissa = ltrim(substr(lit, 1, instr(lower(lit), 'e') - 1), '-'),
    ex = CAST(substr(lit, instr(lower(lit), 'e') + 1) AS INTEGER);
UPDATE exponent_form SET
    digits = replace(mantissa, '.', ''),
    point = ex + CASE WHEN instr(mantissa, '.') > 0
        THEN instr(mantissa, '.') - 1 ELSE length(mantissa) END;
UPDATE exponent_form SET plain =
    CASE WHEN lit LIKE '-%' THEN '-' ELSE '' END || CASE
        WHEN point <= 0
            THEN '0.' || replace(printf('%*s', -point, ''), ' ', '0') || digits
        WHEN point >= length(digits)
            THEN digits || replace(printf('%*s', point - length(digits), ''), ' ', '0')
        ELSE substr(digits, 1, point) || '.' || substr(digits, point + 1)
    END;

UPDATE positions SET variable_rate = (SELECT plain FROM exponent_form WHERE lit = variable_rate)
 WHERE variable_rate LIKE '%e%';
UPDATE positions SET long_quantity = (SELECT plain FROM exponent_form WHERE lit = long_quantity)
 WHERE long_quantity LIKE '%e%';
UPDATE positions SET short_quantity = (SELECT plain FROM exponent_form WHERE lit = short_quantity)
 WHERE short_quantity LIKE '%e%';
UPDATE positions SET market_value = (SELECT plain FROM exponent_form WHERE lit = market_value)
 WHERE market_value LIKE '%e%';
UPDATE transaction_legs SET amount = (SELECT plain FROM exponent_form WHERE lit = amount)
 WHERE amount LIKE '%e%';
UPDATE transaction_legs SET cost = (SELECT plain FROM exponent_form WHERE lit = cost)
 WHERE cost LIKE '%e%';
UPDATE transaction_legs SET price = (SELECT plain FROM exponent_form WHERE lit = price)
 WHERE price LIKE '%e%';
DROP TABLE exponent_form;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (6, CAST(strftime('%s','now') AS INTEGER));
