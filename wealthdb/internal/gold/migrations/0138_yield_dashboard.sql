-- ============================================================
-- gold schema, migration 0138 — the tables the Metabase Yield
-- dashboard, the Income dashboard's projected section and the Exposure
-- dashboard's debt cards read (web/DESIGN.md §8).
--
-- The yield report and the income projection are computed in Go
-- (internal/yield), so `web-materialize` writes their figures here
-- (gold.MaterializeYield), as it does for returns, gains and exposure:
--
--   * report_yield_positions: each holding's figures in every reporting
--     currency, today and at the last second of each of the 12 months
--     before;
--   * report_yield_buckets: each holding's DV01 per curve currency and
--     tenor, at the same instants;
--   * report_yield_parts: today's projection, one row per part (a
--     holding's income in a month under a maturing rule), in the
--     holding's own currency, with its rate to each reporting currency
--     (fx_<CCY>); a card sums Fixed + Roll[path] + Principal × (rate +
--     spread + shift) / 1200 + RollPrincipal × shift / 1200 over the
--     months of its horizon (yield.Part.Income), the rate from
--     report_yield_rates;
--   * report_yield_rates: each currency's benchmark rate per month and
--     path;
--   * report_yield_events: the principals maturities return, per rule;
--   * report_yield_received: the income lines the projection counts as
--     received, of this year and the last, per reporting currency.
--
-- report_exposure_debt holds the buckets of credit, maturities, coupons
-- and cycles (exposure_credit_leaves, exposure_maturity_leaves,
-- exposure_yield_leaves) at today and the 12 month ends before, by the
-- source and tax wrapper the pickers narrow to; report_exposure_issuers
-- the largest issuers at the same instants (gold.MaterializeExposure).
--
-- IF NOT EXISTS and OR REPLACE keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

CREATE TABLE IF NOT EXISTS report_yield_positions (
    computed_at           BIGINT   NOT NULL,
    as_of                 BIGINT   NOT NULL,
    currency              TEXT     NOT NULL,
    silver_source_id      TEXT     NOT NULL,
    account_external_id   TEXT     NOT NULL,
    position_key          TEXT     NOT NULL,
    tax_wrapper           TEXT,
    symbol                TEXT,
    name                  TEXT,
    asset_class           TEXT,
    vehicle               TEXT,
    kind                  TEXT     NOT NULL,
    holding_currency      TEXT,
    quantity              DOUBLE,
    value                 DOUBLE   NOT NULL,
    price                 DOUBLE,
    coupon_pct            DOUBLE,
    maturity_date         DATE,
    rating                TEXT,
    stated_pct            DOUBLE,
    current_yield_pct     DOUBLE,
    implied_pct           DOUBLE,
    implied_annual_pct    DOUBLE,
    realized_pct          DOUBLE,
    earnings_yield_pct    DOUBLE,
    dividend_yield_pct    DOUBLE,
    payout_pct            DOUBLE,
    benchmark_pct         DOUBLE,
    gap_pct               DOUBLE,
    duration              DOUBLE,
    convexity             DOUBLE,
    maturity_years        DOUBLE,
    dv01                  DOUBLE,
    income_12m            DOUBLE,
    income_100bp          DOUBLE,
    -- weighs says the figure counts in a row's rates: no liability, no
    -- line of no positive value (yield.summarize).
    weighs                BOOLEAN  NOT NULL,
    tags                  VARCHAR[]
);

CREATE TABLE IF NOT EXISTS report_yield_buckets (
    computed_at           BIGINT   NOT NULL,
    as_of                 BIGINT   NOT NULL,
    currency              TEXT     NOT NULL,
    silver_source_id      TEXT     NOT NULL,
    account_external_id   TEXT     NOT NULL,
    position_key          TEXT     NOT NULL,
    curve_currency        TEXT     NOT NULL,
    tenor                 TEXT     NOT NULL,
    kind                  TEXT     NOT NULL,
    dv01                  DOUBLE   NOT NULL
);

CREATE TABLE IF NOT EXISTS report_yield_parts (
    computed_at           BIGINT   NOT NULL,
    silver_source_id      TEXT     NOT NULL,
    account_external_id   TEXT     NOT NULL,
    position_key          TEXT     NOT NULL,
    holding_currency      TEXT     NOT NULL,
    rule                  TEXT     NOT NULL,
    month                 INTEGER  NOT NULL,
    month_start           DATE     NOT NULL,
    day                   DATE,
    month_only            BOOLEAN  NOT NULL,
    type                  TEXT     NOT NULL,
    basis                 TEXT     NOT NULL,
    reinvested            TEXT,
    fixed                 DOUBLE   NOT NULL,
    principal             DOUBLE   NOT NULL,
    spread                DOUBLE   NOT NULL,
    roll_constant         DOUBLE   NOT NULL,
    roll_forward          DOUBLE   NOT NULL,
    roll_principal        DOUBLE   NOT NULL,
    withholding_pct       DOUBLE,
    fx_USD                DOUBLE,
    fx_CHF                DOUBLE,
    fx_EUR                DOUBLE,
    fx_GBP                DOUBLE
);

CREATE TABLE IF NOT EXISTS report_yield_rates (
    computed_at           BIGINT   NOT NULL,
    currency              TEXT     NOT NULL,
    month                 INTEGER  NOT NULL,
    path                  TEXT     NOT NULL,
    rate_pct              DOUBLE   NOT NULL
);

CREATE TABLE IF NOT EXISTS report_yield_events (
    computed_at           BIGINT   NOT NULL,
    silver_source_id      TEXT     NOT NULL,
    account_external_id   TEXT     NOT NULL,
    position_key          TEXT     NOT NULL,
    holding_currency      TEXT     NOT NULL,
    rule                  TEXT     NOT NULL,
    month                 INTEGER  NOT NULL,
    day                   DATE     NOT NULL,
    type                  TEXT     NOT NULL,
    amount                DOUBLE   NOT NULL,
    fx_USD                DOUBLE,
    fx_CHF                DOUBLE,
    fx_EUR                DOUBLE,
    fx_GBP                DOUBLE
);

CREATE TABLE IF NOT EXISTS report_yield_received (
    computed_at           BIGINT   NOT NULL,
    currency              TEXT     NOT NULL,
    silver_source_id      TEXT     NOT NULL,
    account_external_id   TEXT     NOT NULL,
    position_key          TEXT     NOT NULL,
    day                   DATE     NOT NULL,
    type                  TEXT     NOT NULL,
    value                 DOUBLE   NOT NULL,
    withheld              DOUBLE
);

CREATE TABLE IF NOT EXISTS report_exposure_debt (
    computed_at           BIGINT   NOT NULL,
    currency              TEXT     NOT NULL,
    as_of                 BIGINT   NOT NULL,
    silver_source_id      TEXT,
    tax_wrapper           TEXT,
    via_funds             BOOLEAN  NOT NULL,
    dimension             TEXT     NOT NULL,
    bucket                TEXT     NOT NULL,
    unknown               BOOLEAN  NOT NULL,
    value                 DOUBLE   NOT NULL
);

CREATE TABLE IF NOT EXISTS report_exposure_issuers (
    computed_at           BIGINT   NOT NULL,
    currency              TEXT     NOT NULL,
    as_of                 BIGINT   NOT NULL,
    rank                  INTEGER  NOT NULL,
    issuer                TEXT     NOT NULL,
    lei                   TEXT,
    value                 DOUBLE   NOT NULL,
    share_pct             DOUBLE,
    cumulative_share_pct  DOUBLE,
    equity_pct            DOUBLE,
    debt_pct              DOUBLE,
    cash_pct              DOUBLE
);

-- The dashboards' views. A holding's labels are its row's in the
-- positions of today; an account's income no holding names (rent, a
-- pension: `income:<type>:<CCY>`) reads its account's. A day is a DATE; a rate
-- prints as `_pct`; money is in the holding's currency with fx_<ccy> to
-- each reporting currency, or a row filter on `currency`.
CREATE OR REPLACE VIEW web_yield AS
SELECT CAST(epoch_ms(p.as_of * 1000) AS DATE) AS as_of_day, p.as_of = (SELECT max(as_of) FROM report_yield_positions)
           AS today,
       p.currency, p.silver_source_id, p.account_external_id, p.position_key,
       account_label(a.display_name, p.account_external_id, p.silver_source_id, a.account_kind) AS account_label,
       COALESCE(p.tax_wrapper, 'taxable_personal') AS tax_wrapper,
       COALESCE(p.asset_class, 'unknown') AS asset_class, p.vehicle, p.kind, p.symbol, p.name, p.holding_currency,
       p.quantity, p.price, p.value, p.coupon_pct, p.maturity_date, p.rating, p.stated_pct, p.current_yield_pct,
       p.implied_pct, p.implied_annual_pct, p.realized_pct, p.earnings_yield_pct, p.dividend_yield_pct, p.payout_pct,
       p.benchmark_pct, p.gap_pct, p.duration, p.convexity, p.maturity_years, p.dv01, p.income_12m, p.income_100bp,
       p.weighs, p.tags
  FROM report_yield_positions p
  LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id AND a.account_external_id = p.account_external_id;

CREATE OR REPLACE VIEW web_yield_curve AS
SELECT CAST(epoch_ms(b.as_of * 1000) AS DATE) AS as_of_day, b.currency, b.silver_source_id,
       COALESCE(p.tax_wrapper, 'taxable_personal') AS tax_wrapper, COALESCE(p.asset_class, 'unknown') AS asset_class,
       b.curve_currency, b.tenor, list_position(['3M', '1Y', '5Y', '10Y', '30Y'], b.tenor) AS tenor_order,
       CASE WHEN b.kind IN ('bond_fund', 'money_market') THEN 'funds' WHEN b.kind = 'liability' THEN 'liabilities'
            WHEN b.kind IN ('deposit', 'term_deposit') THEN 'cash' ELSE 'bonds' END AS held_in,
       b.dv01
  FROM report_yield_buckets b
  LEFT JOIN report_yield_positions p ON p.as_of = b.as_of AND p.currency = b.currency
       AND p.silver_source_id = b.silver_source_id AND p.account_external_id = b.account_external_id
       AND p.position_key = b.position_key;

-- web_yield_holdings: the attributes of each holding of today's books,
-- once, from the first reporting currency whose book holds it.
CREATE OR REPLACE VIEW web_yield_holdings AS
SELECT p.silver_source_id, p.account_external_id, p.position_key,
       account_label(a.display_name, p.account_external_id, p.silver_source_id, a.account_kind) AS account_label,
       COALESCE(p.tax_wrapper, 'taxable_personal') AS tax_wrapper,
       COALESCE(p.asset_class, 'unknown') AS asset_class, p.vehicle, p.kind, p.symbol, p.name, p.quantity, p.price
  FROM report_yield_positions p
  LEFT JOIN accounts a ON a.silver_source_id = p.silver_source_id AND a.account_external_id = p.account_external_id
 WHERE p.as_of = (SELECT max(as_of) FROM report_yield_positions)
QUALIFY row_number() OVER (PARTITION BY p.silver_source_id, p.account_external_id, p.position_key
                           ORDER BY list_position(['USD', 'CHF', 'EUR', 'GBP'], p.currency)) = 1;

CREATE OR REPLACE VIEW web_yield_parts AS
SELECT x.*, COALESCE(h.account_label,
                     account_label(a.display_name, x.account_external_id, x.silver_source_id, a.account_kind))
           AS account_label,
       COALESCE(h.tax_wrapper, a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
       COALESCE(h.asset_class, 'income') AS asset_class, h.vehicle, h.kind, h.symbol, h.name
  FROM report_yield_parts x
  LEFT JOIN web_yield_holdings h ON h.silver_source_id = x.silver_source_id
       AND h.account_external_id = x.account_external_id AND h.position_key = x.position_key
  LEFT JOIN accounts a ON a.silver_source_id = x.silver_source_id AND a.account_external_id = x.account_external_id;

CREATE OR REPLACE VIEW web_yield_events AS
SELECT x.*, h.account_label, COALESCE(h.tax_wrapper, 'taxable_personal') AS tax_wrapper,
       COALESCE(h.asset_class, 'unknown') AS asset_class, h.vehicle, h.symbol, h.name
  FROM report_yield_events x
  LEFT JOIN web_yield_holdings h ON h.silver_source_id = x.silver_source_id
       AND h.account_external_id = x.account_external_id AND h.position_key = x.position_key;

CREATE OR REPLACE VIEW web_yield_rates AS SELECT currency, month, path, rate_pct FROM report_yield_rates;

CREATE OR REPLACE VIEW web_yield_received AS
SELECT x.currency, x.silver_source_id, x.account_external_id, x.position_key, x.day, x.type, x.value, x.withheld,
       COALESCE(h.account_label,
                account_label(a.display_name, x.account_external_id, x.silver_source_id, a.account_kind))
           AS account_label,
       COALESCE(h.tax_wrapper, a.tax_wrapper, 'taxable_personal') AS tax_wrapper,
       COALESCE(h.asset_class, CASE WHEN starts_with(x.position_key, 'income:') THEN 'income' ELSE 'unknown' END)
           AS asset_class, h.vehicle, h.symbol, h.name
  FROM report_yield_received x
  LEFT JOIN web_yield_holdings h ON h.silver_source_id = x.silver_source_id
       AND h.account_external_id = x.account_external_id AND h.position_key = x.position_key
  LEFT JOIN accounts a ON a.silver_source_id = x.silver_source_id AND a.account_external_id = x.account_external_id;

CREATE OR REPLACE VIEW web_exposure_debt AS
SELECT CAST(epoch_ms(as_of * 1000) AS DATE) AS as_of_day, currency, silver_source_id,
       COALESCE(tax_wrapper, 'taxable_personal') AS tax_wrapper,
       CASE WHEN via_funds THEN 'via funds' ELSE 'direct' END AS held, dimension, bucket, unknown, value
  FROM report_exposure_debt;

CREATE OR REPLACE VIEW web_exposure_issuers AS
SELECT CAST(epoch_ms(as_of * 1000) AS DATE) AS as_of_day, currency, rank, issuer, lei, value, share_pct,
       cumulative_share_pct, equity_pct, debt_pct, cash_pct
  FROM report_exposure_issuers;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (138, CAST(epoch(now()) AS BIGINT));
