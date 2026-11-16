-- ============================================================
-- schwab-web silver schema, migration 0014 — a transaction's figures and
-- words as columns.
--
-- `transactions` gains what the payload states in three dialects (the
-- statement's numbers, the tx-history export's "$1,000.00" strings, and
-- a distribution letter's market value or cash amount):
--
--   amount       the row's amount as decimal text; on a distribution
--                letter without one, its market value or cash amount
--   quantity     the quantity as decimal text
--   price        the price as decimal text
--   description  the description as printed, trimmed
--   action       the movement the row states, trimmed: the export's
--                `Action`, a statement row's `action`, else its
--                `category`; never the statement parser's "Unknown"
--
-- A number is its JSON text. An export's string loses its "$", its
-- thousands commas and its spaces, and its parentheses or leading minus
-- become one minus; a string that is then no decimal is NULL. A payload
-- whose figure keys hold another JSON type has NULL figures. The loader
-- writes the same values for every row it inserts
-- (load._transaction_columns); this backfills the rows in hand.
-- ============================================================

BEGIN;

ALTER TABLE transactions ADD COLUMN amount TEXT;
ALTER TABLE transactions ADD COLUMN quantity TEXT;
ALTER TABLE transactions ADD COLUMN price TEXT;
ALTER TABLE transactions ADD COLUMN description TEXT;
ALTER TABLE transactions ADD COLUMN action TEXT;

-- Whitespace as Go's strings.TrimSpace reads it.
CREATE TEMP TABLE ws AS SELECT char(9, 10, 11, 12, 13, 32, 133, 160, 5760,
    8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199, 8200, 8201, 8202, 8232,
    8233, 8239, 8287, 12288) AS chars;

-- The export's figure strings, cleaned to sign and digits.
CREATE TEMP TABLE figure_text AS
WITH f(activity_id, field, v) AS (
    SELECT activity_id, k.field,
           trim(json_extract(payload, '$.' || k.field), (SELECT chars FROM ws))
      FROM transactions, (SELECT 'Amount' AS field UNION ALL
                          SELECT 'Quantity' UNION ALL SELECT 'Price') AS k
     WHERE json_type(payload, '$.' || k.field) = 'text'
), p AS (
    SELECT activity_id, field, v,
           (length(v) >= 2 AND substr(v, 1, 1) = '('
            AND substr(v, -1, 1) = ')') AS paren
      FROM f
), q AS (
    SELECT activity_id, field, paren,
           replace(replace(replace(
               CASE WHEN paren THEN substr(v, 2, length(v) - 2) ELSE v END,
               '$', ''), ',', ''), ' ', '') AS r
      FROM p
), n AS (
    SELECT activity_id, field, (paren <> (substr(r, 1, 1) = '-')) AS neg,
           CASE WHEN substr(r, 1, 1) = '-' THEN substr(r, 2) ELSE r END AS d
      FROM q
)
SELECT activity_id, field,
       CASE WHEN neg THEN '-' ELSE '' END || d AS text
  FROM n
 WHERE d <> '' AND d NOT GLOB '*[^0-9.]*' AND d GLOB '*[0-9]*'
   AND length(d) - length(replace(d, '.', '')) <= 1;
CREATE INDEX temp.ix_figure_text ON figure_text(activity_id, field);

UPDATE transactions SET
    amount = CASE WHEN src.typed THEN coalesce(
        src.amount_num,
        (SELECT text FROM figure_text t
          WHERE t.activity_id = src.activity_id AND t.field = 'Amount'),
        src.market_value_num, src.cash_amount_num) END,
    quantity = CASE WHEN src.typed THEN coalesce(
        src.quantity_num,
        (SELECT text FROM figure_text t
          WHERE t.activity_id = src.activity_id AND t.field = 'Quantity')) END,
    price = CASE WHEN src.typed THEN coalesce(
        src.price_num,
        (SELECT text FROM figure_text t
          WHERE t.activity_id = src.activity_id AND t.field = 'Price')) END,
    description = CASE WHEN src.described THEN coalesce(
        nullif(trim(CASE WHEN json_type(src.payload, '$.description') = 'text'
                         THEN json_extract(src.payload, '$.description') END,
                    (SELECT chars FROM ws)), ''),
        nullif(trim(CASE WHEN json_type(src.payload, '$.Description') = 'text'
                         THEN json_extract(src.payload, '$.Description') END,
                    (SELECT chars FROM ws)), '')) END,
    action = (SELECT w FROM (
        SELECT 1 AS o, trim(CASE WHEN json_type(src.payload, '$.Action') = 'text'
                                 THEN json_extract(src.payload, '$.Action') END,
                            (SELECT chars FROM ws)) AS w
        UNION ALL
        SELECT 2, trim(CASE WHEN json_type(src.payload, '$.action') = 'text'
                            THEN json_extract(src.payload, '$.action') END,
                       (SELECT chars FROM ws))
        UNION ALL
        SELECT 3, trim(CASE WHEN json_type(src.payload, '$.category') = 'text'
                            THEN json_extract(src.payload, '$.category') END,
                       (SELECT chars FROM ws)))
        WHERE w <> '' AND lower(w) <> 'unknown' ORDER BY o LIMIT 1)
FROM (
    SELECT activity_id, payload,
           coalesce(json_type(payload, '$.amount'), 'null')
               IN ('integer', 'real', 'null')
           AND coalesce(json_type(payload, '$.quantity'), 'null')
               IN ('integer', 'real', 'null')
           AND coalesce(json_type(payload, '$.price'), 'null')
               IN ('integer', 'real', 'null')
           AND coalesce(json_type(payload, '$.market_value'), 'null')
               IN ('integer', 'real', 'null')
           AND coalesce(json_type(payload, '$.cash_amount'), 'null')
               IN ('integer', 'real', 'null')
           AND coalesce(json_type(payload, '$.Amount'), 'null')
               IN ('text', 'null')
           AND coalesce(json_type(payload, '$.Quantity'), 'null')
               IN ('text', 'null')
           AND coalesce(json_type(payload, '$.Price'), 'null')
               IN ('text', 'null') AS typed,
           coalesce(json_type(payload, '$.description'), 'null')
               IN ('text', 'null')
           AND coalesce(json_type(payload, '$.Description'), 'null')
               IN ('text', 'null') AS described,
           CASE WHEN json_type(payload, '$.amount') IN ('integer', 'real')
                THEN payload -> '$.amount' END AS amount_num,
           CASE WHEN json_type(payload, '$.quantity') IN ('integer', 'real')
                THEN payload -> '$.quantity' END AS quantity_num,
           CASE WHEN json_type(payload, '$.price') IN ('integer', 'real')
                THEN payload -> '$.price' END AS price_num,
           CASE WHEN json_type(payload, '$.market_value') IN ('integer', 'real')
                THEN payload -> '$.market_value' END AS market_value_num,
           CASE WHEN json_type(payload, '$.cash_amount') IN ('integer', 'real')
                THEN payload -> '$.cash_amount' END AS cash_amount_num
      FROM transactions
) AS src
WHERE transactions.activity_id = src.activity_id;

DROP TABLE figure_text;
DROP TABLE ws;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (14, CAST(strftime('%s','now') AS INTEGER));

COMMIT;
