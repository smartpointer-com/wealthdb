-- ============================================================
-- ubs-web silver schema, migration 0017 — the quarterly reports of
-- private-markets funds.
--
-- A "Private Market Letter" whose cover page is titled "Quarterly
-- Report" is the fund administrator's report on a feeder fund
-- (DESIGN.md §3.12). Two parts of it are read:
--
--   private_market_reports       one row per report: the feeder fund
--                                and the investor's capital account.
--   private_market_report_lines  one row per line of the report's
--                                per-investment table.
--
-- Every figure is as printed. A figure the report does not print is
-- NULL. The tables hold nothing yet: they fill when the document pass
-- re-derives the archive. That happens on the next load, since the
-- parser generation has moved (migration 0009).
-- ============================================================

-- The capital account's figures are the investor's column, in
-- currency_iso. as_of is the capital account's date, Unix seconds UTC
-- midnight. lines_as_of is the date the per-investment table states
-- for itself, which may lag as_of; NULL where the table prints none.
--
-- line_count is how many investment lines the table gave. lines_total
-- is the residual value of the table's closing total. lines_reconciled
-- is 1 when the lines' residual values sum to that total within the
-- table's rounding, so the lines are the whole table; 0 otherwise.
CREATE TABLE private_market_reports (
    source_doc_token     TEXT     NOT NULL PRIMARY KEY, -- FK to documents.doc_token
    instrument_isin      TEXT     NOT NULL,             -- the feeder fund
    fund_name            TEXT,                          -- as the capital account names it
    as_of                INTEGER  NOT NULL,
    lines_as_of          INTEGER,
    currency_iso         TEXT,
    commitment           REAL,                          -- the investor's net commitment
    capital_called       REAL,
    cumulative_expenses  REAL,                          -- negative, as printed in parentheses
    valuation            REAL,                          -- the investor's NAV
    shares_in_issue      REAL,
    nav_per_share        REAL,
    line_count           INTEGER  NOT NULL,
    lines_total          REAL,
    lines_reconciled     INTEGER  NOT NULL,
    payload              TEXT     NOT NULL              -- the printed lines each figure was read from
);

CREATE INDEX ix_private_market_reports_isin
    ON private_market_reports(instrument_isin, as_of);

-- One investment of the fund behind the feeder. region, sector and
-- strategy are the labels the table prints. A table states a sector or
-- a strategy, never both. cost is the capital invested and
-- residual_value what remains of it, in the table's own unit (often
-- millions of the fund's currency). residual_value is the table's own
-- column, its current NAV, or its total value less its realised value.
CREATE TABLE private_market_report_lines (
    source_doc_token  TEXT     NOT NULL,                -- FK to private_market_reports
    line_no           INTEGER  NOT NULL,                -- order in the table, from 1
    name              TEXT     NOT NULL,
    region            TEXT     NOT NULL,
    sector            TEXT,
    strategy          TEXT,
    cost              REAL,
    residual_value    REAL,
    payload           TEXT     NOT NULL,                -- the printed line
    PRIMARY KEY (source_doc_token, line_no)
);


INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (17, CAST(strftime('%s','now') AS INTEGER));
