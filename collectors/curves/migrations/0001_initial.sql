-- curves silver schema v1.
--
-- The public government yield curves of the euro (the ECB's) and the
-- franc (the SNB's), one series per tenor (see download.py's
-- RATE_SERIES map). The schema runner (collectorkit.silver) records the
-- applied silver_schema_version in schema_meta; current version =
-- MAX(silver_schema_version). Each NNNN_*.sql migration ends by
-- inserting its own version.

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                -- Unix seconds UTC
);

-- One row per loaded download (fetch) run, keyed by the bronze run-dir
-- timestamp, so an already-loaded bronze dir is skipped. (Rows of rates
-- are upserted by date, so a fresh fetch overwrites a revised day and
-- appends new ones — see load.py.)
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,   -- bronze run ts, Unix s UTC
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL                -- absolute dump-dir path
);

-- One row per (observation date, series): the fred collector's rates
-- table, column for column, so the engine reads both alike. value is the
-- series' value as the portal states it, in percent. payload records what
-- the collector's map says the series is:
-- {"currency":..,"tenor":..,"basis":..,"compounding":..}, basis spot (a
-- zero rate) for every series of the map, compounding continuous for
-- the ECB's and annual for the SNB's.
CREATE TABLE rates (
    as_of      INTEGER NOT NULL,   -- observation date @ 00:00 UTC, Unix s
    series_id  TEXT    NOT NULL,
    value      TEXT    NOT NULL,   -- decimal string, percent
    payload    TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (as_of, series_id)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));
