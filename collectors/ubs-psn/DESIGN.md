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
  MT940 for the same accounts in a retail PSN setup. Bronze
  keeps the raw zip; silver does not ingest. See migration 0001's
  header.
- **MT536, MT568, MT590, MT599, MT600, MT608, MT900/910, MT942,
  MT990.** Loader stubs not yet implemented — added when real
  samples exist to develop against. Bronze keeps every zip; silver
  simply skips.
- **Empty XML containers** (`TDOPT`, `TDMM`, `TDOTC` when the
  relationship is not provisioned for, or holds none of, those product
  types; `TDCAPI` on days with no service-charge or interest bookings;
  `TDPOPF` between monthly emissions). Silver does not insert empty
  rows.
- **Computed / derived columns.** No FX-converted values, no
  realised-PnL, no settled flags. Those live in the gold layer. The
  cost columns of §8 are figures the source states, not derived ones.

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
│   │                                 run-dir creation, "complete"/"empty"
│   │                                 once the pull finishes; "mode" is
│   │                                 "download" or "recover"
│   ├── listing.json                  the full pre-pull SFTP listing (per
│   │                                 order type: filenames + sizes),
│   │                                 the accepted host-key fingerprint,
│   │                                 a capture stamp; provenance, never
│   │                                 a load input
│   ├── ZMD.zip                       PSN XML master data (SDCL/SDCA/SDSA/…)
│   ├── ZME.zip                       PSN XML rates / contracts (TDFXR/…)
│   ├── ZAH.zip                       MT535 holdings
│   ├── Z40.zip                       MT940 cash balances + movements
│   ├── …                             one <ORDERTYPE>.zip per queued type;
│   │                                 a --recover run lands
│   │                                 <ORDERTYPE>_<YYYYMMDD>.zip instead
│   ├── HAC.zip / PTK.zip             EBICS admin zips (raw bronze; not
│   │                                 load inputs, but never debug artefacts)
│   └── screenshots/                  legacy --debug capture from before
│                                     listing.json; never a load input
├── 20260525T120000Z/
│   └── …
└── ubs-psn.db                        silver SQLite (default location)
```

A run dir is a flat set of zips plus the two JSON records. A normal
pull lands `<ORDERTYPE>.zip` queue files; a `--recover` run lands
`<ORDERTYPE>_<YYYYMMDD>.zip` dated archive copies. Every `Z*.zip` is a
`load` input (the loader globs `Z*.zip` for both its XML and MT passes
and routes a dated stem to the same order-type loader); the non-`Z`
admin zips (`HAC`/`PTK`) are raw bronze the loader ignores but that
`prune` still keeps. A fetched queue zip cannot be fetched again — UBS
deletes it server-side on a successful download — but a dot-prefixed
dated archive copy of each batch stays on the server for roughly two
months and survives fetching, which is what `--recover` replays;
beyond that window a batch is irreplaceable.

`prune` therefore treats any run dir containing a zip as complete and
untouchable — the has-zip check short-circuits *before* the `run.json`
`status` field is consulted, so even a crash that left `status` at
`"in-progress"` alongside already-fetched zips is kept whole. The one
thing it reclaims from such a dump is a legacy `screenshots/` capture
(`debug_subdirs`), which holds no load input; `listing.json` is
provenance and stays. Otherwise it can delete only a zip-less shell,
and only once quiescent: a crash shell (a run dir minted before the
first `sftp.get`), or the `status: "empty"` dump a pull leaves when
nothing was queued — kept so its `listing.json` can be read, since
discarding it would hide the listing in exactly the case it explains.
The verb stays safety-first; its value is guaranteeing a fleet-wide
prune never deletes a load input.

## 8. Cost basis

### Holdings

An MT535 holding states its cost. Silver promotes it to columns on
`holdings` (migration 0005). Each value is stored as printed, with no
conversion. NULL means the holding does not state the value; silver
never derives one.

| Column | MT535 source | Meaning |
| --- | --- | --- |
| `cost_basis` | `:19A::BOOK//<CCY><amount>` | total book cost |
| `cost_currency` | the currency of BOOK | currency of `cost_basis` and `average_cost` |
| `average_cost` | `AVER` in the `:70C::SUBB//` narrative | average cost per unit |
| `acquisition_fx_rate` | `AEXR` in the narrative | average FX rate of the purchases |
| `acquisition_fx_from` | first currency of `AEXR` | the instrument currency |
| `acquisition_fx_to` | second currency of `AEXR` | the reference currency |

How to read them:

- BOOK and AVER are in the instrument currency. `holdings` has no
  currency column of its own, so `cost_currency` is set whenever a
  cost figure is.
- One unit of `acquisition_fx_from` is `acquisition_fx_rate` units of
  `acquisition_fx_to`.
- The narrative also carries `AHOD`. It restates BOOK and is not
  promoted.
- An `AVER` that is not a currency amount, such as a percent of
  nominal, is not promoted. Its raw text stays in `payload.fields`.
- A holding that states no BOOK keeps `cost_basis` NULL.

### Trade confirmations

The MT515 payload structures the trade's charges as
`transaction_tax_*` (`:19A::TRAX//`), `stamp_duty_*` (`:19A::STAM//`)
and `charges_*` (`:19A::CHAR//`). Each is an `_amount` and a
`_currency` key, both null when the confirmation states no such
charge. An amount carrying ISO 15022's `N` sign is negative.

### Rows loaded before migration 0005

The migration only adds the columns. On every run, `load.py` fills the
rows that predate them from what silver already stores: a holding's
FIN block in `payload.fields`, a confirmation's block 4 in
`payload.raw_fields`. The pass reads no bronze, and a filled row equals
a freshly loaded one. Once every such row is filled, the pass finds
nothing to do. The exception is a holding whose cost field the parse
cannot read: it stays NULL and the pass reads it again on each run.
