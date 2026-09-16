-- The two tables the cash flow statement's boundary is stamped into,
-- and the pool macro that reads them.
--
-- SQL has to know which accounts are the household's, and the answer
-- is half engine and half configuration: an engine default per tax
-- wrapper, which a deployment may move per wrapper. Neither half is
-- reachable from a macro, so the RESOLVED answer is stamped into gold
-- by every enrichment pass, on the `spend_account_scope` precedent —
-- config in, table out, removal removes. The alternative, injecting
-- config into every query, would mean the dashboard and the CLI could
-- disagree about whose money moved.
--
--   cashflow_wrapper_sides   every tax wrapper, with the side of the
--                            boundary it sits on and — on the vehicle
--                            side alone — which pool. Stamped WHOLE,
--                            defaults included, so one place decides a
--                            wrapper's side and no macro re-derives it.
--   cashflow_account_scope   which accounts the pool holds, as
--                            overrides of an include-everything
--                            default. The other two families' scope
--                            tables' shape, so the three read alike.
--
-- The scope table keeps the `mode` column its two siblings have even
-- though only 'exclude' is ever stamped: the pool's default is every
-- account, so an include could fence nothing, and a third shape here
-- would buy a reader nothing but a difference to explain. The CHECK
-- admits both because the table's shape is the contract, not what one
-- config happens to write into it.
--
-- The SCOPE INHERITS NOTHING. An account spending or income excludes
-- is not thereby outside the household's cash pool: cashflow reads
-- each family's resolved verdict rather than its base, so their scopes
-- and base exclusions do not reach inside the pool. The shape to hold
-- in mind is an income scope dropping an account whose funding leg
-- cannot pair — inherited here, it would put that account's
-- withdrawals in the pool while its deposits vanished.
--
-- IF NOT EXISTS / OR REPLACE throughout keep this replayable for the
-- DDL-rerun test.

CREATE TABLE IF NOT EXISTS cashflow_wrapper_sides (
    tax_wrapper TEXT NOT NULL PRIMARY KEY,
    side        TEXT NOT NULL CHECK (side IN ('household', 'vehicle', 'giving')),
    -- NULL unless side = 'vehicle': only an earmarked pool has one to
    -- name. `untracked` is deliberately not admitted — no wrapper
    -- sends a crossing there, only the ABSENCE of a far account does.
    class       TEXT CHECK (class IS NULL OR class IN (
        'retirement', 'education', 'health', 'trusts'
    )),
    CHECK ((side = 'vehicle') = (class IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS cashflow_account_scope (
    silver_source_id    TEXT NOT NULL,
    account_external_id TEXT NOT NULL,
    mode                TEXT NOT NULL CHECK (mode IN ('include', 'exclude')),
    PRIMARY KEY (silver_source_id, account_external_id)
);

-- cashflow_pool_accounts: the household's cash pool — every scoped
-- account whose tax wrapper is on the household side.
--
-- An UNSET wrapper reads as household, which is the render-time
-- default gold applies everywhere else and the safe direction: nothing
-- is silently removed from the statement. It is not free, and nothing
-- in the reconciliation can see the cost — an adapter that leaves the
-- column unset puts a retirement or health account inside the pool,
-- where its own trades become household investing and its
-- contributions become invisible, and the crossing is ABSENT rather
-- than wrong. `wealthdb status -v` counts pooled accounts with no
-- wrapper for exactly that reason.
--
-- The join is LEFT and the COALESCE is on the SIDE rather than on the
-- wrapper, so an account carrying a wrapper the stamped table does not
-- hold — a database migrated ahead of its binary — reads as household
-- too, by the same rule and not by accident.
CREATE OR REPLACE MACRO cashflow_pool_accounts() AS TABLE (
    SELECT a.silver_source_id, a.account_external_id, a.account_kind,
           a.display_name, a.nickname, a.account_category,
           a.portfolio_external_id, a.tax_wrapper
      FROM accounts a
      LEFT JOIN cashflow_account_scope s
             ON s.silver_source_id    = a.silver_source_id
            AND s.account_external_id = a.account_external_id
      LEFT JOIN cashflow_wrapper_sides w
             ON w.tax_wrapper = a.tax_wrapper
     WHERE COALESCE(s.mode, 'include') = 'include'
       AND COALESCE(w.side, 'household') = 'household'
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (80, CAST(epoch(now()) AS BIGINT));
