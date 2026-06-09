-- angellist silver v6 — the funding-account cash ledger.
--
-- The funding-accounts page (InvestmentEntityQuery.investmentEntity) exposes
-- the actual DATED cash ledger plus the current cash balance — the real
-- source for dated cash flows that the GraphQL positions / activity feed /
-- K-1s lack. Transaction `type` is one of: deposit / withdrawal (external
-- bank ↔ funding account), investment / refund (capital ↔ an SPV; a refund
-- returns an oversubscribed commitment), disbursement (a deal pays out).
-- `amount` is SIGNED (+ money in, − money out) and the ledger reconciles
-- exactly to the balance, so the K-1 Line 19(a) annual distribution is now
-- redundant and is no longer emitted as a transaction (k1_capital_accounts
-- is kept only for the position tax-basis statement valuations).

CREATE TABLE funding_accounts (
    funding_account_external_id TEXT PRIMARY KEY,  -- AngelList investment entity id
    slug_name      TEXT,
    legal_name     TEXT,                            -- PII (kept in silver only)
    currency       TEXT,
    balance_minor  INTEGER,                          -- current cash balance (minor units)
    snapshot_at    INTEGER,                          -- download run that observed it
    payload        TEXT
);

CREATE TABLE funding_transactions (
    transaction_external_id     TEXT PRIMARY KEY,    -- AngelList transaction id
    funding_account_external_id TEXT,
    occurred_at        INTEGER NOT NULL,             -- transaction date (unix seconds UTC)
    type               TEXT,                         -- raw AngelList type (see header)
    amount_minor       INTEGER,                      -- SIGNED minor units (+ in / − out)
    currency           TEXT,
    running_balance_minor INTEGER,                   -- balance after this tx (provenance)
    syndicate_name     TEXT,                         -- SPV / fund (investment/refund); PII; nullable
    description        TEXT,                         -- source label (bank / deal); PII; nullable
    snapshot_at        INTEGER,                      -- download run that last saw it
    payload            TEXT
);
CREATE INDEX idx_funding_tx_date ON funding_transactions(occurred_at);

INSERT INTO schema_meta (silver_schema_version) VALUES (6);
