-- refdata silver schema v1.
--
-- Reference data as the providers served it: what each fund holds, what
-- each fund is, and what each security is. Labels stay as fetched (a
-- sector or a country in the provider's own words); the only computed
-- columns are helpers such as a line count or a weight sum. Everything
-- built from the data (label mappings, proxies, remainders) belongs to
-- gold. The Go adapter (wealthdb/internal/silver/refdata) reads this
-- shape; its testdata/silver_schema.sql is a copy, and the collector's
-- tests check that the two agree.
--
-- Conventions:
--   * Decimals (weights, amounts, rates) are strings, never REAL.
--   * as_of is the UTC midnight of the day a row describes, in Unix
--     seconds: the day a composition is as of, never the day it was
--     fetched or filed; for a security, the day of the lookup.
--   * run_at is the bronze run that wrote the row (dump_runs.snapshot_at).
--     A later run that serves the same fund and day replaces its rows.
--     lines carry none: a line belongs to its composition's run.
--   * Weights are in percent of the fund's net assets.
--   * payload is a JSON object, '{}' when there is nothing to add.
--
-- The schema runner (collectorkit.silver) records the applied version in
-- schema_meta; current version = MAX(silver_schema_version).

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                -- Unix seconds UTC
);

-- One row per loaded bronze run. loaded_at is when the load wrote the
-- run's rows, strictly increasing across loads; a load that re-parses a
-- run it loaded before stamps it again. It is the change clock gold
-- reads, so a run fetched earlier but loaded later still reaches gold.
CREATE TABLE dump_runs (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,   -- bronze run ts, Unix s UTC
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL,
    loaded_at             INTEGER NOT NULL                -- Unix s UTC
);

-- One row per fund, day and basis. basis is holdings, collateral (a swap
-- fund's substitute basket) or index (the index lines a provider lists
-- for a swap fund). source_kind is daily_file, filing, report or inbox.
CREATE TABLE compositions (
    fund_ref           TEXT    NOT NULL,
    as_of              INTEGER NOT NULL,
    basis              TEXT    NOT NULL,
    provider           TEXT    NOT NULL,
    source_kind        TEXT    NOT NULL,
    fund_name          TEXT,
    currency           TEXT,
    net_assets         TEXT,
    line_count         INTEGER NOT NULL,
    weight_sum         TEXT,
    units_outstanding  TEXT,
    run_at             INTEGER NOT NULL,
    payload            TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of, basis)
);

-- One row per composition line, in the provider's order (line_no).
-- The two legs of a currency forward sit in long_* and short_*, each at
-- notional in its own currency.
CREATE TABLE lines (
    fund_ref        TEXT    NOT NULL,
    as_of           INTEGER NOT NULL,
    basis           TEXT    NOT NULL,
    line_no         INTEGER NOT NULL,
    isin            TEXT,
    cusip           TEXT,
    figi            TEXT,
    ticker          TEXT,
    exchange        TEXT,
    name            TEXT,
    asset_type      TEXT,
    weight          TEXT,
    quantity        TEXT,
    market_value    TEXT,
    currency        TEXT,
    country         TEXT,
    risk_country    TEXT,
    sector          TEXT,
    industry        TEXT,
    maturity        TEXT,                        -- YYYY-MM-DD
    coupon          TEXT,
    long_currency   TEXT,
    long_amount     TEXT,
    short_currency  TEXT,
    short_amount    TEXT,
    payload         TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of, basis, line_no)
);

-- One row per fund and day: what the fund is.
CREATE TABLE attributes (
    fund_ref         TEXT    NOT NULL,
    as_of            INTEGER NOT NULL,
    domicile         TEXT,
    legal_form       TEXT,
    ucits            INTEGER,                    -- 1, 0, or NULL where unstated
    replication      TEXT,
    distribution     TEXT,
    ter              TEXT,                       -- percent a year
    hedged_currency  TEXT,
    lending          INTEGER,
    index_name       TEXT,
    depositary       TEXT,
    run_at           INTEGER NOT NULL,
    payload          TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of)
);

-- One row per fund, day, role and counterparty. role is swap or
-- derivative (any other derivative's counterparty); share is percent of
-- net assets.
CREATE TABLE counterparties (
    fund_ref  TEXT    NOT NULL,
    as_of     INTEGER NOT NULL,
    role      TEXT    NOT NULL,
    name      TEXT    NOT NULL,
    lei       TEXT,
    share     TEXT,
    run_at    INTEGER NOT NULL,
    payload   TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of, role, name)
);

-- One row per security, lookup day and provider: the identifiers the
-- lookup tied together. security_key is the key gold asked about.
CREATE TABLE securities (
    security_key      TEXT    NOT NULL,
    as_of             INTEGER NOT NULL,
    provider          TEXT    NOT NULL,
    isin              TEXT,
    cusip             TEXT,
    figi              TEXT,
    lei               TEXT,
    ticker            TEXT,
    exchange          TEXT,
    name              TEXT,
    security_type     TEXT,
    listing_currency  TEXT,
    run_at            INTEGER NOT NULL,
    payload           TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (security_key, as_of, provider)
);

-- One row per security, field, lookup day and provider: the value as the
-- provider states it, with the provider's scheme (sic, an issuer's
-- sector label, gleif_jurisdiction …).
CREATE TABLE security_facts (
    security_key  TEXT    NOT NULL,
    field         TEXT    NOT NULL,
    as_of         INTEGER NOT NULL,
    provider      TEXT    NOT NULL,
    value         TEXT    NOT NULL,
    scheme        TEXT,
    run_at        INTEGER NOT NULL,
    payload       TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (security_key, field, as_of, provider)
);

CREATE INDEX idx_compositions_run   ON compositions (run_at);
CREATE INDEX idx_attributes_run     ON attributes (run_at);
CREATE INDEX idx_counterparties_run ON counterparties (run_at);
CREATE INDEX idx_securities_run     ON securities (run_at);
CREATE INDEX idx_security_facts_run ON security_facts (run_at);

INSERT INTO schema_meta (silver_schema_version, applied_at)
VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));
