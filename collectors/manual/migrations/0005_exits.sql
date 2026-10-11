-- ============================================================
-- EXITS — what a position was sold for
-- ============================================================
-- A position that leaves the book at closed_at says here what it brought:
-- the proceeds, gross of any part an agent holds back (that part is an
-- escrow position of its own). The gold adapter projects each row as a
-- `sell` transaction on the exit date, which the lot engine realizes
-- against the position's book value. A write-off is proceeds 0. A note
-- that converts to equity has no row: nothing was sold.
BEGIN;

CREATE TABLE exits (
    position_id  TEXT NOT NULL PRIMARY KEY,
    exit_date    TEXT NOT NULL,                -- ISO 'YYYY-MM-DD'
    proceeds     TEXT NOT NULL,                -- decimal string in `currency`
    currency     TEXT NOT NULL,
    notes        TEXT
);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (5, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
