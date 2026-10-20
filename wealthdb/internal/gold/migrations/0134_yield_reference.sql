-- ============================================================
-- gold schema, migration 0134 — the reference data behind the yield
-- report, and the `yield` config block.
--
-- Loaded, windowed by as_of like the reference tables of 0125:
--
--   * fund_lines gains a debt line's coupon kind, its default and
--     arrears flags, the exchange rate it was valued at, and the
--     duration and yield figures an issuer's file states per line;
--   * fund_risk: a filing's rate sensitivities, per fund, day,
--     currency and tenor;
--   * fund_characteristics: the figures a provider states for a fund as
--     a whole (its duration and yields, its money-market yields, its
--     credit-quality breakdown);
--   * security_period_facts: a company's reported figures, each for the
--     period it covers, dated by the period's last day;
--   * rate_observations: the public rate series, one value per series
--     and day, in percent.
--
-- Config, replaced whole at every load like the exposure block
-- (gold.SyncYield): yield_curves, yield_withholding,
-- yield_rating_funds, yield_conventions, yield_cycles and
-- yield_settings.
--
-- OR REPLACE and IF NOT EXISTS keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS coupon_kind       TEXT;
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS is_default        BOOLEAN;
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS in_arrears        BOOLEAN;
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS paid_in_kind      BOOLEAN;
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS exchange_rate     DECIMAL(28, 12);
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS duration          DECIMAL(18, 8);
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS modified_duration DECIMAL(18, 8);
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS yield_to_maturity DECIMAL(18, 8);
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS yield_to_worst    DECIMAL(18, 8);
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS yield_to_call     DECIMAL(18, 8);
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS real_duration     DECIMAL(18, 8);
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS real_yield        DECIMAL(18, 8);
ALTER TABLE fund_lines ADD COLUMN IF NOT EXISTS accrual_date      DATE;

-- fund_risk: one row per fund, day, currency and tenor (3M, 1Y, 5Y, 10Y,
-- 30Y). An interest-rate row names the curve's currency; dv01 and dv100
-- are what the fund's value moves by for a move of one and of one
-- hundred basis points at the tenor. A credit-spread row has currency
-- XXX and carries what it moves by for a one basis point move of
-- investment-grade and of non-investment-grade spreads. Amounts are in
-- the currency of the fund's composition that day, signed as filed:
-- positive when the fund loses value as rates rise.
CREATE TABLE IF NOT EXISTS fund_risk (
    silver_source_id  TEXT           NOT NULL,
    fund_ref          TEXT           NOT NULL,
    as_of             BIGINT         NOT NULL,
    currency          TEXT           NOT NULL,
    tenor             TEXT           NOT NULL,
    dv01              DECIMAL(28, 4),
    dv100             DECIMAL(28, 4),
    credit_ig_dv01    DECIMAL(28, 4),
    credit_hy_dv01    DECIMAL(28, 4),
    payload           JSON,
    PRIMARY KEY (silver_source_id, fund_ref, as_of, currency, tenor)
);

-- fund_characteristics: one figure a provider states for a fund as a
-- whole on one day. label is '' for a single figure, the provider's own
-- label for one part of a breakdown (credit_quality). value is percent
-- for a yield or a share, years for a duration, days for a weighted
-- average maturity or life; a field whose unit is another one names it
-- (option_adjusted_spread_bp, weighted_average_maturity_years), and a
-- ratio is a plain number (convexity, price_earnings).
CREATE TABLE IF NOT EXISTS fund_characteristics (
    silver_source_id  TEXT           NOT NULL,
    fund_ref          TEXT           NOT NULL,
    as_of             BIGINT         NOT NULL,
    provider          TEXT           NOT NULL,
    field             TEXT           NOT NULL,
    label             TEXT           NOT NULL,
    value             DECIMAL(28, 8) NOT NULL,
    payload           JSON,
    PRIMARY KEY (silver_source_id, fund_ref, as_of, provider, field, label)
);

CREATE INDEX IF NOT EXISTS ix_fund_characteristics_field ON fund_characteristics(field);

-- security_period_facts: a figure a company reported for a period. as_of
-- is the UTC midnight of the period's last day, period_start its first
-- (the last day itself for a balance on a day). field is wealthdb's
-- name for the figure, scheme the concept the provider served it under.
CREATE TABLE IF NOT EXISTS security_period_facts (
    silver_source_id  TEXT           NOT NULL,
    security_key      TEXT           NOT NULL,
    field             TEXT           NOT NULL,
    as_of             BIGINT         NOT NULL,
    period_start      DATE           NOT NULL,
    provider          TEXT           NOT NULL,
    value             DECIMAL(28, 8) NOT NULL,
    scheme            TEXT,
    unit              TEXT,
    fiscal_year       INTEGER,
    fiscal_period     TEXT,
    form              TEXT,
    filed             DATE,
    payload           JSON,
    PRIMARY KEY (silver_source_id, security_key, field, as_of, period_start, provider)
);

CREATE INDEX IF NOT EXISTS ix_security_period_facts_key ON security_period_facts(security_key);

-- rate_observations: one day's value of a published rate series, in
-- percent. The yield config's curves name a series by its source and id.
CREATE TABLE IF NOT EXISTS rate_observations (
    silver_source_id  TEXT           NOT NULL,
    series_id         TEXT           NOT NULL,
    as_of             BIGINT         NOT NULL,
    value             DECIMAL(18, 8) NOT NULL,
    payload           JSON,
    PRIMARY KEY (silver_source_id, series_id, as_of)
);

-- The `yield` config block, replaced whole at every load.
--
-- yield_curves: the public curve of a currency, one series per tenor.
-- tenor is the config's (3M, 1Y, …), tenor_months the same in months.
CREATE TABLE IF NOT EXISTS yield_curves (
    currency          TEXT     NOT NULL,
    tenor             TEXT     NOT NULL,
    tenor_months      INTEGER  NOT NULL,
    silver_source_id  TEXT     NOT NULL,
    series_id         TEXT     NOT NULL,
    PRIMARY KEY (currency, tenor)
);

-- yield_withholding: the tax withheld at source on income, in percent, by
-- the holding's situs country and the account's tax wrapper; wrapper
-- 'default' stands for every wrapper the country names no rate for.
CREATE TABLE IF NOT EXISTS yield_withholding (
    country   TEXT    NOT NULL,
    wrapper   TEXT    NOT NULL,
    rate_pct  DOUBLE  NOT NULL,
    PRIMARY KEY (country, wrapper)
);

-- yield_rating_funds: a fund sliced by credit rating, whose lines carry
-- its bucket or range of buckets. rating is the config's text (BBB,
-- AAA-A); bucket_high and bucket_low its two ends.
CREATE TABLE IF NOT EXISTS yield_rating_funds (
    fund_ref     TEXT  NOT NULL PRIMARY KEY,
    rating       TEXT  NOT NULL,
    bucket_high  TEXT  NOT NULL,
    bucket_low   TEXT  NOT NULL
);

-- yield_conventions: coupon frequency (payments a year, 0 for a discount
-- instrument) and day count per market. market is a currency, or a
-- currency and a bond sector or `bill` (USD:government, USD:bill).
-- origin is builtin or config; a config entry replaces the built-in one.
CREATE TABLE IF NOT EXISTS yield_conventions (
    market     TEXT     NOT NULL PRIMARY KEY,
    frequency  INTEGER  NOT NULL,
    day_count  TEXT     NOT NULL,
    origin     TEXT     NOT NULL
);

-- yield_cycles: the cycle of each of the eleven sectors (cyclical,
-- defensive, sensitive), the built-in map with the config's overrides.
CREATE TABLE IF NOT EXISTS yield_cycles (
    sector  TEXT  NOT NULL PRIMARY KEY,
    cycle   TEXT  NOT NULL,
    origin  TEXT  NOT NULL
);

-- yield_settings: one row. horizon_months is the projection's window
-- when none is given.
CREATE TABLE IF NOT EXISTS yield_settings (
    horizon_months  INTEGER  NOT NULL
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (134, CAST(epoch(now()) AS BIGINT));
