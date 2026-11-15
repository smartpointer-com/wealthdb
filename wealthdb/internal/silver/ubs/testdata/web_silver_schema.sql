-- The ubs-web silver schema the collector's migrations
-- (collectors/ubs-web/migrations) build, for the web feed's fixture tests.

CREATE TABLE schema_meta (
    silver_schema_version INTEGER NOT NULL PRIMARY KEY,
    applied_at            INTEGER NOT NULL                -- Unix seconds UTC
);
CREATE TABLE banking_relationships (
    snapshot_at              INTEGER NOT NULL,
    banking_relationship_id  TEXT    NOT NULL,            -- UBS opaque token
    account_number_prefix    TEXT,                        -- e.g. 'BBBB AAAAAAAA'
    description              TEXT,                        -- optional human label
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, banking_relationship_id)
);
CREATE TABLE portfolios (
    snapshot_at              INTEGER NOT NULL,
    portfolio_external_id    TEXT    NOT NULL,            -- e.g. 'RNNN'
    banking_relationship_id  TEXT,
    portfolio_full_id        TEXT,                        -- e.g. 'BBBB AAAAAAAA RNNN'
    portfolio_uid            TEXT,                        -- UBS opaque token
    base_currency            TEXT,                        -- e.g. 'CHF', from "Valued in:" footer
    description              TEXT,                        -- human label if discoverable
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, portfolio_external_id)
);
CREATE TABLE accounts (
    snapshot_at              INTEGER NOT NULL,
    account_external_id      TEXT    NOT NULL,            -- IBAN no-spaces upper
    kind                     TEXT    NOT NULL
        CHECK (kind IN ('cash', 'safekeeping')),
    iban                     TEXT,                        -- formatted IBAN with spaces
    account_acct_id_psn_form TEXT,                        -- 21-char PSN-style AcctId
    account_number_raw       TEXT,                        -- 'BBBB AAAAAAAA.40' form
    account_opaque_id        TEXT,                        -- UBS URL-resolvable token
    banking_relationship_id  TEXT,
    portfolio_external_id    TEXT,                        -- which portfolio it sits under
    currency_iso             TEXT,
    description              TEXT,                        -- 'CHF Checking, UBS Personal Account, …'
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);
CREATE TABLE positions (
    snapshot_at              INTEGER NOT NULL,
    portfolio_external_id    TEXT    NOT NULL,
    account_external_id      TEXT    NOT NULL,            -- IBAN for cash; '' for securities-only rows
    instrument_isin          TEXT,                        -- NULL for cash positions
    valor                    TEXT,
    currency_iso             TEXT    NOT NULL,            -- INSTRUMENT currency
    units                    REAL,                        -- "Number/Amt." column
    market_value             REAL,                        -- in `market_value_currency`
    market_value_currency    TEXT,                        -- portfolio base ccy from positions.csv footer
    cost_price               REAL,                        -- per unit, instrument currency
    accrued_interest         REAL,
    lending_value            REAL,
    lending_value_ratio      REAL,
    description              TEXT,                        -- e.g. 'Shs Example Materials AG'
    payload                  TEXT    NOT NULL,            -- full positions.csv row
    PRIMARY KEY (snapshot_at, portfolio_external_id,
                 account_external_id, instrument_isin)
);
CREATE TABLE transactions (
    transaction_external_id  TEXT    NOT NULL,             -- UBS Transaction no.
    account_external_id      TEXT    NOT NULL,             -- joins accounts.account_external_id
    snapshot_at              INTEGER NOT NULL,             -- which dump first captured this row
    trade_date               INTEGER,                      -- Unix seconds UTC; CSV "Trade date"
    booking_date             INTEGER,                      -- CSV "Booking date"
    value_date               INTEGER NOT NULL,             -- CSV "Value date" — gold splice key
    currency_iso             TEXT    NOT NULL,
    amount_debit             REAL,
    amount_credit            REAL,
    counterparty             TEXT,                         -- extracted from description1
    description_kind         TEXT,                         -- "Dividend", "e-banking payment order", etc.
    payload                  TEXT    NOT NULL,             -- all four description columns + footnotes
    PRIMARY KEY (transaction_external_id, account_external_id)
);
CREATE INDEX ix_transactions_account_value_date
    ON transactions(account_external_id, value_date);
CREATE TABLE documents (
    doc_token                TEXT    NOT NULL PRIMARY KEY, -- UBS API token
    content_sha256           TEXT    NOT NULL UNIQUE,
    file_path                TEXT    NOT NULL,             -- relative to bronze-root
    size_bytes               INTEGER NOT NULL,
    snapshot_at              INTEGER NOT NULL,             -- which dump first captured the file
    -- Best-effort parse from the listing-row label:
    doc_type                 TEXT,                         -- 'Account Statement', 'Tax Report', ...
    doc_date                 INTEGER,                      -- Unix seconds UTC
    account_external_id      TEXT,                         -- when discoverable
    portfolio_external_id    TEXT,                         -- when discoverable
    label                    TEXT    NOT NULL              -- raw label text
);
CREATE INDEX ix_documents_doc_date ON documents(doc_date);
CREATE INDEX ix_documents_doc_type ON documents(doc_type);
CREATE INDEX ix_documents_account  ON documents(account_external_id);
CREATE TABLE historical_position_snapshots (
    as_of_date               INTEGER NOT NULL,             -- Unix seconds UTC, end-of-period
    portfolio_external_id    TEXT    NOT NULL,             -- 'BBBBAAAAAAAANN01' etc.
    account_external_id      TEXT    NOT NULL,             -- IBAN no-spaces for cash; '' for securities
    instrument_isin          TEXT,                         -- NULL for cash
    currency_iso             TEXT,                         -- instrument currency
    units                    REAL,                         -- shares, units, or currency amount
    market_value             REAL,                         -- in `market_value_currency`
    market_value_currency    TEXT,                         -- portfolio base currency
    cost_price               REAL,                         -- per-unit cost basis
    market_price             REAL,                         -- per-unit market price
    accrued_interest         REAL,
    current_fx_rate    REAL,                         -- FX rate for non-base-ccy positions
    description              TEXT,                         -- e.g. 'Reg.shs Example Equity AG (XMPL)'
    sector                   TEXT,
    source_doc_token         TEXT    NOT NULL,             -- FK to documents.doc_token
    payload                  TEXT    NOT NULL, acquisition_fx_rate REAL, cost_basis          REAL, last_purchase_date  INTEGER, nav_date TEXT,             -- raw parsed text block
    PRIMARY KEY (as_of_date, portfolio_external_id,
                 account_external_id, instrument_isin)
);
CREATE INDEX ix_hist_pos_date         ON historical_position_snapshots(as_of_date);
CREATE INDEX ix_hist_pos_isin         ON historical_position_snapshots(instrument_isin);
CREATE INDEX ix_hist_pos_account      ON historical_position_snapshots(account_external_id);
CREATE TABLE historical_cash_balances (
    period_end               INTEGER NOT NULL,             -- Unix seconds UTC, last day of period
    account_external_id      TEXT    NOT NULL,             -- IBAN no-spaces uppercase
    currency_iso             TEXT    NOT NULL,
    period_start             INTEGER NOT NULL,             -- Unix seconds UTC, first day of period
    opening_balance          REAL,
    closing_balance          REAL,
    total_debits             REAL,
    total_credits            REAL,
    source_doc_token         TEXT    NOT NULL,
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (period_end, account_external_id)
);
CREATE INDEX ix_hist_cash_account_period
    ON historical_cash_balances(account_external_id, period_end);
CREATE TABLE mortgages (
    snapshot_at              INTEGER NOT NULL,
    account_external_id      TEXT    NOT NULL,        -- UBS-internal mortgage no.
    banking_relationship_id  TEXT,
    portfolio_external_id    TEXT,
    currency_iso             TEXT    NOT NULL,
    outstanding_balance      REAL,                    -- negative (liability)
    start_date               INTEGER,                 -- Unix seconds UTC (term start)
    end_date                 INTEGER,                 -- Unix seconds UTC (term end)
    rate_type                TEXT,                    -- 'fixed' | 'variable' | NULL
    collateral_description   TEXT,
    description              TEXT,
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);
CREATE INDEX ix_mortgages_account ON mortgages(account_external_id);
CREATE TABLE historical_mortgages (
    as_of_date              INTEGER NOT NULL,        -- Unix seconds UTC
    account_external_id     TEXT    NOT NULL,        -- UBS mortgage no.
    currency_iso            TEXT    NOT NULL,
    outstanding_balance     REAL,                    -- negative (liability)
    product_name            TEXT,                    -- 'UBS SARON Mortgage', …
    rate_type               TEXT,                    -- 'fixed'|'variable'|NULL (SARON maps to 'variable')
    collateral_description  TEXT,
    source_doc_token        TEXT    NOT NULL,
    payload                 TEXT    NOT NULL,
    PRIMARY KEY (as_of_date, account_external_id)
);
CREATE INDEX ix_hist_mortgages_account
    ON historical_mortgages(account_external_id);
CREATE TABLE IF NOT EXISTS "dump_runs" (
    snapshot_at           INTEGER NOT NULL PRIMARY KEY,
    silver_schema_version INTEGER NOT NULL,
    run_dir               TEXT    NOT NULL,
    window_since          INTEGER,
    window_until          INTEGER
);
CREATE TABLE card_accounts (
    snapshot_at              INTEGER NOT NULL,   -- Unix seconds UTC
    account_external_id      TEXT    NOT NULL,   -- UBS opaque card-account id
    account_number           TEXT,               -- the printed account number
    currency_iso             TEXT,
    -- Balance as UBS reports it: negative while the card owes. NULL when
    -- the roster did not carry one for this account.
    balance                  REAL,
    available                REAL,               -- remaining spending power
    credit_limit             REAL,
    -- Authorised-but-unposted activity, from the roster: each card's
    -- `balanceIncludingReserved` less its `balance`. NOT summed from
    -- the ledger's RESERVED rows — those carry no id and cannot be
    -- stored individually (see card_transactions). This is the whole
    -- of what silver keeps about them, and it is the gap between a
    -- balance reconstructed from booked rows and the one the card
    -- reports.
    reserved_amount          REAL,
    reserved_count           INTEGER,
    product_name             TEXT,               -- e.g. the card product line
    card_type                TEXT,               -- UBS internal type code
    account_status           TEXT,
    structure_type           TEXT,               -- e.g. a top-level account
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (snapshot_at, account_external_id)
);
CREATE TABLE card_transactions (
    transaction_external_id  TEXT    NOT NULL PRIMARY KEY,  -- minted `card:<hash>`
    account_external_id      TEXT    NOT NULL,   -- joins card_accounts
    snapshot_at              INTEGER NOT NULL,   -- dump that first captured it
    -- Two dates, both promoted: the purchase and the booking. A window
    -- fetch filters on the booking date (cards.py), so that is the one
    -- comparable with an invoice period.
    transaction_date         INTEGER,            -- purchase, Unix seconds UTC
    value_date               INTEGER NOT NULL,   -- booking, Unix seconds UTC
    -- Posted amount in the account's currency, signed UBS's way.
    amount                   REAL    NOT NULL,
    currency_iso             TEXT    NOT NULL,
    -- The amount as charged, before conversion. Equal to `amount` on a
    -- domestic row; the FX columns are populated only on a converted one.
    original_amount          REAL,
    original_currency_iso    TEXT,
    exchange_rate            REAL,
    -- The merchant descriptor as the terminal supplied it. This is the
    -- payee, and gold builds its merchant signature from it.
    merchant                 TEXT,
    -- The merchant CATEGORY in words — an ISO 18245 MCC description.
    -- Despite the API spelling it `merchantName`, it names a line of
    -- business, never a merchant. Free text, not an enum.
    merchant_category        TEXT,
    -- UBS's own coarser grouping over those descriptions.
    merchant_group_code      TEXT,
    card_number              TEXT,               -- which card booked it
    settled_in_invoice       INTEGER,            -- 1 once billed
    -- No foreign key to card_accounts: that table is a snapshot series,
    -- so `account_external_id` repeats there and cannot be a parent key.
    -- `transactions` relates to `accounts` the same way, for the same
    -- reason.
    payload                  TEXT    NOT NULL
);
CREATE INDEX ix_card_transactions_account_value_date
    ON card_transactions(account_external_id, value_date);
CREATE TABLE card_invoices (
    account_external_id      TEXT    NOT NULL,
    period_end               INTEGER NOT NULL,   -- Unix seconds UTC
    period_start             INTEGER NOT NULL,
    invoice_external_id      TEXT    NOT NULL,
    snapshot_at              INTEGER NOT NULL,
    invoicing_date           INTEGER,
    -- The day the cash account is debited for this bill. NULL on the
    -- open period, which has not been billed yet. This is the date gold's
    -- internal-transfer matcher pairs the cash-side debit against.
    debiting_date            INTEGER,
    due_on                   INTEGER,
    -- Closing figure for the period, signed UBS's way (negative = owed).
    due_amount               REAL,
    minimal_due_amount       REAL,
    currency_iso             TEXT,
    -- Opening balance and the period's turnover. NULL until the
    -- per-invoice detail has been captured — the period list alone does
    -- not carry them.
    balance_forward          REAL,
    total_debit              REAL,
    total_credit             REAL,
    -- 1 when the four figures above satisfy the identity, 0 when they do
    -- not, NULL when the detail is missing and there was nothing to check.
    reconciles               INTEGER,
    statement_type           TEXT,               -- a closed period vs the open one
    payment_method           TEXT,               -- the settlement rail
    invoice_status           TEXT,
    -- Whether this period's transactions are in `card_transactions`.
    -- A function of which ledger windows have been loaded, so the loader
    -- recomputes it on every load rather than keeping the first answer;
    -- 0 is the safe default for a period no load has covered yet.
    transactions_covered     INTEGER NOT NULL DEFAULT 0,
    payload                  TEXT    NOT NULL,
    PRIMARY KEY (account_external_id, period_end)
);
CREATE INDEX ix_card_invoices_debiting_date
    ON card_invoices(debiting_date);
CREATE TABLE card_statements (
    content_sha256           TEXT    NOT NULL PRIMARY KEY,
    account_external_id      TEXT    NOT NULL,
    invoice_external_id      TEXT    NOT NULL,
    period_end               INTEGER NOT NULL,
    file_path                TEXT    NOT NULL,   -- absolute path on disk at load time
    size_bytes               INTEGER NOT NULL,
    snapshot_at              INTEGER NOT NULL
);
CREATE INDEX ix_card_statements_account ON card_statements(account_external_id);
CREATE TABLE parser_generations (
    scope      TEXT    NOT NULL PRIMARY KEY,  -- the pass, e.g. 'documents'
    generation TEXT    NOT NULL,              -- collectorkit.srcfp fingerprint
    stamped_at INTEGER NOT NULL               -- Unix seconds UTC
);
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
CREATE INDEX ix_portfolio_transactions_acct_value_date
    ON portfolio_transactions(safekeeping_account_external_id, value_date);
CREATE INDEX ix_portfolio_transactions_portfolio
    ON portfolio_transactions(portfolio_external_id, value_date);
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
CREATE TABLE account_rates (
    snapshot_at          INTEGER NOT NULL,   -- Unix seconds UTC
    account_external_id  TEXT    NOT NULL,   -- IBAN no-spaces upper
    side                 TEXT    NOT NULL
        CHECK (side IN ('credit', 'debit')),
    tier                 INTEGER NOT NULL CHECK (tier >= 1),
    rate_pct             REAL    NOT NULL,
    upper_limit          REAL,
    valid_from           TEXT,
    surface              TEXT    NOT NULL
        CHECK (surface IN ('api', 'legacy')),
    payload              TEXT    NOT NULL,   -- the condition as read
    PRIMARY KEY (snapshot_at, account_external_id, side, tier)
);
CREATE TABLE private_market_reports (
    source_doc_token     TEXT     NOT NULL PRIMARY KEY, -- FK to documents.doc_token
    instrument_isin      TEXT     NOT NULL,             -- the feeder fund
    fund_name            TEXT,                          -- as the capital account names it
    as_of                INTEGER  NOT NULL,
    lines_as_of          INTEGER,
    currency_iso         TEXT,
    commitment           REAL,                          -- the investor's net commitment
    capital_called       REAL,
    cumulative_expenses  REAL,                          -- negative, as printed in parentheses
    valuation            REAL,                          -- the investor's NAV
    shares_in_issue      REAL,
    nav_per_share        REAL,
    line_count           INTEGER  NOT NULL,
    lines_total          REAL,
    lines_reconciled     INTEGER  NOT NULL,
    payload              TEXT     NOT NULL              -- the printed lines each figure was read from
);
CREATE INDEX ix_private_market_reports_isin
    ON private_market_reports(instrument_isin, as_of);
CREATE TABLE private_market_report_lines (
    source_doc_token  TEXT     NOT NULL,                -- FK to private_market_reports
    line_no           INTEGER  NOT NULL,                -- order in the table, from 1
    name              TEXT     NOT NULL,
    region            TEXT     NOT NULL,
    sector            TEXT,
    strategy          TEXT,
    cost              REAL,
    residual_value    REAL,
    payload           TEXT     NOT NULL,                -- the printed line
    PRIMARY KEY (source_doc_token, line_no)
);
