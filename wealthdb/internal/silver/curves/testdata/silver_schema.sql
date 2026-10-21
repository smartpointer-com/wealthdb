-- Minimal curves silver schema for adapter tests (mirrors
-- collectors/curves/migrations/0001_initial.sql without the migration
-- wrapper). Published interest rates; `value` is a decimal STRING in
-- percent, dates are Unix seconds @ 00:00 UTC.

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE rates (
    as_of      INTEGER NOT NULL,
    series_id  TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    payload    TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (as_of, series_id)
);
