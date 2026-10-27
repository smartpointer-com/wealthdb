-- ============================================================
-- fidelity-web silver schema, migration 0015 — stated periods.
--
-- One row per account and period, as a document states that
-- period's summary: the beginning and ending value and the
-- monthly flows between them. The rows are a record of what the
-- document says. They create no transaction and no position.
--
-- fidelity-web itself writes no row. A statement family that
-- states a period's summary fills the table, such as the svb
-- month-end workbook (collectors/svb/DESIGN.md).
--
-- Columns:
--
--   * `period_start` and `period_end` are the period's first and
--     last day.
--   * `beginning_value`, `ending_value`, `accrual`,
--     `net_additions`, `net_gain`, `income` and `fees` are in
--     the row's currency, as stated. `net_additions` is signed:
--     money into the account is positive, money out negative.
--     `fees` is a positive amount.
--
-- NULL means the document states nothing for that cell.
-- `payload` keeps every cell as read.
-- ============================================================

CREATE TABLE stated_periods (
    period_start         INTEGER NOT NULL,  -- Unix seconds at midnight UTC, the period's first day
    period_end           INTEGER NOT NULL,  -- Unix seconds at midnight UTC, the period's last day
    account_external_id  TEXT    NOT NULL,
    beginning_value      REAL,
    ending_value         REAL,
    accrual              REAL,
    net_additions        REAL,              -- signed: in positive, out negative
    net_gain             REAL,
    income               REAL,
    fees                 REAL,              -- a positive amount, as stated
    currency             TEXT    NOT NULL DEFAULT 'USD',
    source_sha256        TEXT    NOT NULL,  -- sha256 of the document that states the period
    payload              TEXT    NOT NULL,  -- the row's cells as read, JSON
    PRIMARY KEY (period_end, account_external_id, source_sha256)
);

CREATE INDEX ix_stated_periods_account
    ON stated_periods(account_external_id, period_end);


-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (15, CAST(strftime('%s', 'now') AS INTEGER));
