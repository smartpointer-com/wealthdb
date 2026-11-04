-- ============================================================
-- gold schema, migration 0176 — benchmarks (docs/RETURNS-NOTES.md,
-- "Benchmarks").
--
-- A benchmark is a named mix of funds with target weights, rebalanced
-- monthly. Its funds stand in for indexes.
--
--   * fund_returns: a filing's monthly total returns, per fund, filing
--     day and month, in percent, as filed. Reference data: a load writes
--     it, windowed by as_of as fund_risk is.
--   * benchmark_funds: the built-in set and the config's `benchmarks`
--     block, replaced whole at every load (gold.SyncBenchmarks).
--   * benchmark_returns, benchmark_mwr, benchmark_exposure and
--     benchmark_yield: derived, like report_returns. `web refresh`
--     rewrites them, and the dashboards read them as they stand.
--   * web_benchmarks: the Benchmark picker's values.
--
-- IF NOT EXISTS and OR REPLACE keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

CREATE TABLE IF NOT EXISTS fund_returns (
    silver_source_id  TEXT           NOT NULL,
    fund_ref          TEXT           NOT NULL,
    as_of             BIGINT         NOT NULL,
    month             DATE           NOT NULL,
    class_id          TEXT,
    total_return      DECIMAL(18, 8) NOT NULL,
    payload           JSON,
    PRIMARY KEY (silver_source_id, fund_ref, as_of, month)
);

-- benchmark_funds: one row per benchmark and fund. weight_pct is the
-- fund's target weight, the weights of a benchmark summing to 100.
CREATE TABLE IF NOT EXISTS benchmark_funds (
    benchmark   TEXT    NOT NULL,
    fund_ref    TEXT    NOT NULL,
    weight_pct  DOUBLE  NOT NULL,
    builtin     BOOLEAN NOT NULL,
    PRIMARY KEY (benchmark, fund_ref)
);

-- benchmark_returns: a benchmark's time-weighted return beside the
-- household's over the same days. granularity monthly, quarterly and
-- annual hold the household's buckets, at window_from_year 0. total
-- holds one row per start-year window, 0 being since inception: its
-- start_day and end_day are the days both sides have. Returns are
-- ratios, as in report_returns. excess is portfolio less benchmark. Days
-- are epoch seconds. A bucket or window with no return has NULLs and
-- says why in quality.
CREATE TABLE IF NOT EXISTS benchmark_returns (
    computed_at               BIGINT  NOT NULL,
    benchmark                 TEXT    NOT NULL,
    currency                  TEXT    NOT NULL,
    granularity               TEXT    NOT NULL,
    window_from_year          INTEGER NOT NULL,
    period                    TEXT    NOT NULL,
    start_day                 BIGINT  NOT NULL,
    end_day                   BIGINT  NOT NULL,
    twr                       DOUBLE,
    twr_annualized            DOUBLE,
    portfolio_twr             DOUBLE,
    portfolio_twr_annualized  DOUBLE,
    excess                    DOUBLE,
    excess_annualized         DOUBLE,
    portfolio_mwr             DOUBLE,
    mwr                       DOUBLE,
    mwr_annualized            DOUBLE,
    quality                   TEXT    NOT NULL,
    PRIMARY KEY (benchmark, currency, granularity, window_from_year, end_day)
);

-- benchmark_mwr: per benchmark, currency and start-year window, one row
-- per side and month end. series 'portfolio' is the household, 'benchmark'
-- the same start value and flows invested in the benchmark. value is what
-- that side holds on the day. mwr is the money-weighted return from the
-- window's start to the day, as report_returns' mwr: for the period, not
-- per year. A month end XIRR finds no answer for has mwr NULL and the
-- engine's mwr_* flag.
CREATE TABLE IF NOT EXISTS benchmark_mwr (
    computed_at       BIGINT  NOT NULL,
    benchmark         TEXT    NOT NULL,
    currency          TEXT    NOT NULL,
    window_from_year  INTEGER NOT NULL,
    series            TEXT    NOT NULL,
    end_day           BIGINT  NOT NULL,
    value             DOUBLE,
    mwr               DOUBLE,
    quality           TEXT    NOT NULL,
    PRIMARY KEY (benchmark, currency, window_from_year, series, end_day)
);

-- benchmark_exposure: a benchmark's look-through today. Each dimension's
-- buckets are shares in percent of the benchmark, on the dashboards'
-- definitions. A security or an issuer is a bucket too, label naming it.
CREATE TABLE IF NOT EXISTS benchmark_exposure (
    computed_at  BIGINT NOT NULL,
    benchmark    TEXT   NOT NULL,
    dimension    TEXT   NOT NULL,
    bucket       TEXT   NOT NULL,
    label        TEXT,
    lei          TEXT,
    share_pct    DOUBLE NOT NULL,
    PRIMARY KEY (benchmark, dimension, bucket)
);

-- benchmark_yield: a benchmark's yield and duration on a day, its funds'
-- stated figures, weighted. A day that lacks a figure for a fund of the
-- mix has none for the benchmark.
CREATE TABLE IF NOT EXISTS benchmark_yield (
    computed_at  BIGINT NOT NULL,
    benchmark    TEXT   NOT NULL,
    as_of        BIGINT NOT NULL,
    yield_pct    DOUBLE,
    duration     DOUBLE,
    PRIMARY KEY (benchmark, as_of)
);

-- web_benchmarks: the picker's values, None first.
CREATE OR REPLACE VIEW web_benchmarks AS
    SELECT benchmark
      FROM (SELECT 'None' AS benchmark, 0 AS rank
            UNION ALL
            SELECT DISTINCT benchmark, 1 FROM benchmark_funds)
     ORDER BY rank, benchmark;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (176, CAST(epoch(now()) AS BIGINT));
