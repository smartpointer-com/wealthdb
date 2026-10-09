-- ============================================================
-- migration 0006 — positions and transactions: cost-basis columns.
--
-- The figures a cost-basis adapter reads become columns. Each column
-- holds the source's own figure and unit; nothing is converted or
-- rescaled. NULL means the source states no figure. The payload keeps
-- the full row.
--
-- positions
--   average_cost              cost per unit, in the row's currency.
--                             Live rows: the Positions export's
--                             "Unit cost". Statement rows: the
--                             Portfolio Performance "Average price".
--   price_quote               'unit' or 'percent' (of nominal). Says
--                             how average_cost and the row's market
--                             price are quoted. The statement prints
--                             bond prices in percent of nominal; the
--                             Positions export prints every price per
--                             unit, a bond's per unit of nominal.
--   market_value_chf          the Positions export's "Total value CHF".
--   unrealized_gain_loss_chf  the Positions export's "P&L Nominal CHF".
--                             market_value_chf minus this column is the
--                             position's cost in CHF at the FX rates of
--                             its purchases.
--   Statement rows leave both CHF columns NULL. The statement states
--   no P&L; its CHF valuation stays in payload.
--
-- transactions (the CSV export's cells, on every row type)
--   quantity          "Quantity".
--   unit_price        "Unit price", without its "%" marker.
--   price_quote       'percent' when the export marks the unit price
--                     with "%" (a bond trade, in percent of nominal);
--                     'unit' otherwise; NULL when no unit price is
--                     stated.
--   fees              "Costs": the row's charges, as printed.
--   accrued_interest  "Accrued Interest", signed as printed.
--   A blank cell, or one with no digit in it, is NULL. A printed zero
--   stays 0.
--
-- Backfill: every value is already in payload, so the UPDATEs below
-- fill the columns for rows loaded before this migration. A
-- statement row's payload keeps the average price without its "%"
-- marker, so the backfill reads percent from the asset class: the
-- statement's bond section is the one it prints in percent. The
-- loader reads the marker itself for rows it writes.
-- ============================================================

ALTER TABLE positions ADD COLUMN average_cost             REAL;
ALTER TABLE positions ADD COLUMN price_quote              TEXT
    CHECK (price_quote IN ('unit', 'percent'));
ALTER TABLE positions ADD COLUMN market_value_chf         REAL;
ALTER TABLE positions ADD COLUMN unrealized_gain_loss_chf REAL;

ALTER TABLE transactions ADD COLUMN quantity         REAL;
ALTER TABLE transactions ADD COLUMN unit_price       REAL;
ALTER TABLE transactions ADD COLUMN price_quote      TEXT
    CHECK (price_quote IN ('unit', 'percent'));
ALTER TABLE transactions ADD COLUMN fees             REAL;
ALTER TABLE transactions ADD COLUMN accrued_interest REAL;

-- Live rows: the XLS cells are numbers, or '' when blank.
UPDATE positions SET
    average_cost = CASE
        WHEN json_type(payload, '$.unit_cost') IN ('integer', 'real')
        THEN json_extract(payload, '$.unit_cost') END,
    price_quote = 'unit',
    market_value_chf = CASE
        WHEN json_type(payload, '$.total_value_chf') IN ('integer', 'real')
        THEN json_extract(payload, '$.total_value_chf') END,
    unrealized_gain_loss_chf = CASE
        WHEN json_type(payload, '$.pl_nominal_chf') IN ('integer', 'real')
        THEN json_extract(payload, '$.pl_nominal_chf') END
WHERE source = 'live';

-- Statement rows: the parser stores numbers.
UPDATE positions SET
    average_cost = CASE
        WHEN json_type(payload, '$.avg_price') IN ('integer', 'real')
        THEN json_extract(payload, '$.avg_price') END,
    price_quote = CASE json_extract(payload, '$.asset_class')
        WHEN 'Bonds' THEN 'percent' ELSE 'unit' END
WHERE source LIKE 'pp:%';

-- Transactions: the CSV cells are text. A number with a decimal comma
-- reads the same way net_amount does.
UPDATE transactions SET
    quantity = CASE
        WHEN json_extract(payload, '$.quantity') GLOB '*[0-9]*'
        THEN CAST(replace(trim(json_extract(payload, '$.quantity')),
                          ',', '.') AS REAL) END,
    unit_price = CASE
        WHEN json_extract(payload, '$.unit_price') GLOB '*[0-9]*'
        THEN CAST(replace(rtrim(trim(json_extract(payload, '$.unit_price')),
                                ' %'),
                          ',', '.') AS REAL) END,
    price_quote = CASE
        WHEN json_extract(payload, '$.unit_price') GLOB '*[0-9]*'
        THEN CASE
            WHEN trim(json_extract(payload, '$.unit_price')) GLOB '*%'
            THEN 'percent' ELSE 'unit' END
        END,
    fees = CASE
        WHEN json_extract(payload, '$.costs') GLOB '*[0-9]*'
        THEN CAST(replace(trim(json_extract(payload, '$.costs')),
                          ',', '.') AS REAL) END,
    accrued_interest = CASE
        WHEN json_extract(payload, '$.accrued_interest') GLOB '*[0-9]*'
        THEN CAST(replace(trim(json_extract(payload, '$.accrued_interest')),
                          ',', '.') AS REAL) END;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (6, CAST(strftime('%s', 'now') AS INTEGER));
