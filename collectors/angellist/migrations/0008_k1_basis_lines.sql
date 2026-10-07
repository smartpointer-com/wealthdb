-- angellist silver v8 — K-1 property distributions and gain lines as columns.
--
-- Three Schedule K-1 lines bear on a vehicle's cost basis and realized
-- gains. Each K-1 CSV row carries them; they become minor-unit columns
-- beside the capital account, in the table's existing convention:
--
--   property_distributions_minor  Line 19(c) Property Distributions — what
--                                 the vehicle distributed in kind, such as
--                                 shares; it reduces the partner's basis in
--                                 the vehicle, as Line 19(a) does for cash.
--   short_term_gain_minor         Line 8 Net Short-Term Capital Gain (Loss)
--   long_term_gain_minor          Line 9(a) Net Long-Term Capital Gain (Loss)
--
-- Each is the K-1 figure as printed. NULL when the cell is blank or the row
-- has no such cell (a "Not Issuing" row can end after its notes).
--
-- Backfill. `payload` already holds every CSV cell, keyed by its header, so
-- the rows on disk fill from it here and need no re-parse. A header matches
-- as load.py matches it: case-insensitively, by a phrase it contains, first
-- matching header wins (the CSV spells a line letter either way, 9(a) or
-- 9(A)). A cell parses by the money rules load.py uses for the K-1 CSV:
-- `$` and `,` dropped, accounting parentheses negative, a leading sign kept,
-- anything else that is not a plain decimal number NULL.

ALTER TABLE k1_capital_accounts ADD COLUMN property_distributions_minor INTEGER;
ALTER TABLE k1_capital_accounts ADD COLUMN short_term_gain_minor INTEGER;
ALTER TABLE k1_capital_accounts ADD COLUMN long_term_gain_minor INTEGER;

WITH cells AS (
    SELECT k.rowid AS rid,
           je.id AS pos,
           CASE WHEN je.key LIKE '%property distributions%' THEN 'prop'
                WHEN je.key LIKE '%net short-term capital gain%' THEN 'st'
                WHEN je.key LIKE '%net long-term capital gain%' THEN 'lt'
           END AS line,
           trim(je.value) AS raw
      FROM k1_capital_accounts AS k, json_each(k.payload) AS je
     WHERE json_valid(k.payload) AND je.type = 'text'
),
nums AS (
    SELECT rid, line, pos,
           raw LIKE '(%)' AS neg,
           trim(replace(replace(trim(raw, '()'), ',', ''), '$', '')) AS num
      FROM cells
     WHERE line IS NOT NULL
),
firsts AS (
    SELECT rid, line,
           CASE WHEN num GLOB '[-+.0-9]*'
                 AND substr(num, 2) NOT GLOB '*[^.0-9]*'
                 AND num GLOB '*[0-9]*'
                 AND num NOT GLOB '*.*.*'
                THEN CAST(round(CAST(num AS REAL) * 100) AS INTEGER)
                     * (CASE WHEN neg THEN -1 ELSE 1 END)
           END AS minor,
           row_number() OVER (PARTITION BY rid, line ORDER BY pos) AS nth
      FROM nums
),
lines AS (
    SELECT rid,
           max(CASE WHEN line = 'prop' THEN minor END) AS prop,
           max(CASE WHEN line = 'st' THEN minor END) AS st,
           max(CASE WHEN line = 'lt' THEN minor END) AS lt
      FROM firsts
     WHERE nth = 1
     GROUP BY rid
)
UPDATE k1_capital_accounts
   SET property_distributions_minor = lines.prop,
       short_term_gain_minor = lines.st,
       long_term_gain_minor = lines.lt
  FROM lines
 WHERE lines.rid = k1_capital_accounts.rowid;

INSERT INTO schema_meta (silver_schema_version) VALUES (8);
