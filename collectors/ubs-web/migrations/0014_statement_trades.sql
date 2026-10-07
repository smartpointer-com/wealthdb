-- ============================================================
-- ubs-web silver schema, migration 0014 — the transaction list a
-- Statement of assets prints.
--
-- A Statement of assets may close with a "Transaction list": every
-- securities booking in its period, with the quantity, the price, the
-- charges and, for a sale, the cost of what was sold and the realized
-- P/L (DESIGN.md §3.10). It is the only record of a portfolio's trades
-- that states quantities for the years before the portfolio export
-- (§3.7b) reaches back.
--
-- One row per booking per statement, as printed. A booking recurs in
-- every statement whose period covers it, such as a month-end statement
-- and the quarter-end one around it; `settlement_no` names the booking
-- across statements. Nothing here is matched against the other rails:
-- like `portfolio_transactions`, these rows are a reading of one source,
-- and arbitration between rails is gold's.
--
-- Currencies, as the list prints them:
--   currency_iso            the trade's: the prices
--   reporting_currency_iso  the statement's: transaction value, cost value
--   settlement_currency_iso the cash account's: settlement amount
--   charges_currency_iso    the charges' own, as printed beside them
-- Signs are as printed. The list prints a sale's quantity and
-- transaction value, a purchase's settlement amount and the charges as
-- negative figures.
--
-- The rows hold values silver never parsed, so they fill when the
-- document pass re-derives the archive, on the next load.
-- ============================================================

CREATE TABLE statement_trades (
    source_doc_token          TEXT    NOT NULL,   -- FK to documents.doc_token
    seq                       INTEGER NOT NULL,   -- place in the list, from 1
    as_of_date                INTEGER NOT NULL,   -- the statement's, Unix seconds UTC
    portfolio_external_id     TEXT    NOT NULL,   -- PSN-aligned, as in historical_position_snapshots
    reporting_currency_iso    TEXT,
    period_start              INTEGER,            -- the list's own period
    period_end                INTEGER,
    trade_date                INTEGER,
    trade_time                TEXT,               -- 'HH:MM:SS', or the word printed instead
    value_date                INTEGER,
    -- What the bank calls the booking, its printed lines joined:
    -- 'Purchase Spot', 'Sale Spot', 'Incoming from spin-off'.
    booking_text              TEXT    NOT NULL,
    quantity                  REAL,
    security_name             TEXT,
    valor                     TEXT,
    isin                      TEXT,
    currency_iso              TEXT,
    -- A purchase's price, or the average cost of the holding a sale
    -- draws on, with the matching exchange rate: the purchase rate, or
    -- the average buy rate. The rate is printed only when the trade's
    -- currency differs from the reporting currency.
    cost_price                REAL,
    acquisition_fx_rate       REAL,
    cost_basis                REAL,               -- "Cost value", reporting currency
    transaction_price         REAL,               -- a sale's price
    transaction_fx_rate       REAL,
    transaction_gain_pct      REAL,               -- transaction price over cost price
    exchange_gain_pct         REAL,               -- transaction rate over average buy rate
    realized_pl_pct           REAL,               -- transaction value over cost value
    transaction_value         REAL,               -- reporting currency
    accrued_interest          REAL,
    settlement_amount         REAL,
    settlement_currency_iso   TEXT,
    -- The charges, one column per label the list prints.
    taxes                     REAL,               -- "Tax"
    fees                      REAL,               -- "Various"
    commission                REAL,               -- "Brokerage"
    stock_exchange_fees       REAL,               -- "Stock exchange"
    third_party_fees          REAL,               -- "Third-party executions"
    financial_transaction_tax REAL,               -- "Foreign Financial Transaction Tax"
    charges_currency_iso      TEXT,
    place_of_execution        TEXT,
    settlement_no             TEXT,
    order_no                  TEXT,
    custody_account           TEXT,               -- as printed, 'BBB-AAAAAAAA.XX'
    account_iban              TEXT,               -- the cash account, IBAN without spaces
    payload                   TEXT    NOT NULL,   -- every printed cell, by column and row
    PRIMARY KEY (source_doc_token, seq)
);

CREATE INDEX ix_statement_trades_isin       ON statement_trades(isin, trade_date);
CREATE INDEX ix_statement_trades_settlement ON statement_trades(settlement_no);


INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (14, CAST(strftime('%s','now') AS INTEGER));
