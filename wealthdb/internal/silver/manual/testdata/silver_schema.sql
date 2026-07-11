-- Minimal manual silver schema for adapter tests (mirrors
-- collectors/manual/migrations/0001_initial.sql, without the migration
-- wrapper). Money is decimal STRINGS (TEXT); dates are ISO 'YYYY-MM-DD' TEXT.
-- The collector records positions + valuations only (no transactions).

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL
);

CREATE TABLE load_runs (
    load_at               INTEGER NOT NULL,
    silver_schema_version INTEGER NOT NULL,
    bronze_dir            TEXT    NOT NULL,
    positions_total       INTEGER NOT NULL DEFAULT 0,
    valuations_total      INTEGER NOT NULL DEFAULT 0,
    payload               TEXT    NOT NULL
);

CREATE TABLE positions (
    id            TEXT NOT NULL PRIMARY KEY,
    kind          TEXT NOT NULL,
    vehicle       TEXT,
    display_name  TEXT NOT NULL,
    currency      TEXT NOT NULL,
    acquired_at   TEXT NOT NULL,
    closed_at     TEXT,
    notes         TEXT,
    payload       TEXT NOT NULL
);

CREATE TABLE valuations (
    position_id  TEXT NOT NULL,
    as_of_date   TEXT NOT NULL,
    value        TEXT NOT NULL,
    currency     TEXT NOT NULL,
    notes        TEXT,
    payload      TEXT NOT NULL,
    PRIMARY KEY (position_id, as_of_date)
);
