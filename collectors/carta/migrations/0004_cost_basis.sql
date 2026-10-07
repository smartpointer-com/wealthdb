-- ============================================================
-- carta silver schema, migration 0004 — cost-basis facts as columns.
--
-- Bronze states several cost-basis facts that silver kept only in payload
-- or did not parse. This migration gives each a column; the loader fills
-- them (DESIGN.md §5.3).
--
--   * securities.exercise_type — the certificate's own `exercise_type`
--     (ISO / NSO) for a share lot born from an option exercise. It decides
--     which basis applies: an NSO's shares take the fair-market-value at
--     exercise as their tax basis.
--   * securities.exercise_date / exercise_fmv — the date and the
--     fair-market-value per share an exercise-detail xlsx states, on the
--     share certificate that exercise produced.
--   * fund_metrics.accepted_date — partner-metrics' `partner.accepted_date`,
--     the day the fund accepted the partner, as printed (ISO timestamp).
--   * fund_metrics.management_fees / net_operating_income / realized_gain /
--     unrealized_gain / carried_interest — a capital-account statement's
--     inception-to-date lines, on that statement's row.
--   * k1_capital_accounts — the federal Schedule K-1 face page of each tax
--     document: item L (the partner's tax capital account), boxes 8 and 9a
--     (net short- and long-term capital gain) and box 19 codes A and C
--     (cash and property distributions).
--
-- Backfill: exercise_type and accepted_date are already in payload and are
-- filled here. The other columns come from bronze files silver never
-- parsed, so they fill on a reload (`load --force`).
-- ============================================================

BEGIN;

ALTER TABLE securities ADD COLUMN exercise_type TEXT;   -- row.exercise_type ('ISO' | 'NSO'), as printed
ALTER TABLE securities ADD COLUMN exercise_date TEXT;   -- exercise-detail date (MM/DD/YYYY), as printed
ALTER TABLE securities ADD COLUMN exercise_fmv  REAL;   -- fair-market-value per share at that exercise

-- Money on these rows follows the table's convention: decimal strings as
-- TEXT. The statement figures are signed as the statement prints them (a
-- parenthesised amount is negative; the nil dash is 0). NULL when the line
-- is absent, and on a partner-metrics row with no statement of its date.
ALTER TABLE fund_metrics ADD COLUMN accepted_date        TEXT;
ALTER TABLE fund_metrics ADD COLUMN management_fees      TEXT;
ALTER TABLE fund_metrics ADD COLUMN net_operating_income TEXT;
ALTER TABLE fund_metrics ADD COLUMN realized_gain        TEXT;
ALTER TABLE fund_metrics ADD COLUMN unrealized_gain      TEXT;
ALTER TABLE fund_metrics ADD COLUMN carried_interest     TEXT;

UPDATE securities   SET exercise_type = json_extract(payload, '$.exercise_type');
UPDATE fund_metrics SET accepted_date = json_extract(payload, '$.partner.accepted_date');

-- One row per K-1 document. The column names follow angellist's
-- k1_capital_accounts without its `_minor` suffix: money here is a decimal
-- string as printed (whole dollars on most K-1s), not integer cents.
-- distributions is the figure inside the form's parentheses on the
-- withdrawals line, so it is positive; every other figure carries the sign
-- the form prints. NULL where the form leaves the field blank.
CREATE TABLE k1_capital_accounts (
    content_sha256          TEXT    NOT NULL PRIMARY KEY,   -- the K-1 PDF (documents.content_sha256)
    doc_id                  INTEGER,                        -- documents.doc_id
    entity_external_id      INTEGER,                        -- the fund entity (index fund_id)
    tax_year                INTEGER,                        -- the form's calendar year
    beginning_capital       TEXT,                           -- item L: beginning capital account
    contributions           TEXT,                           -- item L: capital contributed during the year
    net_income              TEXT,                           -- item L: current year net income (loss)
    other_change            TEXT,                           -- item L: other increase (decrease)
    distributions           TEXT,                           -- item L: withdrawals and distributions
    cash_distributions      TEXT,                           -- box 19, code A
    property_distributions  TEXT,                           -- box 19, code C
    ending_capital          TEXT,                           -- item L: ending capital account
    short_term_gain         TEXT,                           -- box 8: net short-term capital gain (loss)
    long_term_gain          TEXT,                           -- box 9a: net long-term capital gain (loss)
    payload                 TEXT    NOT NULL                -- every figure read, as printed
);
CREATE INDEX ix_k1_entity ON k1_capital_accounts(entity_external_id, tax_year);

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s', 'now') AS INTEGER));

COMMIT;
