-- A managed portfolio's own movements: every securities trade it made,
-- and the corporate actions booked against its holdings.
--
-- The `transactions` table above is the CASH surface — the accounts the
-- homepage files as cash tiles, on two rails (the CSV/MT940 export and,
-- below its floor, the Account Statement PDF). A managed portfolio is on
-- neither. Its movements are published on a separate list, and until now
-- reached silver only where an annual Account Statement happened to
-- reprint them — which is to say once a year, in the following January,
-- leaving the current year unreadable for as long as twelve months.
--
-- These rows are deliberately NOT in `transactions`, though a trade is a
-- transaction and the columns would mostly fit. Two things have to be
-- settled before one of these can be a ledger row, and neither is
-- answerable from this source alone:
--
--   1. WHICH ACCOUNT SETTLED IT. The export names the safekeeping
--      account the securities moved in and the currency the cash moved
--      in, but not the cash account that paid. A portfolio holds one
--      cash account per currency, so the pair determines it — via a
--      roster this collector does not hold (its own `accounts` rows
--      carry the relationship, not the numbered portfolio).
--   2. WHETHER ANOTHER RAIL ALREADY HAS IT. From the day the sibling
--      PSN feed's trade confirmations begin, the same trade arrives
--      there too, under an id this one cannot match. Both in the ledger
--      is the trade counted twice.
--
-- Both are cross-source questions, and cross-source arbitration is
-- gold's (DESIGN.md: sources merge only at gold). So the export is
-- recorded here in the source's own terms — safekeeping account,
-- portfolio, settlement currency, valor — and the adapter that resolves
-- those questions is what decides which of these rows becomes a ledger
-- row. A reader that joins this table to `transactions` today is
-- reading two rails and must expect the overlap.
CREATE TABLE portfolio_transactions (
    -- `ptx:` + the export's own "External reference" where it carries
    -- one (every actual trade does), else `ptx:` + a content hash of
    -- the booking. Corporate actions and FX legs are published without
    -- a reference, so the hash is what keeps a re-fetch idempotent for
    -- them; it is derived from the columns UBS fills for such a row.
    transaction_external_id  TEXT    NOT NULL,
    -- The custody account the securities moved in ("Product"), in the
    -- long form the PSN feed keys safekeeping accounts by, so the two
    -- feeds name the same account the same way.
    safekeeping_account_external_id TEXT NOT NULL,
    -- The numbered portfolio ("Portfolio"). The piece this collector's
    -- own account rows lack, and half of what names the settling cash
    -- account.
    portfolio_external_id    TEXT    NOT NULL,
    snapshot_at              INTEGER NOT NULL,   -- dump that captured it
    trade_date               INTEGER,            -- when it was executed
    booking_date             INTEGER,
    value_date               INTEGER NOT NULL,   -- when the cash settled
    -- What the bank called it ("Description 1"): "Stock Market Spot
    -- Purchase", "Sale from issue", "Incoming Rights", an FX leg. The
    -- kind is classified from this, so it is stored verbatim rather
    -- than mapped here.
    booking_type             TEXT    NOT NULL,
    security_name            TEXT,               -- "Description 2"; a label
    -- The Swiss valor identifies the security; the ISIN is carried
    -- beside it where the export states one. A name is neither.
    valor                    TEXT,
    isin                     TEXT,
    -- Signed the export's way: negative is leaving the portfolio.
    quantity                 REAL,
    -- The currency the cash leg settled in — with the portfolio, what
    -- names the cash account that paid.
    settlement_currency_iso  TEXT,
    trans_price              REAL,
    exchange_rate            REAL,
    -- The currency the export was valued in, and the value in it. Also
    -- signed the export's way: positive is cash leaving.
    valuation_currency_iso   TEXT,
    trans_value              REAL,
    accrued_interest         REAL,
    realized_pl              REAL,
    order_no                 TEXT,
    external_reference       TEXT,               -- NULL on an unreferenced row
    asset_class              TEXT,
    sub_asset_class          TEXT,
    instrument_category      TEXT,
    payload                  TEXT    NOT NULL,   -- the export row verbatim
    PRIMARY KEY (transaction_external_id, safekeeping_account_external_id)
);

-- One portfolio over a window is how both the gap analysis and the
-- eventual adapter read this.
CREATE INDEX ix_portfolio_transactions_acct_value_date
    ON portfolio_transactions(safekeeping_account_external_id, value_date);
CREATE INDEX ix_portfolio_transactions_portfolio
    ON portfolio_transactions(portfolio_external_id, value_date);


INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (12, CAST(strftime('%s','now') AS INTEGER));
