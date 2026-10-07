-- ============================================================
-- ubs-web silver schema, migration 0013 — the cost side of a
-- statement holding, and the advices that state a paid-in price.
--
-- A Statement-of-assets holding prints up to four lines (DESIGN.md
-- §3.8). Line 1 gives the cost price in the holding's currency. The
-- lines below it give what converts that price into the portfolio's
-- reporting currency and when the holding was last bought:
--
--   acquisition_fx_rate  the average buy exchange rate, from the
--                        holding's currency (`currency_iso`) to the
--                        portfolio's (`market_value_currency`). Printed
--                        only when the two differ.
--   current_fx_rate      the exchange rate at the as-of date, same pair.
--   cost_basis           the statement's "cost value": the units at
--                        their average cost, at the average buy rate, in
--                        `market_value_currency`.
--   last_purchase_date   Unix seconds UTC; NULL where none is printed.
--
-- `current_fx_rate` is the column 0002 named `exchange_rate_to_base`,
-- renamed so the two rates read as a pair. A cash line prints its one
-- exchange rate in that column and keeps it. A private-markets holding
-- prints a single figure where a listed one prints its prices; that
-- figure is the NAV per unit (units × figure = market value), so it
-- moves to `market_price` and `current_fx_rate` is left NULL.
--
-- The three new columns hold values silver never parsed, so they fill
-- when the document pass re-derives the archive. That happens on the
-- next load, since the parser generation has moved (migration 0009).
-- ============================================================

ALTER TABLE historical_position_snapshots
    RENAME COLUMN exchange_rate_to_base TO current_fx_rate;

UPDATE historical_position_snapshots
   SET market_price = current_fx_rate,
       current_fx_rate = NULL
 WHERE json_extract(payload, '$.kind') = 'private_market';

ALTER TABLE historical_position_snapshots ADD COLUMN acquisition_fx_rate REAL;
ALTER TABLE historical_position_snapshots ADD COLUMN cost_basis          REAL;
ALTER TABLE historical_position_snapshots ADD COLUMN last_purchase_date  INTEGER;


-- One row per securities advice that states what a holding was bought
-- for. Two kinds:
--
--   capital_call   a private-markets fund's call on its commitment. It
--                  states an amount and a value date but no units and
--                  no price: the units are issued later, at a NAV the
--                  statement of assets reports. For such units MT535
--                  states no book cost, so the calls are what was paid
--                  in.
--   contract_note  a purchase outside the exchange (a new issue, a fund
--                  subscription, or a prepayment towards one), with the
--                  units, the price and the charges.
--
-- Every figure is as printed, in `currency_iso` unless the column says
-- otherwise. A figure the document does not print is NULL.
CREATE TABLE advices (
    source_doc_token        TEXT    NOT NULL PRIMARY KEY, -- FK to documents.doc_token
    kind                    TEXT    NOT NULL,             -- 'capital_call' | 'contract_note'
    -- What the document calls the booking: a contract note's heading
    -- ('New issue purchase'), a call's title line.
    title                   TEXT,
    doc_date                INTEGER,                      -- the "Produced on" date
    trade_date              INTEGER,                      -- contract notes
    value_date              INTEGER,                      -- when the cash moves
    instrument_isin         TEXT,
    valor                   TEXT,                         -- contract notes that print one
    security_name           TEXT,                         -- contract notes
    currency_iso            TEXT,                         -- the trading currency
    quantity                REAL,
    price                   REAL,
    -- A contract note's market value; a call's called amount.
    amount                  REAL,
    -- A subscription settled against an earlier prepayment states the
    -- prepayment it deducts.
    prepayment              REAL,
    placement_fee           REAL,
    stamp_duty              REAL,
    -- The cash the booking takes from the account: a contract note's
    -- debit, a call's total payable (the called amount plus any
    -- equalisation interest).
    settlement_amount       REAL,
    settlement_currency_iso TEXT,
    -- The conversion rate a contract note states, with its pair as
    -- printed ('USD/CHF').
    fx_rate                 REAL,
    fx_rate_pair            TEXT,
    payload                 TEXT    NOT NULL              -- the printed lines each figure was read from
);

CREATE INDEX ix_advices_isin ON advices(instrument_isin);


INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (13, CAST(strftime('%s','now') AS INTEGER));
