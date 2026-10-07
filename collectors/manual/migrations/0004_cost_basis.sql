-- ============================================================
-- COST BASIS — an optional paid-in series per position
-- ============================================================
-- A position's book value is the valuation dated at its acquired_at. That
-- is the cost of a bought asset, but not of a commitment paid in over time:
-- a fund entered at its commitment on its first day books the commitment,
-- while the capital is called in later. A row here states the capital paid in as of a date,
-- gross of any capital paid back. A position with rows here takes its book
-- value from them: the latest row on or before the date, none before the
-- first. A position without any keeps the valuation at acquired_at.
BEGIN;

CREATE TABLE cost_basis (
    position_id  TEXT NOT NULL,
    as_of_date   TEXT NOT NULL,                -- ISO 'YYYY-MM-DD'
    amount       TEXT NOT NULL,                -- decimal string in `currency`
    currency     TEXT NOT NULL,
    notes        TEXT,
    PRIMARY KEY (position_id, as_of_date)
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
