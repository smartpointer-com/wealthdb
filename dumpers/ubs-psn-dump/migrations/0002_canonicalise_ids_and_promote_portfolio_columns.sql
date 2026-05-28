-- ============================================================
-- ubs-psn-dump silver schema, migration 0002 —
--   canonicalise account identifiers across silver tables, and
--   promote portfolio linkage / base currency from payload JSON to
--   first-class columns.
--
-- Background
-- ----------
-- Before this migration, silver stored four flavours of account ID:
--
--   - cash_accounts.account_external_id        = IBAN
--   - cash_balances.account_external_id        = MT940 ':25:'
--                                                (UBS 21-char internal form)
--   - safekeeping_accounts.account_external_id = SDSA ExtAcctId
--                                                (dashed display form)
--   - holdings.safekeeping_external_id         = MT535 ':97A::SAFE//'
--                                                (matches SDSA AcctId)
--
-- The mismatch between the two sides of each pair forced the gold-layer
-- adapter (wealthdb) to keep per-relationship lookup tables to bridge
-- them. From this migration onward, the loader normalises:
--
--   - cash side:        every account_external_id is the IBAN.
--                       load.py converts MT940 ':25:' to IBAN via the
--                       same-dump cash_accounts.payload.AcctId lookup.
--   - safekeeping side: every account_external_id is the MT535/AcctId
--                       form (which is UBS's primary internal form).
--                       load_sdsa switches from ExtAcctId to AcctId.
--
-- The schema does not enforce the canonical format — the contract is in
-- the loader. Silver databases that already exist must be rebuilt from
-- bronze for the change to apply; a plain in-place migration would
-- leave a mix of old- and new-form rows.
--
-- New columns
-- -----------
-- Three nullable columns are promoted from payload-internal fields:
--
--   - cash_accounts.portfolio_external_id        — UBS PrtflId of the
--     parent portfolio, or NULL for accounts not enrolled in any
--     wealth-management portfolio (the gold-layer adapter calls these
--     "standalone bank accounts"). The column is nullable because the
--     source legitimately omits PrtflId on some accounts.
--   - safekeeping_accounts.portfolio_external_id — same, for the
--     custody side; also nullable for the same reason.
--   - portfolios.base_currency                   — the portfolio's
--     reporting currency (UBS PrtflCcyIsoCd: CHF / EUR / USD). Was
--     buried at portfolios.payload.PrtflKey.PrtflCcyIsoCd.
--
-- Columns are nullable because (a) the UBS source legitimately omits
-- PrtflId on some accounts and (b) NOT NULL with ADD COLUMN requires a
-- default expression in SQLite. We may tighten them in a later
-- migration once we have run enough cycles to confirm the nulls are
-- only in the expected places.
-- ============================================================

ALTER TABLE cash_accounts        ADD COLUMN portfolio_external_id TEXT;
ALTER TABLE safekeeping_accounts ADD COLUMN portfolio_external_id TEXT;
ALTER TABLE portfolios           ADD COLUMN base_currency         TEXT;

INSERT INTO schema_meta (silver_schema_version, applied_at)
    VALUES (2, CAST(strftime('%s', 'now') AS INTEGER));
