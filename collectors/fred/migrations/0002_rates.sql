-- fred silver schema v2: published interest-rate series.
--
-- One row per (observation date, series), from the rates family of
-- download.py (RATE_SERIES): the Treasury constant-maturity yields and
-- the three-month bill. value is the series' value as FRED serves it, in
-- percent. payload records what the collector's map says the series is:
-- {"currency":..,"tenor":..,"basis":..}, basis being constant_maturity
-- (a par yield, semiannual) or discount (a bill's discount rate). Rows
-- upsert by date, as fx_rates do, so a fresh fetch overwrites a revised
-- day.
CREATE TABLE rates (
    as_of      INTEGER NOT NULL,   -- observation date @ 00:00 UTC, Unix s
    series_id  TEXT    NOT NULL,
    value      TEXT    NOT NULL,   -- decimal string, percent
    payload    TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (as_of, series_id)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
VALUES (2, CAST(strftime('%s', 'now') AS INTEGER));
