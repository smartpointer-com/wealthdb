-- Spending enrichment overlay: the tables that carry a spend category
-- and the layered macros that define which transactions are spend at
-- all.
--
-- Three tables, deliberately keyed differently from each other:
--
--   spend_txn_enrichment    per-transaction, source-scoped, rewritten
--                           by every enrichment pass. Holds the
--                           merchant signature computed for the row
--                           and, when a rule / matcher / provider hint
--                           could place it, the category. Because it
--                           is derived it is cheap to throw away —
--                           `reset <source>` clears it.
--   spend_merchant_categories
--                           GLOBAL, keyed by merchant signature alone
--                           and NOT by source. A merchant is the same
--                           merchant whichever card met it, so a
--                           verdict paid for once should not be bought
--                           again per source. This is a deliberate
--                           departure from symbol_resolutions, which
--                           is per-source because a symbol is only
--                           meaningful inside its source's id space.
--                           It survives `reset`, and `reload -a`
--                           carries it across the fresh-file swap.
--   spend_account_scope     configuration stamped into gold, on the
--                           SetFxPriorities precedent (migration
--                           0019): which accounts spending counts,
--                           expressed as overrides of the account-kind
--                           default so the report macros need no
--                           runtime config injection.
--
-- signature_version appears on both overlay tables. A signature is a
-- normalisation of a counterparty string, and normalisation rules
-- change; stamping the version that produced a row means a bump can
-- re-key the paid merchant verdicts forward instead of orphaning them
-- behind signatures nothing will ever compute again.
--
-- provenance is the tier that placed the verdict. matcher, rule,
-- provider and signature-only are derived from gold by the enrichment
-- pass (internal/spending/enrich.go); 'manual' is the pins ledger's
-- (internal/spending/pins.go), re-stamped by every pass exactly like
-- the others. The model tier writes no row here at all — its verdicts
-- are keyed by merchant signature, and spend_txn_categories() resolves
-- them as 'model' where the two scopes meet (migration 0050). The
-- CHECK is closed, and DuckDB cannot widen one in place (the
-- rename-recreate dance of 0008 / 0017 / 0036 / 0037), so admitting a
-- further tier later costs a table rewrite.
--
-- spend_detailed carries no FK to spend_categories (gold's fact tables
-- carry no FKs) and is nullable on the enrichment table: a row the
-- pass could reach but not categorise is recorded with its signature
-- and provenance 'signature-only', which is what lets the model tier
-- find its work.
--
-- IF NOT EXISTS / OR REPLACE throughout keep this replayable for the
-- DDL-rerun test (see gold.Migrate's REPLAY note).

CREATE TABLE IF NOT EXISTS spend_txn_enrichment (
    silver_source_id        TEXT   NOT NULL,
    transaction_external_id TEXT   NOT NULL,
    merchant_signature      TEXT,
    signature_version       INTEGER NOT NULL,
    spend_detailed          TEXT,
    provenance              TEXT   NOT NULL CHECK (provenance IN (
        'matcher', 'rule', 'provider', 'signature-only', 'manual'
    )),
    assigned_at             BIGINT NOT NULL,   -- Unix seconds UTC
    PRIMARY KEY (silver_source_id, transaction_external_id)
);

CREATE TABLE IF NOT EXISTS spend_merchant_categories (
    merchant_signature TEXT   NOT NULL PRIMARY KEY,
    merchant_name      TEXT   NOT NULL,
    spend_detailed     TEXT   NOT NULL,
    signature_version  INTEGER NOT NULL,
    assigned_at        BIGINT NOT NULL,        -- Unix seconds UTC
    model_name         TEXT   NOT NULL         -- copy of cfg.model.name at verdict time
);

CREATE TABLE IF NOT EXISTS spend_account_scope (
    silver_source_id    TEXT NOT NULL,
    account_external_id TEXT NOT NULL,
    mode                TEXT NOT NULL CHECK (mode IN ('include', 'exclude')),
    PRIMARY KEY (silver_source_id, account_external_id)
);

-- The signature is the join key from a transaction to the global
-- merchant store, so it is looked up far more often than the primary
-- key it hangs off.
CREATE INDEX IF NOT EXISTS ix_spend_txn_enrichment_signature
    ON spend_txn_enrichment(merchant_signature);

-- ============================================================
-- The spending populations, layered.
--
-- Four macros, each one narrower than the one it reads, so every
-- population has exactly ONE definition and no Go-side predicate
-- restates it:
--
--   spend_scoped_accounts()             which accounts count at all
--   spend_enrichment_population(f, t)   what the enrichment pass sees
--   spend_matcher_pool(f, t)            what the transfer matcher sees
--   spending_lines_base(f, t)           what a report charts
--
-- The layering is not decoration. The enrichment pass is the thing
-- that DECIDES which rows are own-account moves, so it cannot read a
-- population that has already dropped them — that would be circular.
-- And the matcher needs a pool broader still: to pair a card payment
-- with the withdrawal that funded it, it has to see the income-side
-- `deposit` / `transfer_in` rows the spending base deliberately
-- excludes.
-- ============================================================

-- spend_scoped_accounts: the account set spending counts, in one
-- place. Kind 'cash' or 'card' by default, overridden EITHER WAY by a
-- spend_account_scope row — 'include' pulls in an account of another
-- kind, 'exclude' drops one of these.
CREATE OR REPLACE MACRO spend_scoped_accounts() AS TABLE (
    SELECT a.silver_source_id, a.account_external_id, a.account_kind,
           a.display_name, a.nickname, a.account_category
      FROM accounts a
      LEFT JOIN spend_account_scope s
             ON s.silver_source_id    = a.silver_source_id
            AND s.account_external_id = a.account_external_id
     WHERE COALESCE(s.mode,
                    CASE WHEN a.account_kind IN ('cash', 'card')
                         THEN 'include' ELSE 'exclude' END) = 'include'
);

-- spend_enrichment_population: every transaction the enrichment pass
-- may write a verdict for — the scoped accounts crossed with the
-- spending kind set, and NOTHING else. No category resolution, no
-- internal-transfer exclusion: the pass is what decides those.
--
-- Kind rules, each a decision rather than a detail:
--
--   * purchase / refund / reward / withdrawal / fee / tax always,
--     plus `interest` ONLY when the net amount is negative — a card's
--     finance charge is spend, credited interest is income.
--   * `deposit` and positive `interest` are excluded: they are the
--     income side, which belongs to a future cashflow feature.
--   * `other` is EXCLUDED. The UBS adapter deliberately demotes
--     internal conduit legs to `other` to keep them out of analytics,
--     and the kind carries no reliable sign, so admitting it would
--     import noise with no way to orient it. Rows on an in-scope
--     account carrying either CATCH-ALL kind — `other` or `journal` —
--     are counted by `wealthdb status -v` as excluded_unmapped, so a
--     class of rows an adapter could not classify is loud rather than
--     silently uncounted. The deliberate exclusions above are not
--     counted: they occur in bulk and would drown that signal.
--
-- net_amount keeps its canonical sign (spend negative, refunds and
-- rewards positive); currency conversion belongs to the report macros
-- that wrap these.
CREATE OR REPLACE MACRO spend_enrichment_population(p_from, p_to) AS TABLE (
    SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
           t.account_external_id, sa.account_kind, sa.display_name,
           sa.nickname, sa.account_category,
           t.kind, t.currency, t.net_amount, t.description,
           t.counterparty, t.provider_category
      FROM transactions t
      JOIN spend_scoped_accounts() sa
             ON sa.silver_source_id    = t.silver_source_id
            AND sa.account_external_id = t.account_external_id
     WHERE t.occurred_at BETWEEN p_from AND p_to
       AND (t.kind IN ('purchase', 'refund', 'reward', 'withdrawal', 'fee', 'tax')
            OR (t.kind = 'interest' AND t.net_amount < 0))
);

-- spend_matcher_pool: the legs offered to the internal-transfer
-- matcher — the scoped accounts crossed with the transfer-eligible
-- kinds. It is deliberately NOT a subset of the enrichment
-- population: `deposit` and `transfer_in` are income-side rows that
-- never count as spend, but a card payment's funding withdrawal is
-- only recognisable as internal because its counter-leg is one of
-- them. A pool narrowed to the spending kinds would leave every
-- own-account move one-legged and therefore indistinguishable from
-- real spending.
CREATE OR REPLACE MACRO spend_matcher_pool(p_from, p_to) AS TABLE (
    SELECT t.silver_source_id, t.transaction_external_id, t.occurred_at,
           t.account_external_id, t.kind, t.currency, t.net_amount,
           t.description, t.counterparty
      FROM transactions t
      JOIN spend_scoped_accounts() sa
             ON sa.silver_source_id    = t.silver_source_id
            AND sa.account_external_id = t.account_external_id
     WHERE t.occurred_at BETWEEN p_from AND p_to
       AND t.kind IN ('deposit', 'withdrawal', 'card_payment',
                      'transfer_in', 'transfer_out')
);

-- spending_lines_base: what a report charts — the enrichment
-- population with its category resolved and its own-account moves
-- removed.
--
--   * Resolved category is COALESCE(transaction scope, merchant
--     scope): a per-transaction verdict beats the merchant-wide one.
--     The merchant store is reached THROUGH the enrichment row's
--     signature, which is also why the pass records a signature even
--     when it cannot categorise.
--   * Rows resolving to `internal_transfer` are excluded: an
--     own-account move is not spend. Rows resolving to NULL stay in —
--     that is exactly the backlog the model tier works from.
CREATE OR REPLACE MACRO spending_lines_base(p_from, p_to) AS TABLE (
    SELECT p.silver_source_id, p.transaction_external_id, p.occurred_at,
           p.account_external_id, p.account_kind, p.display_name,
           p.nickname, p.account_category,
           p.kind, p.currency, p.net_amount, p.description,
           p.counterparty, p.provider_category,
           e.merchant_signature, m.merchant_name,
           COALESCE(e.spend_detailed, m.spend_detailed) AS spend_detailed,
           c.spend_primary,
           e.provenance
      FROM spend_enrichment_population(p_from, p_to) p
      LEFT JOIN spend_txn_enrichment e
             ON e.silver_source_id        = p.silver_source_id
            AND e.transaction_external_id = p.transaction_external_id
      LEFT JOIN spend_merchant_categories m
             ON m.merchant_signature = e.merchant_signature
      LEFT JOIN spend_categories c
             ON c.spend_detailed = COALESCE(e.spend_detailed, m.spend_detailed)
     WHERE COALESCE(e.spend_detailed, m.spend_detailed) IS DISTINCT FROM 'internal_transfer'
     ORDER BY p.occurred_at, p.silver_source_id, p.transaction_external_id
);

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (41, CAST(epoch(now()) AS BIGINT));
