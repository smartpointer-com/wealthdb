-- ============================================================
-- wealthdb gold schema, migration 0001 — initial schema.
--
-- Mirrors docs/DESIGN.md §7.2. Migration discipline: each change
-- lands as a new numbered file in this directory. schema.go's
-- Migrate() reads MAX(gold_schema_version) from schema_meta and
-- applies any migration with a higher number, in order. Forward-
-- only; no backwards-compatible drift.
--
-- Conventions:
--   timestamps  BIGINT Unix seconds UTC
--   money       DECIMAL(28, 4)
--   quantities  DECIMAL(28, 8)
--   FX rates    DECIMAL(20, 10)
--   currency    TEXT, ISO 4217
--   identifiers TEXT, silver-source-scoped
--   payloads    JSON
-- ============================================================

-- ============================================================
-- META / BOOKKEEPING
-- ============================================================

CREATE TABLE schema_meta (
    gold_schema_version INTEGER PRIMARY KEY,
    applied_at          BIGINT  NOT NULL
);

CREATE TABLE silver_sources (
    silver_source_id    TEXT    PRIMARY KEY,
    silver_kind         TEXT    NOT NULL CHECK (silver_kind IN ('schwab', 'ubs', 'swissquote')),
    silver_path         TEXT    NOT NULL,
    high_watermark      BIGINT  NOT NULL,
    first_loaded_at     BIGINT  NOT NULL,
    last_loaded_at      BIGINT  NOT NULL
);

CREATE TABLE load_audit (
    silver_source_id        TEXT    NOT NULL,
    loaded_at               BIGINT  NOT NULL,
    change_number_before    BIGINT,
    change_number_after     BIGINT  NOT NULL,
    window_start            BIGINT  NOT NULL,
    window_end              BIGINT  NOT NULL,
    snapshots_loaded        INTEGER NOT NULL,
    transactions_loaded     INTEGER NOT NULL,
    PRIMARY KEY (silver_source_id, loaded_at),
    FOREIGN KEY (silver_source_id) REFERENCES silver_sources(silver_source_id)
);

-- ============================================================
-- DIMENSIONS — accounts and instruments
-- ============================================================

CREATE TABLE accounts (
    silver_source_id        TEXT    NOT NULL,
    account_external_id     TEXT    NOT NULL,
    account_kind            TEXT    NOT NULL,
    display_name            TEXT,
    base_currency           TEXT,
    relationship_id         TEXT,
    first_seen_at           BIGINT  NOT NULL,
    last_seen_at            BIGINT  NOT NULL,
    payload                 JSON,
    PRIMARY KEY (silver_source_id, account_external_id),
    FOREIGN KEY (silver_source_id) REFERENCES silver_sources(silver_source_id)
);

CREATE TABLE instruments (
    silver_source_id        TEXT    NOT NULL,
    instrument_external_id  TEXT    NOT NULL,
    asset_class             TEXT    NOT NULL,
    isin                    TEXT,
    cusip                   TEXT,
    symbol                  TEXT,
    name                    TEXT,
    currency                TEXT,
    first_seen_at           BIGINT  NOT NULL,
    last_seen_at            BIGINT  NOT NULL,
    payload                 JSON,
    PRIMARY KEY (silver_source_id, instrument_external_id),
    FOREIGN KEY (silver_source_id) REFERENCES silver_sources(silver_source_id)
);

CREATE INDEX ix_instruments_isin   ON instruments(isin);
CREATE INDEX ix_instruments_symbol ON instruments(symbol);

-- ============================================================
-- FACTS — positions and cash balances (snapshot grain)
-- ============================================================

CREATE TABLE positions (
    silver_source_id        TEXT             NOT NULL,
    snapshot_at             BIGINT           NOT NULL,
    account_external_id     TEXT             NOT NULL,
    position_key            TEXT             NOT NULL,
    instrument_external_id  TEXT,
    asset_class             TEXT             NOT NULL,
    currency                TEXT             NOT NULL,
    quantity                DECIMAL(28, 8),
    market_value            DECIMAL(28, 4),
    book_value              DECIMAL(28, 4),
    accrued_interest        DECIMAL(28, 4),
    acquisition_date        DATE,
    payload                 JSON,
    PRIMARY KEY (silver_source_id, snapshot_at, account_external_id, position_key),
    FOREIGN KEY (silver_source_id, account_external_id)
        REFERENCES accounts(silver_source_id, account_external_id)
);

CREATE TABLE cash_balances (
    silver_source_id        TEXT             NOT NULL,
    snapshot_at             BIGINT           NOT NULL,
    account_external_id     TEXT             NOT NULL,
    currency                TEXT             NOT NULL,
    balance_kind            TEXT             NOT NULL,
    amount                  DECIMAL(28, 4)   NOT NULL,
    payload                 JSON,
    PRIMARY KEY (silver_source_id, snapshot_at, account_external_id, currency, balance_kind),
    FOREIGN KEY (silver_source_id, account_external_id)
        REFERENCES accounts(silver_source_id, account_external_id)
);

-- ============================================================
-- FACTS — FX rates (snapshot grain)
-- ============================================================

CREATE TABLE fx_rates (
    silver_source_id        TEXT             NOT NULL,
    snapshot_at             BIGINT           NOT NULL,
    base_currency           TEXT             NOT NULL,
    quote_currency          TEXT             NOT NULL,
    mid_rate                DECIMAL(20, 10)  NOT NULL,
    bid_rate                DECIMAL(20, 10),
    ask_rate                DECIMAL(20, 10),
    payload                 JSON,
    PRIMARY KEY (silver_source_id, snapshot_at, base_currency, quote_currency)
);

CREATE INDEX ix_fx_rates_pair_time
    ON fx_rates(base_currency, quote_currency, snapshot_at);

-- ============================================================
-- FACTS — transactions (event grain)
-- ============================================================

CREATE TABLE transactions (
    silver_source_id        TEXT             NOT NULL,
    transaction_external_id TEXT             NOT NULL,
    occurred_at             BIGINT           NOT NULL,
    account_external_id     TEXT             NOT NULL,
    instrument_external_id  TEXT,
    kind                    TEXT             NOT NULL,
    currency                TEXT             NOT NULL,
    gross_amount            DECIMAL(28, 4),
    net_amount              DECIMAL(28, 4),
    quantity                DECIMAL(28, 8),
    price                   DECIMAL(28, 8),
    payload                 JSON,
    PRIMARY KEY (silver_source_id, transaction_external_id),
    FOREIGN KEY (silver_source_id, account_external_id)
        REFERENCES accounts(silver_source_id, account_external_id)
);

CREATE INDEX ix_transactions_account_time
    ON transactions(silver_source_id, account_external_id, occurred_at);

CREATE INDEX ix_transactions_kind_time
    ON transactions(kind, occurred_at);

-- ============================================================
-- Mark migration applied. Must be the last statement; the loader
-- uses MAX(gold_schema_version) as its "what's applied" marker.
-- ============================================================
INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (1, CAST(epoch(now()) AS BIGINT));
