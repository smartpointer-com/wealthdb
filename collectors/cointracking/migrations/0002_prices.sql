-- cointracking silver schema v2 — price tables.
--
-- Three new tables:
--
--   `portfolio_prices` — what CoinTracking reports per portfolio,
--     scraped from /overview.php. Quote currency varies by
--     portfolio (each portfolio's "main fiat" setting in CT —
--     can be EUR, USD, etc.). Stored verbatim so the gold layer
--     can reproduce CT's per-portfolio totals byte-exactly
--     without re-running an FX layer.
--
--   `coin_prices` — canonical USD prices per coin per day, fetched
--     from an external market-data provider (current: Binance
--     public spot, USDT-denominated; the small USDT/USD basis is
--     absorbed). The cross-source reference used to FX-convert
--     non-USD portfolios into the wealthdb canonical USD.
--
--   `coin_mapping` — CT-ticker (`BTC`) → external-provider-id
--     (`BTCUSDT` on Binance) translation. Keyed on (instrument,
--     provider) so we can swap providers without touching the
--     schema. Built lazily from the provider's master list on
--     first fetch.

CREATE TABLE IF NOT EXISTS portfolio_prices (
    as_of_date             DATE NOT NULL,
    portfolio_external_id  VARCHAR NOT NULL,
    instrument_external_id VARCHAR NOT NULL,
    quote_currency         VARCHAR NOT NULL,
    price                  DECIMAL(38, 18) NOT NULL,
    snapshot_at            BIGINT NOT NULL,
    PRIMARY KEY (as_of_date, portfolio_external_id, instrument_external_id, quote_currency)
);

CREATE TABLE IF NOT EXISTS coin_prices (
    as_of_date             DATE NOT NULL,
    instrument_external_id VARCHAR NOT NULL,
    price_usd              DECIMAL(38, 18) NOT NULL,
    source                 VARCHAR NOT NULL,
    fetched_at             BIGINT NOT NULL,
    PRIMARY KEY (as_of_date, instrument_external_id, source)
);

CREATE TABLE IF NOT EXISTS coin_mapping (
    instrument_external_id VARCHAR NOT NULL,
    provider               VARCHAR NOT NULL,   -- 'binance', etc.
    provider_coin_id       VARCHAR,            -- NULL = no match recorded
    payload                JSON,
    mapped_at              BIGINT NOT NULL,
    PRIMARY KEY (instrument_external_id, provider)
);

INSERT INTO schema_meta (silver_schema_version) VALUES (2)
ON CONFLICT DO NOTHING;
