-- The cash rows in historical_position_snapshots had accumulated one copy
-- per ingested dump.
--
-- The table's primary key ends in `instrument_isin`. A cash line has none,
-- and SQLite treats NULLs in a primary key as DISTINCT, so the loader's
-- `INSERT OR REPLACE` never collapsed them. `_load_historical_from_pdfs`
-- re-lists the WHOLE cumulative documents table on every dump — by design,
-- so a statement first seen in an old dump is still re-derived — which made
-- that one extra copy of every cash row per dump, growing without limit.
-- Securities rows were never affected: their ISIN makes the key compare.
--
-- Every copy was byte-identical to its twin, down to source_doc_token and
-- payload — the duplication factor was simply the number of dumps ingested
-- since the last rebuild. The collapse is therefore lossless: it removes
-- copies, never an observation.
--
-- The rows are KEPT rather than dropped, though the gold adapter skips them
-- (internal/silver/ubs/historical.go takes cash from historical_cash_
-- balances instead). They are not as redundant with that table as its
-- comment assumes: most carry a quarter-end date the monthly series has no
-- row for, on accounts it does otherwise cover, so those observations exist
-- nowhere else in silver. Whether gold should read them is an open
-- question; deleting them would answer it by destroying the evidence.
--
-- `_replace_hist_cash_row` in the loader keeps them collapsed from here on
-- by deleting the row a cash line is about to replace — what the primary
-- key would do itself if NULLs compared equal.
DELETE FROM historical_position_snapshots
 WHERE instrument_isin IS NULL
   AND rowid NOT IN (
       SELECT MIN(rowid)
         FROM historical_position_snapshots
        WHERE instrument_isin IS NULL
        GROUP BY as_of_date, portfolio_external_id,
                 account_external_id, currency_iso
   );

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (11, CAST(strftime('%s','now') AS INTEGER));
