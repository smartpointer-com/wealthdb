-- fred silver schema v1.
--
-- Daily USD-centric FX reference rates from the US Federal Reserve H.10
-- release, pulled per-currency from FRED series (see download.py's
-- FX_SERIES map). The schema runner (collectorkit.silver) records the
-- applied silver_schema_version in schema_meta; current version =
-- MAX(silver_schema_version). Each NNNN_*.sql migration ends by
-- inserting its own version.

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                -- Unix seconds UTC
);

-- One row per loaded download (fetch) run, keyed by the bronze run-dir
-- timestamp, so an already-loaded bronze dir is skipped. (Row-level
-- fx_rates are upserted by date, so a fresh fetch overwrites any
-- FRED-revised dates and appends new ones — see load.py.)
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,   -- bronze run ts, Unix s UTC
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL                -- absolute dump-dir path
);

-- One row per (observation date, base, quote).
--
-- snapshot_at is the observation DATE at 00:00 UTC (FRED H.10 are daily
-- rates with no intraday time). base/quote follow the canonical FX
-- convention: `(base, quote, mid)` means "1 unit of quote = mid units of
-- base" (mid = base per quote) — the same direction as the canonical
-- FxRateChange and the ubs-psn fx_rates table, so a gold adapter maps
-- straight across. FRED H.10 are mid reference rates (no bid/ask).
CREATE TABLE fx_rates (
    snapshot_at          INTEGER NOT NULL,   -- observation date @ 00:00 UTC, Unix s
    base_currency_iso    TEXT    NOT NULL,
    quote_currency_iso   TEXT    NOT NULL,
    mid                  TEXT    NOT NULL,    -- decimal string; 1 quote = mid base
    payload              TEXT    NOT NULL,    -- {"series_id":..,"value":..}
    PRIMARY KEY (snapshot_at, base_currency_iso, quote_currency_iso)
);

CREATE INDEX idx_fx_rates_date ON fx_rates (snapshot_at);

INSERT INTO schema_meta (silver_schema_version, applied_at)
VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));
