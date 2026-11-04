-- refdata silver schema v4: a fund's monthly total returns.
--
-- fund_returns holds what an N-PORT filing states for the three months
-- it covers, for the share class the fund's ticker maps to. Gold reads
-- them for the benchmarks (wealthdb/docs/RETURNS-NOTES.md).
--
-- Conventions as in 0001. as_of is the filing's report day, as for
-- fund_risk. month is the last day of the month the return covers,
-- YYYY-MM-DD text. total_return is in percent, a decimal string, as
-- filed. Two filings can state one month, and a filing's rows never
-- touch another's: gold reads the later one. A request that states a
-- fund's returns for a day replaces the rows of that fund and day.
CREATE TABLE fund_returns (
    fund_ref      TEXT    NOT NULL,
    as_of         INTEGER NOT NULL,
    month         TEXT    NOT NULL,
    class_id      TEXT,                          -- the filing's share class, NULL where it names none
    total_return  TEXT    NOT NULL,
    run_at        INTEGER NOT NULL,
    payload       TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of, month)
);

CREATE INDEX idx_fund_returns_run ON fund_returns (run_at);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s', 'now') AS INTEGER));
