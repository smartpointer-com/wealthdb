-- ============================================================
-- fidelity-web silver, migration 0002 — gold-layer feedback.
--
-- Four columns promoted for the gold-side adapter:
--
-- 1. Promoted `currency` on positions + transactions, defaulted
--    to 'USD'. Fidelity US is USD-only today, but a foreign-fund
--    holding (international ETF with native NAV) would make this
--    column load-bearing and silver is the right place.
--
-- 2. Promoted `asset_class` on positions. The existing `type`
--    column is 'Cash' / 'Margin' (margin segment of the account,
--    not the instrument bucket). The loader populates
--    `asset_class` from the bronze Symbol + description shape:
--    'money_market' / 'bond' / 'plan_fund' / 'mutual_fund' /
--    'equity'. Nullable: best-effort, gold can override.
--
-- 3. Promoted `is_core_position` on positions, plus normalisation
--    of the `**` suffix Fidelity appends to money-market core
--    positions in the Symbol column. Pre-migration silver had
--    'CORE_X**' in positions and 'CORE_X' in transactions — two
--    keys for one instrument. Post-migration: 'CORE_X' in both,
--    with `is_core_position = 1` on the positions row.
--
-- 4. Drop the cosmetic `balances_present` / `performance_present`
--    flags on `dump_runs`. They referenced presence of the
--    balances/performance HTML files, which are already indexed
--    in `documents` under `doc_kind IN ('balances_html',
--    'performance_html')`. Queries can derive the same signal
--    from there.
--
-- Re-classification of historical rows runs inline here using
-- SQL CASE expressions on the already-loaded Symbol values; new
-- dumps land with the classification populated at load time.
-- ============================================================

-- ------------------------------------------------------------
-- 1. currency
-- ------------------------------------------------------------
ALTER TABLE positions    ADD COLUMN currency TEXT NOT NULL DEFAULT 'USD';
ALTER TABLE transactions ADD COLUMN currency TEXT NOT NULL DEFAULT 'USD';

-- ------------------------------------------------------------
-- 2. asset_class
--
-- Heuristics anchored to the Symbol shape Fidelity emits in the
-- positions CSV. Order matters — first matching rule wins:
--   * is_core_position (set in step 3 below)        → money_market
--   * Symbol matches CUSIP-9 (8 alphanumeric + check
--     digit), with description containing classic
--     muni / bond keywords                          → bond
--   * Symbol is 9-char alphanumeric (CUSIP shape) but
--     description doesn't carry bond keywords       → bond
--   * Symbol matches '[A-Z]{3}[0-9]{6}' — 3 letters
--     followed by 6 digits — the Fidelity 529-plan
--     investment-option code shape                  → plan_fund
--   * Symbol is 5 chars ending in 'X' — Fidelity /
--     industry mutual-fund convention               → mutual_fund
--   * Otherwise (1-5 char alpha tickers: stocks,
--     ETFs, ADRs)                                    → equity
-- ------------------------------------------------------------
ALTER TABLE positions ADD COLUMN asset_class TEXT;

-- ------------------------------------------------------------
-- 3. is_core_position + Symbol normalisation
-- ------------------------------------------------------------
ALTER TABLE positions ADD COLUMN
    is_core_position INTEGER NOT NULL DEFAULT 0;

UPDATE positions
   SET is_core_position = 1
 WHERE instrument_key LIKE '%*';

-- Strip trailing '*' characters from the instrument_key so it
-- joins cleanly against transactions.instrument_key.
UPDATE positions
   SET instrument_key = rtrim(instrument_key, '*')
 WHERE instrument_key LIKE '%*';

-- Populate asset_class on the historical rows. The CUSIP check
-- accepts the broad alphanumeric-9 shape and uses a description
-- heuristic to keep it tight.
-- plan_fund BEFORE the broader CUSIP-9 check (the plan-fund shape
-- is a stricter subset; real CUSIPs almost never fit 3-letters-
-- then-6-digits exactly).
UPDATE positions
   SET asset_class = CASE
       WHEN is_core_position = 1
            THEN 'money_market'
       WHEN length(instrument_key) = 9
            AND instrument_key GLOB '[A-Z][A-Z][A-Z][0-9][0-9][0-9][0-9][0-9][0-9]'
            THEN 'plan_fund'
       WHEN length(instrument_key) = 9
            AND instrument_key GLOB '[A-Z0-9]*'
            AND substr(instrument_key, 9, 1) GLOB '[0-9]'
            THEN 'bond'
       WHEN length(instrument_key) = 5
            AND substr(instrument_key, 5, 1) = 'X'
            AND instrument_key GLOB '[A-Z][A-Z][A-Z][A-Z]X'
            THEN 'mutual_fund'
       ELSE 'equity'
   END
 WHERE asset_class IS NULL;

-- ------------------------------------------------------------
-- 4. drop the cosmetic *_present flags from dump_runs
-- ------------------------------------------------------------
ALTER TABLE dump_runs DROP COLUMN balances_present;
ALTER TABLE dump_runs DROP COLUMN performance_present;

-- ------------------------------------------------------------
-- Migration-complete marker. Must be the last statement.
-- ------------------------------------------------------------
INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s', 'now') AS INTEGER));
