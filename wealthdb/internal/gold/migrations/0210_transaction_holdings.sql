-- ============================================================
-- gold schema, migration 0210 — the holding each transaction
-- belongs to.
--
-- A bank books a dividend or a trade on a cash account, beside the
-- custody account that holds the security, or in another portfolio.
-- The yield pass decides once, per transaction that names an
-- instrument, which account held that instrument as the line
-- happened (yield.Holders.Place). The yield readers read the holder
-- from here.
--
--   * holder_account: the account that held the instrument, else the
--     account the line is booked on.
--   * instrument: the id the holder held it under, as the matching
--     folds it (yield.Inputs.Fold).
--   * basis: which step placed it: own (the booking account), portfolio
--     (the one other account of its portfolio that held it), source
--     (the one account of its source), or none (no account, or more
--     than one).
--
-- transaction_holdings_inputs holds one row: the stamp of the inputs
-- the rows were placed from. A reader whose inputs stamp otherwise,
-- such as one before the first pass, places the lines itself.
-- ============================================================

CREATE TABLE IF NOT EXISTS transaction_holdings (
    silver_source_id        VARCHAR NOT NULL,
    transaction_external_id VARCHAR NOT NULL,
    holder_account          VARCHAR NOT NULL,
    instrument              VARCHAR NOT NULL,
    basis                   VARCHAR NOT NULL,
    PRIMARY KEY (silver_source_id, transaction_external_id)
);

CREATE TABLE IF NOT EXISTS transaction_holdings_inputs (
    stamp VARCHAR NOT NULL
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (210, CAST(epoch(now()) AS BIGINT));
