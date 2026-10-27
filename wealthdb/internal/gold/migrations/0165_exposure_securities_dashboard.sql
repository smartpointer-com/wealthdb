-- ============================================================
-- gold schema, migration 0165 — the securities the Exposure dashboard
-- ranks.
--
-- report_exposure_security_slices holds report_exposure_securities'
-- rows (EXPOSURE.md §7) cut by what the dashboard's pickers narrow to:
-- one row per reporting currency, instant, source, tax wrapper, asset
-- class and security. A card sums a security's slices of the
-- selection, so a stock held through several funds is one row there.
-- The instants are the debt tables': today and the last second of each
-- of the 12 months before (gold.MaterializeExposure).
--
-- A slice keeps what the card needs to restate the view's row:
--
--   * name, ticker, market_cap and size, which a card takes the
--     greatest of, as the view does;
--   * sector and country: each the label of the slice's largest leaf
--     that has one, the greater label on a tie, with that leaf's
--     absolute value as its weight (sector_weight, country_weight);
--     class_weight is the slice's largest leaf. A card takes each label
--     from the slice of greatest weight, so it gives the view's labels;
--   * value, direct_value and via_funds_value;
--   * funds, the directly held funds the slice is reached through. A
--     card counts the distinct ones of its slices.
--
-- web_exposure_securities renders the instant as a day and reads a
-- missing tax wrapper as taxable_personal, as web_exposure does.
--
-- IF NOT EXISTS and OR REPLACE keep this replayable (see gold.Migrate's
-- REPLAY note).
-- ============================================================

CREATE TABLE IF NOT EXISTS report_exposure_security_slices (
    computed_at           BIGINT   NOT NULL,
    currency              TEXT     NOT NULL,
    as_of                 BIGINT   NOT NULL,
    silver_source_id      TEXT,
    tax_wrapper           TEXT,
    asset_class           TEXT,
    security              TEXT     NOT NULL,
    name                  TEXT,
    ticker                TEXT,
    sector                TEXT,
    country               TEXT,
    class_weight          DOUBLE,
    sector_weight         DOUBLE,
    country_weight        DOUBLE,
    market_cap            DOUBLE,
    size                  TEXT,
    value                 DOUBLE   NOT NULL,
    direct_value          DOUBLE   NOT NULL,
    via_funds_value       DOUBLE   NOT NULL,
    funds                 TEXT[]   NOT NULL
);

CREATE OR REPLACE VIEW web_exposure_securities AS
SELECT CAST(epoch_ms(as_of * 1000) AS DATE) AS as_of_day, currency, silver_source_id,
       COALESCE(tax_wrapper, 'taxable_personal') AS tax_wrapper, asset_class, security, name, ticker,
       sector, country, class_weight, sector_weight, country_weight, market_cap, size,
       value, direct_value, via_funds_value, funds
  FROM report_exposure_security_slices;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (165, CAST(epoch(now()) AS BIGINT));
