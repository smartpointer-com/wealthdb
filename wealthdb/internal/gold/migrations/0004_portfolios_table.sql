-- ============================================================
-- gold schema, migration 0004 — portfolios as their own entity.
--
-- Portfolios are *not* accounts. A portfolio (UBS-specific today)
-- is the wealth-management wrapper that GROUPS one or more cash
-- and safekeeping accounts under a single mandate (personal,
-- managed, advisory, ...). It does not hold cash or securities
-- directly; its component accounts do. The previous schema kept
-- portfolios in `accounts` with account_kind='portfolio' and
-- linked children via parent_account_external_id, which double-
-- counted when summing the column.
--
-- New shape:
--
--   portfolios          one row per bank-defined portfolio.
--   accounts            cash / safekeeping / brokerage / etc.
--                       carry a `portfolio_external_id` pointing
--                       at the portfolio they belong to (NULL
--                       when the bank doesn't group them, e.g.
--                       Schwab, Swissquote).
--
-- The `wealthdb holdings portfolios` subcommand aggregates each portfolio's
-- value as the sum of its component accounts' values. Schwab and
-- Swissquote accounts (no portfolio) and UBS accounts the bank
-- didn't group still appear under a sentinel NULL portfolio per
-- silver_source so the totals tie out against `wealthdb holdings accounts`
-- and `wealthdb holdings positions --with-cash`.
--
-- The old `parent_account_external_id` column is dropped; the old
-- `account_kind='portfolio'` rows are deleted (the next load
-- re-populates the proper portfolios table). Forward contracts,
-- money-market contracts, and OTC contracts previously stored
-- against the portfolio_external_id as if it were an account are
-- now attributed to a synthetic per-portfolio overlay account
-- (account_kind='overlay') so every position remains owned by an
-- account row.
-- ============================================================

CREATE TABLE portfolios (
    silver_source_id        TEXT    NOT NULL,
    portfolio_external_id   TEXT    NOT NULL,
    display_name            TEXT,
    base_currency           TEXT,
    relationship_id         TEXT,
    nickname                TEXT,
    first_seen_at           BIGINT  NOT NULL,
    last_seen_at            BIGINT  NOT NULL,
    payload                 JSON,
    PRIMARY KEY (silver_source_id, portfolio_external_id)
);

ALTER TABLE accounts DROP COLUMN parent_account_external_id;
ALTER TABLE accounts ADD COLUMN portfolio_external_id TEXT;

-- Old rows where portfolios masqueraded as accounts. The next
-- `wealthdb load` re-populates portfolios in their own table.
DELETE FROM accounts WHERE account_kind = 'portfolio';

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (4, CAST(epoch(now()) AS BIGINT));
