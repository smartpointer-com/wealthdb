-- Minimal fred silver schema for adapter tests (mirrors
-- collectors/fred/migrations/0001_initial.sql without the migration
-- wrapper). FX reference rates only; `mid` is a decimal STRING, dates are
-- Unix seconds @ 00:00 UTC.

CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL
);

CREATE TABLE fx_rates (
    snapshot_at          INTEGER NOT NULL,
    base_currency_iso    TEXT    NOT NULL,
    quote_currency_iso   TEXT    NOT NULL,
    mid                  TEXT    NOT NULL,
    payload              TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, base_currency_iso, quote_currency_iso)
);
