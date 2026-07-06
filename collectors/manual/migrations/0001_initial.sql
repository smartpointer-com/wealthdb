-- ============================================================
-- manual silver schema, migration 0001 — initial schema (SQLite + JSON1).
--
-- The "manual" collector has NO source to fetch from — the input is
-- hand-maintained. Bronze is two hand-maintained CSVs in
-- $XDG_DATA_HOME/wealthdb/manual/ (positions.csv, valuations.csv); there is no login /
-- download step. load.py validates those CSVs and rebuilds this silver from
-- them, which the gold adapter reads.
--
-- Scope: the collector tracks only the illiquid POSITIONS and their
-- VALUATIONS — the data nothing else holds. It deliberately records NO
-- cash-flow transactions: the wires that fund a purchase, pay a fee, or
-- return a distribution are real movements in the bank accounts,
-- already captured by the bank collectors; the acquisition date lives on the
-- position. A transactions ledger would only duplicate the banks (DESIGN §6).
--
-- Migration discipline (shared across wealthdb collectors): every change
-- lands as a new numbered file here; collectorkit's loader applies any file
-- whose number exceeds the max applied silver_schema_version, and each
-- migration ends by inserting its own version into schema_meta. (This silver
-- is rebuilt from the CSVs on every load — there is no persistent state to
-- migrate — so schema changes can also land by deleting + reloading.)
--
-- Storage conventions (shared across wealthdb collectors):
--   * SQLite + JSON1 — the repo default (not DuckDB: this is a tiny shape
--     transformation, a few rows a year, not a high-volume / complex-query
--     store). Stable filter/join fields are promoted to columns; everything
--     kind-specific rides in a `payload` TEXT (JSON) column, so a new asset
--     kind or per-kind field never needs a schema migration.
--   * Money (value) is stored as a decimal STRING (TEXT), verbatim from the
--     CSV, to avoid float rounding — the gold adapter parses it to a canonical
--     Decimal (same approach as carta's fund-LP money). Rates / ownership_pct
--     / other ratios live inside `payload`, not money columns.
--   * Dates (acquired_at, closed_at, as_of_date) are ISO-8601 'YYYY-MM-DD'
--     TEXT; the gold adapter parses them. Ingest timestamps (load_at) are
--     Unix-seconds-UTC INTEGER.
--   * Referential integrity (a valuation's position_id; a conversion's
--     converted_from_position_id back-reference) is enforced in load.py with
--     row+column context, NOT by SQLite FK constraints — the loader truncates
--     and fully rebuilds these tables from the current CSVs on every run.
--   * Allowed `kind` vocabulary lives in load.py (single source of truth,
--     easy to extend), documented inline below.
-- ============================================================

PRAGMA foreign_keys = ON;

-- All-or-nothing: executescript() implicitly COMMITs before running, so the
-- transaction is controlled inside the file.
BEGIN;

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                  -- Unix seconds UTC
);

-- One row per load invocation. There are no timestamped bronze run-dirs
-- here (no download step): each load is a full rebuild from the current
-- CSVs, so this is an append-only audit log (when did we last load, how
-- many rows), not an idempotency gate.
CREATE TABLE load_runs (
    load_at               INTEGER NOT NULL,                 -- Unix seconds UTC
    silver_schema_version INTEGER NOT NULL,
    bronze_dir            TEXT    NOT NULL,                 -- where the CSVs were read from
    positions_total       INTEGER NOT NULL DEFAULT 0,
    valuations_total      INTEGER NOT NULL DEFAULT 0,
    payload               TEXT    NOT NULL
);

-- ============================================================
-- POSITIONS — one row per held asset (mirrors positions.csv)
-- ============================================================
-- `kind` discriminates the asset family. It is deliberately identical to the
-- canonical gold `asset_class` (the gold classmap is an identity). The set is
-- open-ended — `load.py`'s POSITION_KINDS is the source of truth. Current
-- values: 'real_estate', 'private_equity', 'convertible_note', 'private_fund'
-- (a venture/PE fund LP interest), 'spv' (a single-deal vehicle), and 'other'
-- (catch-all, e.g. a receivable). 'convertible_note' covers a 0%
-- early-stage note expected to convert to equity at the next round (or go to
-- zero). `payload` carries the kind-specific fields, e.g.
--   real_estate:      {"property_type":"residential","ownership_pct":100, ...}
--   convertible_note: {"principal":..., "interest_rate":0, "cap":...,
--                      "maturity_date":"YYYY-MM-DD", "conversion_terms":"...",
--                      "counterparty":"..."}
--   private_equity:   {"ownership_pct":10, "share_cnt":..., "fiduciary":"...",
--                      "converted_from_position_id":"..."}  (if from a note)
--   private_fund:     {"role":"limited_partner", "commitment":...}
--   spv:              {"spv_name":"...", "company":"...", "post_money_valuation":...}
-- `closed_at` is set when the position stops existing — a full disposal, or a
-- convertible note that converted to equity (the new equity position
-- back-references the note via payload.converted_from_position_id) — so gold
-- drops it from as-of queries after that date.
CREATE TABLE positions (
    id            TEXT NOT NULL PRIMARY KEY,   -- user-assigned stable id (synthetic)
    kind          TEXT NOT NULL,               -- canonical asset_class; see POSITION_KINDS in load.py
    display_name  TEXT NOT NULL,               -- PII: synthetic placeholder in tracked files
    currency      TEXT NOT NULL,               -- ISO 4217
    acquired_at   TEXT NOT NULL,               -- ISO 'YYYY-MM-DD'
    closed_at     TEXT,                         -- ISO date; NULL while still held
    notes         TEXT,
    payload       TEXT NOT NULL                -- JSON object
);
CREATE INDEX ix_positions_kind ON positions(kind);

-- ============================================================
-- VALUATIONS — the periodic mark-to-market time series
-- ============================================================
-- One row per (position, as-of date). The position's market value as of a
-- date D is the latest row with as_of_date <= D (gold forward-fills) — so an
-- illiquid asset's value moves correctly over time (a property re-appraised
-- every few years, a convertible note re-marked at a funding round). The
-- valuation dated at the position's acquired_at is the cost basis (gold's
-- book_value). `currency` must match the position's. `payload` carries
-- kind-specific provenance, e.g. {"appraisal_source":"..."}.
CREATE TABLE valuations (
    position_id  TEXT NOT NULL,
    as_of_date   TEXT NOT NULL,                -- ISO 'YYYY-MM-DD'
    value        TEXT NOT NULL,                -- decimal string in `currency`
    currency     TEXT NOT NULL,
    notes        TEXT,
    payload      TEXT NOT NULL,                -- JSON object
    PRIMARY KEY (position_id, as_of_date)
);
CREATE INDEX ix_valuations_position ON valuations(position_id);

-- Migration-complete marker (COMMIT closes the txn).
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
