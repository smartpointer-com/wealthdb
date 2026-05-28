# ubs-psn-dump: Design notes for downstream consumers

This document captures decisions and contracts that aren't obvious from
the operational README — primarily for engineers writing adapters
against this repo's silver schema (today: the `wealthdb` gold-layer
adapter). The README covers how to run the tools; this document covers
what the silver data does and does not contain, and why.

## 1. Account taxonomy and pension accounts

What the silver carries: each `cash_accounts` /
`safekeeping_accounts` row promotes the UBS product code in its
`payload.AcctTpCd` / `AcctTpDesc` fields. This section records what
those values do and don't reveal about pension assets. How gold
interprets these columns is owned by the wealthdb UBS adapter — see
[the adapter doc](../../wealthdb/docs/adapters/ubs.md).

### What we know vs. what we suspect

**Known empirically:** scanning every loaded payload across
`cash_accounts`, `safekeeping_accounts`, and `portfolios` for
`pension`, `vorsorge`, `pillar`, `bvg`, `lpp`, `säule`, `saule`,
`freizüg`, `freizug`, `vested`, `retirement` returns zero matches.
All observed `AcctTpCd` / `AcctTpDesc` values are non-pension UBS
private-banking product codes.

**But this evidence is weak**, because the customer producing this
silver data holds no Swiss pension assets at UBS at all. The absence
of pension accounts in PSN therefore tells us nothing about whether
PSN *would* surface them if they existed.

**Strong structural tendency (not proof):** Swiss pension assets
typically live in legally separate entities from a private-banking
relationship:

- **Pillar 2 (BVG / LPP):** the employer's *Personalvorsorgekasse* /
  *Pensionskasse* — a foundation distinct from UBS AG.
- **Vested benefits (Freizügigkeit):** a *Freizügigkeitsstiftung* —
  e.g. "UBS Freizügigkeitsstiftung" is a separate foundation from
  UBS AG, with its own customer numbers and its own reporting.
- **Pillar 3a (Säule 3a):** a *Vorsorgestiftung* (e.g. "UBS AG
  Stiftung 3. Säule") or a 3a-licensed insurance product. Again
  legally separate.

PSN is bound to a UBS AG banking relationship (`ClntId`, the
`0230…` form). Those pension foundations have their own customer-
number and reporting plumbing. It is therefore *likely* — but not
verified — that PSN does not deliver pension data even when a UBS
customer has pension assets in a UBS-affiliated foundation. The
pension-shaped marker values to watch for, should they ever appear
in `AcctTpDesc`, are German `Vorsorge` / `Säule` / `Freizügigkeit`,
English `pension` / `pillar` / `vested`, French `prévoyance` /
`pilier`.

## 2. Account identifier canonicalisation

Documented in detail in [README.md](README.md) ("Identifier
canonicalisation") and the header of
[migrations/0002](migrations/0002_canonicalise_ids_and_promote_portfolio_columns.sql).
Short form for adapter authors:

- `account_external_id` on `cash_accounts`, `cash_balances`, and on
  cash-side `events` rows is always the IBAN.
- `account_external_id` on `safekeeping_accounts` and on
  safekeeping-side `events` rows (kinds `trade_confirmation`,
  `corporate_action_confirmation`) is the MT535 `:97A::SAFE//` /
  UBS-internal `AcctId` form.
- The wealthdb adapter does **not** need a per-relationship lookup
  table to bridge cash or safekeeping IDs across silver tables — the
  loader has already done it.

## 3. Portfolio model

Portfolios and accounts are two separate entities:

- `portfolios` is the UBS wealth-management wrapper (mandate, strategy,
  product wrapper). It has a `base_currency` (`PrtflCcyIsoCd`) and a
  `portfolio_external_id` (`PrtflId`). Portfolios never own positions
  or balances directly.
- `cash_accounts` and `safekeeping_accounts` each carry a nullable
  `portfolio_external_id` pointing at their parent portfolio.
  `NULL` means "standalone bank account, not enrolled in any
  wealth-management portfolio" — common for current/savings accounts.
- The PSN-specific edge case where forwards / money-market / OTC
  contracts are attributed directly to a portfolio (rather than to a
  sub-account) is preserved as PSN delivers it; see the relevant
  contract tables.

## 4. `snapshot_at` is the per-file as-of date, not the dump-retrieval timestamp

Every PSN file inside a dump zip carries a `YYYY-MM-DD_` filename
prefix that is its data's as-of date. Silver's `snapshot_at` columns
(everywhere except `dump_runs`) come from that prefix, **not** from
the bronze dump-directory name.

Practical consequence for adapters: a single bronze dump can produce
silver rows at multiple `snapshot_at` values, when `download.py` was
skipped for a day and the next dump bundles a catch-up batch. Treat
`snapshot_at` as the trustworthy "as-of" coordinate; treat
`dump_runs.snapshot_at` as an audit trail of when each dump was
processed.

## 5. What silver deliberately omits

Where the gold-layer adapter should not look for data because silver
doesn't try to surface it:

- **MT950 (`ZAY.zip`).** Bank-to-bank statement format; a duplicate of
  MT940 for the same accounts in this customer's PSN setup. Bronze
  keeps the raw zip; silver does not ingest. See migration 0001's
  header.
- **MT536, MT568, MT590, MT599, MT600, MT608, MT900/910, MT942,
  MT990.** Loader stubs not yet implemented — added when we have
  real samples. Bronze keeps every zip; silver simply skips.
- **Empty XML containers** (`TDCAPI`, `TDOPT`, `TDMM`, `TDOTC` for
  this customer; `TDPOPF` until UBS produces the monthly batch).
  Silver does not insert empty rows.
- **Computed / derived columns.** No FX-converted values, no
  realised-PnL, no settled flags. Those live in the gold layer.

## 6. Reload contract

Silver is intended to be entirely rebuildable from bronze. Whenever
the loader's interpretation of bronze changes (a new MT type added, a
canonicalisation rule changed, a `snapshot_at` derivation changed),
the operationally-correct response is:

1. Add a migration if the schema changes.
2. Wipe `<silver>.db` and re-run `load.py`.

`dump_runs.snapshot_at` (= the dump-directory timestamp) provides
idempotency on re-runs that don't change loader semantics, but for
loader-semantics changes a wipe is the only consistent path.
