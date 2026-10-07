-- ============================================================
-- schwab-api silver schema, migration 0005 — promote the position's
-- average cost and open P/L.
--
-- Both figures sit on every position object the Trader API returns.
-- They are Schwab's adjusted tax-lot basis, so an adapter can read a
-- holding's book value without a json_extract per row.
--
--   average_cost         — `averagePrice`, as sent, in the API's price
--                          convention: per share for equities, funds
--                          and options (not per contract), per 100 of
--                          par for bonds. A zero is stored as sent.
--                          The holding's total cost is market value
--                          minus open P/L, which sidesteps that
--                          per-asset-class scale.
--   unrealized_gain_loss — `longOpenProfitLoss`, or
--                          `shortOpenProfitLoss` when the position is
--                          short (`shortQuantity` > 0). USD, as sent.
--
-- NULL means the API did not send the field. `averageLongPrice` stays
-- in payload: it is a different average and does not tie to the
-- statements' printed cost basis.
--
-- Rows loaded before this migration are backfilled from payload; the
-- loader writes both columns on every new row with the same rule.
-- ============================================================

ALTER TABLE positions ADD COLUMN average_cost         REAL;
ALTER TABLE positions ADD COLUMN unrealized_gain_loss REAL;

UPDATE positions SET
    average_cost = json_extract(payload, '$.averagePrice'),
    unrealized_gain_loss = CASE
        WHEN json_extract(payload, '$.shortQuantity') > 0
            THEN json_extract(payload, '$.shortOpenProfitLoss')
        ELSE json_extract(payload, '$.longOpenProfitLoss')
    END;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (5, CAST(strftime('%s','now') AS INTEGER));
