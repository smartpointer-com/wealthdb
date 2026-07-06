# ubs-psn: Design notes for downstream consumers

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

### Why pension assets are unlikely to appear in PSN

PSN is a private-banking custody/cash feed. Swiss pension assets
typically live in legally separate entities from a private-banking
relationship, so they are structurally unlikely to surface in this
feed regardless of a given customer's holdings. An absence of
pension accounts in the loaded PSN data would not, on its own,
establish that PSN *would* surface them if they existed — they may
simply never fall within PSN's scope:

- **Pillar 2 (BVG / LPP):** the employer's *Personalvorsorgekasse* /
  *Pensionskasse* — a foundation distinct from UBS AG.
- **Vested benefits (Freizügigkeit):** a *Freizügigkeitsstiftung* —
  e.g. "UBS Freizügigkeitsstiftung" is a separate foundation from
  UBS AG, with its own customer numbers and its own reporting.
- **Pillar 3a (Säule 3a):** a *Vorsorgestiftung* (e.g. "UBS AG
  Stiftung 3. Säule") or a 3a-licensed insurance product. Again
  legally separate.

PSN is bound to a UBS AG banking relationship (`ClntId`, a
14-digit numeric relationship-number form, distinct from the IBAN
and MT535 `AcctId`). Those pension foundations have their own customer-
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
- **Empty XML containers** (`TDCAPI`, `TDOPT`, `TDMM`, `TDOTC` when
  the relationship is not provisioned for, or holds none of, those
  product types; `TDPOPF` until UBS produces the monthly batch).
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

## 7. Bronze layout and pruning

```
<bronze-root>/
├── 20260524T120000Z/
│   ├── run.json                      status marker: "in-progress" at
│   │                                 run-dir creation, "complete" once
│   │                                 the pull finishes
│   ├── ZMD.zip                       PSN XML master data (SDCL/SDCA/SDSA/…)
│   ├── ZME.zip                       PSN XML rates / contracts (TDFXR/…)
│   ├── ZAH.zip                       MT535 holdings
│   ├── Z40.zip                       MT940 cash balances + movements
│   ├── …                             one <ORDERTYPE>.zip per queued type
│   └── HAC.zip / PTK.zip             EBICS admin zips (raw bronze; not
│                                     load inputs, but never debug artefacts)
├── 20260525T120000Z/
│   └── …
└── ubs-psn.db                        silver SQLite (default location)
```

A run dir is a flat set of `<ORDERTYPE>.zip` files — no subdirs, no
debug artefacts. Every `Z*.zip` is a `load` input (the loader globs
`Z*.zip` for both its XML and MT passes); the non-`Z` admin zips
(`HAC`/`PTK`) are raw bronze the loader ignores but that `prune` still
keeps. The PSN zips are irreplaceable: UBS deletes each file
server-side on a successful download, so a re-run cannot recover it.

`prune` therefore treats any run dir containing a zip as complete and
untouchable — the has-zip check short-circuits *before* the `run.json`
`status` field is consulted, so even a crash that left `status` at
`"in-progress"` alongside already-fetched zips is kept whole. With no
debug artefacts to reclaim, the only path `prune` can ever delete is a
zip-less crash shell (a run dir minted before the first `sftp.get`),
and only once it is quiescent. `download.py` already removes a run dir
that fetched nothing, so in practice the verb is a safety-first
near-no-op whose value is guaranteeing a fleet-wide prune never deletes
a load input.
