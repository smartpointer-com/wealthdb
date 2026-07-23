-- ============================================================
-- fidelity-web silver schema, migration 0004 — historical
-- position snapshots reconstructed from statement PDFs.
--
-- The live `positions` table is point-in-time (one row per dump
-- of Fidelity's positions page). For dates before the toolkit
-- started running, the only available historical-positions
-- source is the quarterly / annual statement PDF archive.
--
-- Coverage:
--   * Accounts whose group the web document center serves
--     statements for, such as 529 College Investing Plan
--     accounts (fidelity-web/DESIGN.md §4.5). Later migrations
--     and loader passes add other statement families.
--   * One row per (as_of_date, account, fund) inside the
--     statement's "Holdings" block.
--
-- Identity model:
--   * `account_external_id` is the 9-digit canonical form,
--     dashes stripped from the statement's `Account # NNN-NNNNNN`
--     header.
--   * `instrument_key` is the cross-walked ticker (looked up
--     against `positions.description` on any later snapshot)
--     when possible; NULL otherwise. The human-readable fund
--     description from the PDF is always preserved in
--     `description` so gold can resolve downstream if our
--     cross-walk missed.
--   * The market value column is denominated in USD (Fidelity
--     US is USD-only).
-- ============================================================

CREATE TABLE historical_position_snapshots (
    as_of_date           INTEGER NOT NULL,  -- Unix seconds at midnight UTC, period-end
    account_external_id  TEXT    NOT NULL,  -- 9-digit, no separator
    description          TEXT    NOT NULL,  -- fund description verbatim from the PDF
    instrument_key       TEXT,              -- ticker / plan code; NULL until cross-walked
    quantity             REAL,
    price                REAL,              -- per-unit, USD
    market_value         REAL,              -- USD
    percent_of_total     REAL,              -- fraction (0..1) of the per-account total
    currency             TEXT    NOT NULL DEFAULT 'USD',
    source_sha256        TEXT    NOT NULL,  -- sha256 of the source PDF
    payload              TEXT    NOT NULL,  -- raw parsed row JSON
    PRIMARY KEY (as_of_date, account_external_id, description)
);

CREATE INDEX ix_hist_pos_date
    ON historical_position_snapshots(as_of_date);
CREATE INDEX ix_hist_pos_account
    ON historical_position_snapshots(account_external_id);
CREATE INDEX ix_hist_pos_instrument
    ON historical_position_snapshots(instrument_key);
CREATE INDEX ix_hist_pos_source
    ON historical_position_snapshots(source_sha256);


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s', 'now') AS INTEGER));
