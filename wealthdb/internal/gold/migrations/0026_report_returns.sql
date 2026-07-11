-- report_returns: materialized TWR/MWR returns for the Metabase Returns
-- dashboards. The first DERIVED table in gold — everything else is either
-- source-of-truth (loader-written) or a view/macro computed on read. Returns
-- can't be a macro: the engine's policy hooks, config-driven inception
-- overrides/exclusions, transfer netting, and XIRR root-finding are not
-- expressible in SQL, so `wealthdb web-materialize` precomputes them here
-- (MaterializeReturns, returns_materialize.go) and the web snapshot carries
-- the result like any other table.
--
-- Contract: each (grain, granularity, currency) partition is the VERBATIM
-- output of one RunReturns call with the CLI's default knobs (`wealthdb
-- returns <grain> --period <granularity> --method both -x <CCY>`), rows
-- unmodified. Bucket rows carry TWR only (per-bucket Modified Dietz); MWR
-- lives on the summary rows — engine behavior, mirrored here. The 'total'
-- granularity partitions therefore contain only summary rows.
--
-- Every run rewrites the whole table (DELETE + INSERT in one transaction),
-- so no PK or indexes: the table is small and never updated in place.

CREATE TABLE IF NOT EXISTS report_returns (
    computed_at      BIGINT  NOT NULL,  -- epoch seconds, same for every row of a run
    currency         TEXT    NOT NULL,  -- 'USD' | 'CHF' | 'EUR'
    grain            TEXT    NOT NULL,  -- accounts | portfolios | sources | global
    granularity      TEXT    NOT NULL,  -- monthly | quarterly | annual | total
    silver_source_id TEXT    NOT NULL,  -- '' for the global grain
    entity_id        TEXT    NOT NULL,  -- '' for global / per-source orphan bucket
    entity_label     TEXT    NOT NULL,
    period           TEXT    NOT NULL,  -- '2025-Q1' / '2025-03' / '2025' / summary label
    is_summary       BOOLEAN NOT NULL,  -- the since-inception cumulative row
    start_day        BIGINT  NOT NULL,  -- epoch seconds (ReturnRow.StartDay * 86400)
    end_day          BIGINT  NOT NULL,
    start_value      DECIMAL(28,4),     -- output-currency; NULL = unresolved (engine semantics)
    end_value        DECIMAL(28,4),
    net_flow         DECIMAL(28,4),
    twr              DOUBLE,            -- ratio (0.07 = 7%); NULL = n/a, reason in quality
    twr_annualized   DOUBLE,
    mwr              DOUBLE,
    mwr_annualized   DOUBLE,
    quality          TEXT    NOT NULL   -- ';'-joined flags, matches the CLI csv column
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (26, CAST(epoch(now()) AS BIGINT));
