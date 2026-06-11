-- ============================================================
-- ubs-web silver schema, migration 0004 — mortgages.
--
-- UBS surfaces mortgages in positions.csv under
-- `Group of products = 'Pro memoria - Mortgages'`. The CSV row is
-- a per-mortgage liability, not a regular security holding:
--
--   - The IBAN column is reused to carry the fixed-rate term as
--     "dd.mm.yyyy - dd.mm.yyyy" (so iban_canonical() rejected the
--     row entirely in earlier loads — they dropped on the floor).
--   - Number/Amt. is the (negative) outstanding principal.
--   - Description / Description 1 carry the product name
--     ("UBS Fixed-Rate Mortgage", "UBS Variable-Rate Mortgage").
--   - Description 3 / Sector carry the collateral property
--     address.
--   - `Product` is the UBS-internal mortgage account number
--     (`BBBB AAAAAAAA.MMM NNNN`-shaped, where MMM is a 3-char
--     mortgage-type code), which we use as the account_external_id.
--
-- Schema kept separate from `accounts` because:
--   - kind would be a third value alongside cash/safekeeping and
--     the CHECK constraint is column-shape-coupled to those.
--   - account_external_id here is NOT an IBAN; the IBAN-derived
--     columns on `accounts` (iban, account_acct_id_psn_form,
--     ...) are not meaningful for a mortgage.
--   - Mortgage-specific fields (term, rate type, collateral)
--     deserve named columns rather than living buried in payload.
--
-- The gold UBS adapter projects each mortgage row as one
-- AccountChange{Kind: mortgage} + InstrumentChange{AssetClass:
-- mortgage} + PositionChange (with a negative MarketValue),
-- using `account_external_id` as both the account ID and the
-- synthetic instrument ID.
-- ============================================================

PRAGMA foreign_keys = ON;

CREATE TABLE mortgages (
    snapshot_at              INTEGER NOT NULL,
    account_external_id      TEXT    NOT NULL,        -- UBS-internal mortgage no.
    banking_relationship_id  TEXT,
    portfolio_external_id    TEXT,
    currency_iso             TEXT    NOT NULL,
    outstanding_balance      REAL,                    -- negative (liability)
    start_date               INTEGER,                 -- Unix seconds UTC (term start)
    end_date                 INTEGER,                 -- Unix seconds UTC (term end)
    rate_type                TEXT,                    -- 'fixed' | 'variable' | NULL
    collateral_description   TEXT,
    description              TEXT,
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);

CREATE INDEX ix_mortgages_account ON mortgages(account_external_id);


INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (4, CAST(strftime('%s', 'now') AS INTEGER));
