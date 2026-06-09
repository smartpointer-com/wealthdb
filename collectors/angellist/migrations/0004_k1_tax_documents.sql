-- K-1 tax-document data — the per-SPV historical capital account that
-- AngelList exposes ONLY via Schedule K-1 packages (PDF + a structured
-- CSV) and quarterly financial statements, not the API/UI.
--
--   tax_documents       — one row per downloaded document; provenance +
--                         completeness (a year's K-1 package keeps updating,
--                         "estimate_provided" -> "complete", until ~Aug of
--                         the following year).
--   k1_capital_accounts — per (tax_year, SPV) capital-account analysis
--                         parsed from the K-1 CSV: the SPV legal name + EIN
--                         (the investment entity), portfolio company, and
--                         contributions / distributions / net income /
--                         ending capital (tax basis), back to inception.
--
-- positions.tax_basis_capital_minor — a SEPARATE valuation column for the
-- K-1 tax-basis ending capital, kept distinct from total_value_minor (FMV)
-- so the two valuation bases never mix.

CREATE TABLE IF NOT EXISTS tax_documents (
    document_id      TEXT PRIMARY KEY,   -- AngelList doc id, else filename-derived
    doc_type         TEXT,               -- 'k1_packet' | 'financial_report'
    tax_year         INTEGER,
    period           TEXT,               -- financial reports: 'Q1'..; K-1: NULL
    document_status  TEXT,               -- 'complete' | 'estimate_provided'
    k1_count         INTEGER,
    total_k1_count   INTEGER,
    updated_at       TEXT,               -- AngelList updatedAt (re-download trigger)
    filename         TEXT,
    content_sha256   TEXT,               -- change detection / idempotent parse
    retrieved_at     INTEGER,
    payload          TEXT
);

CREATE TABLE IF NOT EXISTS k1_capital_accounts (
    tax_year                 INTEGER NOT NULL,
    fund_name                TEXT    NOT NULL,   -- SPV legal name (the investment entity)
    fund_tax_id              TEXT,               -- SPV EIN
    portfolio_company        TEXT,
    k1_status                TEXT,               -- 'Issued' | 'Not Issuing'
    final_k1                 INTEGER,            -- 0/1 (exit signal)
    beginning_capital_minor  INTEGER,
    contributions_minor      INTEGER,
    net_income_minor         INTEGER,            -- Current Year Net Income (Loss)
    other_change_minor       INTEGER,            -- Other Increase (Decrease)
    distributions_minor      INTEGER,            -- Withdrawals & Distributions
    cash_distributions_minor INTEGER,            -- Line 19(a) Cash Distributions
    ending_capital_minor     INTEGER,            -- tax-basis capital (the valuation)
    ending_capital_pct       REAL,
    vehicle_external_id      TEXT,               -- best-effort link to vehicles (by company name)
    source_document_id       TEXT,
    payload                  TEXT,               -- all CSV columns for this row
    PRIMARY KEY (tax_year, fund_name)
);
CREATE INDEX IF NOT EXISTS idx_k1_company ON k1_capital_accounts(portfolio_company);
CREATE INDEX IF NOT EXISTS idx_k1_vehicle ON k1_capital_accounts(vehicle_external_id);

ALTER TABLE positions ADD COLUMN tax_basis_capital_minor INTEGER;

INSERT INTO schema_meta (silver_schema_version) VALUES (4);
