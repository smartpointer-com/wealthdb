-- ============================================================
-- schwab-api silver schema, migration 0002 — add instruments table.
--
-- Background: Schwab's positions and transactions endpoints omit the
-- `description` field for assetType=EQUITY but populate it for every
-- other asset class. To make the silver view consistent rather than
-- inheriting that inconsistency, the `instruments` table caches the
-- response of Schwab's /marketdata/v1/instruments endpoint (projection
-- = symbol-search), which returns descriptions for every asset class.
--
-- Populated by download.py's default instrument lookup; --no-instruments
-- suppresses it, leaving this table empty for that run.
-- ============================================================

-- One row per (snapshot, symbol). Loader dedup: insert only when the
-- canonical-JSON payload differs from the most recent row for the same
-- symbol (matches the accounts/user_preference pattern). Instrument
-- metadata changes rarely; dedup keeps row count near "one per symbol".
CREATE TABLE instruments (
    snapshot_at INTEGER NOT NULL,
    symbol      TEXT    NOT NULL,
    payload     TEXT    NOT NULL,                 -- JSON: full /instruments record
    PRIMARY KEY (snapshot_at, symbol)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s','now') AS INTEGER));
