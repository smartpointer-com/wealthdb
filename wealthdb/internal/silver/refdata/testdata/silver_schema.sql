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
-- refdata silver schema v2: the facts behind the yield report.
--
-- Labels and figures stay as the providers served them, as in v1:
--   * lines gain a debt line's coupon kind, its default, arrears and
--     paid-in-kind flags, the exchange rate it was valued at, and the
--     duration and yield figures an issuer's file states per line.
--   * fund_risk holds a filing's interest-rate and credit-spread
--     sensitivities: what the fund's value changes by for a move of one
--     basis point (dv01) or one hundred (dv100) at one tenor.
--   * characteristics holds the figures a fund's page or filing states
--     for the fund as a whole: its duration and yields, its money-market
--     yields and maturities, and its credit-quality breakdown.
--   * period_facts holds a company's reported figures (earnings and
--     dividends per share, income, debt, cash), each for the period it
--     covers.
--
-- Conventions as in 0001. Percentages are in percent (4.25 is 4.25%),
-- durations in years, a money figure in its unit's currency.

ALTER TABLE lines ADD COLUMN coupon_kind       TEXT;     -- as served: N-PORT's Fixed, Floating, Variable, None
ALTER TABLE lines ADD COLUMN is_default        INTEGER;  -- 1, 0, or NULL where unstated
ALTER TABLE lines ADD COLUMN in_arrears        INTEGER;  -- interest payments in arrears: 1, 0 or NULL
ALTER TABLE lines ADD COLUMN paid_in_kind      INTEGER;  -- coupon paid in more of the same paper: 1, 0 or NULL
-- The rate the line was valued at: units of the line's currency per unit
-- of the composition's (per US dollar in an N-PORT filing).
ALTER TABLE lines ADD COLUMN exchange_rate     TEXT;
ALTER TABLE lines ADD COLUMN duration          TEXT;     -- years, as the provider names it (effective at iShares)
ALTER TABLE lines ADD COLUMN modified_duration TEXT;     -- years
ALTER TABLE lines ADD COLUMN yield_to_maturity TEXT;     -- percent
ALTER TABLE lines ADD COLUMN yield_to_worst    TEXT;     -- percent
ALTER TABLE lines ADD COLUMN yield_to_call     TEXT;     -- percent
ALTER TABLE lines ADD COLUMN real_duration     TEXT;     -- years, an inflation-linked line's
ALTER TABLE lines ADD COLUMN real_yield        TEXT;     -- percent, an inflation-linked line's real yield to maturity
ALTER TABLE lines ADD COLUMN accrual_date      TEXT;     -- YYYY-MM-DD, the day interest accrues from

-- One row per fund, day, currency and tenor. tenor is 3M, 1Y, 5Y, 10Y or
-- 30Y. An interest-rate row names the currency of the curve the
-- sensitivity is to; dv01 and dv100 are what the fund's value moves by,
-- in its composition's currency, for a move of one and of one hundred
-- basis points at that tenor. The credit-spread rows are the fund's as a
-- whole: currency XXX (ISO 4217's "no currency"), with credit_ig_dv01
-- and credit_hy_dv01 what it moves by for a one basis point move of
-- investment-grade and of non-investment-grade spreads. Every figure is
-- signed as filed. Filers follow no one sign convention: gold reads each
-- filing in one sign (fund_risk_signed). A request that states a fund's
-- sensitivities for a day replaces the rows of that fund and day; one
-- that states only a composition leaves them.
CREATE TABLE fund_risk (
    fund_ref        TEXT    NOT NULL,
    as_of           INTEGER NOT NULL,
    currency        TEXT    NOT NULL,
    tenor           TEXT    NOT NULL,
    dv01            TEXT,
    dv100           TEXT,
    credit_ig_dv01  TEXT,
    credit_hy_dv01  TEXT,
    run_at          INTEGER NOT NULL,
    payload         TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of, currency, tenor)
);

-- One row per fund, day, provider, field and label: a figure a provider
-- states for the fund as a whole. field is the figure (effective_duration,
-- yield_to_maturity, distribution_yield, sec_yield, seven_day_yield,
-- weighted_average_maturity, credit_quality, …); label is '' for a
-- single figure and the provider's own label for one part of a breakdown
-- (credit_quality AAA, BBB, Not Rated). value is a decimal: percent for
-- a yield or a share, years for a duration, days for a weighted average
-- maturity or life; a field whose unit is another one names it
-- (option_adjusted_spread_bp, weighted_average_maturity_years), and a
-- ratio is a plain number (convexity, price_earnings). A request that
-- states a figure for a fund and day replaces that provider's rows of
-- the figure for them, every part of a breakdown with it.
CREATE TABLE characteristics (
    fund_ref   TEXT    NOT NULL,
    as_of      INTEGER NOT NULL,
    provider   TEXT    NOT NULL,
    field      TEXT    NOT NULL,
    label      TEXT    NOT NULL DEFAULT '',
    value      TEXT    NOT NULL,
    run_at     INTEGER NOT NULL,
    payload    TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (fund_ref, as_of, provider, field, label)
);

-- One row per security, field, period and provider: a figure a company
-- reported for a period. as_of is the UTC midnight of the period's last
-- day; period_start is its first day, YYYY-MM-DD, and equals the last
-- day for a balance on a day (debt, cash). field is wealthdb's name for
-- the figure (eps_diluted, dividends_per_share, net_income, revenue,
-- operating_income, interest_expense, long_term_debt, cash,
-- operating_cash_flow, capex); scheme the concept the provider served
-- it under (us-gaap:EarningsPerShareDiluted). unit is the provider's
-- (USD, USD/shares). fiscal_period is FY, Q1, Q2, Q3 or Q4 as filed.
-- Where two filings state one period, the later filing's figure stays.
CREATE TABLE period_facts (
    security_key   TEXT    NOT NULL,
    field          TEXT    NOT NULL,
    as_of          INTEGER NOT NULL,
    period_start   TEXT    NOT NULL,
    provider       TEXT    NOT NULL,
    value          TEXT    NOT NULL,
    scheme         TEXT,
    unit           TEXT,
    fiscal_year    INTEGER,
    fiscal_period  TEXT,
    form           TEXT,
    filed          TEXT,                         -- YYYY-MM-DD
    run_at         INTEGER NOT NULL,
    payload        TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (security_key, field, as_of, period_start, provider)
);

CREATE INDEX idx_fund_risk_run       ON fund_risk (run_at);
CREATE INDEX idx_characteristics_run ON characteristics (run_at);
CREATE INDEX idx_period_facts_run    ON period_facts (run_at);

INSERT INTO schema_meta (silver_schema_version, applied_at)
VALUES (2, CAST(strftime('%s', 'now') AS INTEGER));
