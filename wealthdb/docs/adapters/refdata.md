# refdata adapter

Projects the **refdata** silver into canonical reference records.
`internal/silver/refdata/`.

The refdata collector (`collectors/refdata/`) downloads reference data
from public sources: what a fund holds, what a fund is, and what a
security is. Its silver keeps that data as the providers served it.
This adapter turns it into dated reference records. The exposure report
reads them ([EXPOSURE.md](../EXPOSURE.md)).

refdata is a reference source, like `fred`. It emits no accounts, no
positions and no transactions. Nothing of it reaches the returns engine,
so its kind registers no returns policy.

## 1. Silver source

One SQLite DB (`$XDG_DATA_HOME/wealthdb/refdata/refdata.db`). The schema
is `collectors/refdata/migrations/`. The adapter's test fixture,
`testdata/silver_schema.sql`, is a copy of it, and the collector's tests
check that the two agree.

Conventions:

- `as_of` is the UTC midnight of the day a row describes. For a
  composition it is the day the composition is as of, never the day it
  was fetched or filed. For a security it is the day of the lookup.
- `run_at` is the bronze run that wrote the row. A line has none; it
  belongs to its composition's run.
- `dump_runs.loaded_at` is when the silver load wrote a run. It rises
  with every load, and a load that re-parses a run stamps it again.
- Decimals are strings. Weights are in percent of the fund's net assets.
- Labels stay as fetched: a sector, a country or an asset type in the
  provider's own words. Gold maps them when it reports.

## 2. Coverage matrix

| Silver table | Gold table | One row per |
| --- | --- | --- |
| `dump_runs` | none | Drives `Status` and `ChangeWindow` (§4). |
| `funds` | none | How the provider knows a fund. The collector's own bookkeeping. |
| `compositions` | `fund_compositions` | fund, day and basis |
| `lines` | `fund_lines` | composition line |
| `attributes` | `fund_attributes` | fund and day |
| `counterparties` | `fund_counterparties` | fund, day, role and counterparty |
| `securities` | `security_identities` | security, lookup day and provider |
| `security_facts` | `security_facts` | security, field, lookup day and provider |

Columns pass through by name. Two things are added:

- A composition's `fetched_at` is the row's `run_at`.
- A line gets a `security_key` (§3), computed from its identifiers.

A composition's `basis` is one of three values:

- `holdings`: the fund's own holdings.
- `collateral`: the substitute basket a swap-based fund holds. The
  exposure report never looks through it.
- `index`: the index lines a provider lists for a swap-based fund.

## 3. Identity

One key scheme names both funds and securities
(`internal/canonical/refkeys.go`). The first identifier present wins:

| Key | From |
| --- | --- |
| `isin:<ISIN>` | an ISIN |
| `cusip:<CUSIP>` | a CUSIP |
| `figi:<FIGI>` | an OpenFIGI identifier |
| `symbol:<SYMBOL>` | a ticker, without its exchange |
| `name:<hash>` | a hash of the normalized name, the last resort |

A ticker key carries no exchange. Providers write exchanges in their
own words, and a key must stay stable across providers and dates.

A CUSIP stays a CUSIP key. A CUSIP that starts with a digit belongs to
a US or a Canadian security, and nothing in it says which. The ISIN the
US rule gives (`US`, the CUSIP, the check digit) is therefore a guess.

Gold builds the fund reference of a holding from its instrument's ISIN,
CUSIP and symbol, after it maps the holding's key through
`holding_aliases` and `instrument_aliases()`. The wanted list carries
the reference, and the collector stores the composition under it. The
join back is exact. Gold reads every key through its security aliases
(EXPOSURE.md §1): a ticker or a CUSIP a source states beside an ISIN
reads as that ISIN, and a CUSIP no source ties to an ISIN reads as the
one the US rule gives. A broker's symbol and CUSIP fill each other's
gaps. A fund held at two brokers under two keys that read as one ISIN
is fetched once. What a provider answered under one key counts for the
key it reads as.

## 4. Status and change window

- The snapshot range is the as-of range of `compositions`.
- There are no transactions; the transaction range is `-1`.
- `LatestChangeNumber` is the latest load, `MAX(dump_runs.loaded_at)`.
- Both observed watermarks are the latest fetch run,
  `MAX(dump_runs.snapshot_at)` (`silver.ObserveByRuns`).
  `wealthdb status` therefore shows when reference data was last
  fetched. How old each composition is belongs to the exposure report.

`ChangeWindow` triggers on a load past the watermark. Its window spans
the as-of days that the rows of the runs loaded since describe, over
every reference table. The clock is the load, not the fetch, so a run
fetched earlier but loaded later still reaches gold: an import from
another machine, or a re-parse of kept bronze. A composition for an old
day that arrives late loads like any other. `Snapshots` re-emits every row inside
the window, whichever run wrote it, so gold's windowed delete leaves the
window as silver holds it. A run that wrote nothing advances the
watermark without a window.

`Snapshots` streams the window (`silver.NewReferenceStream`). A batch
holds whole fund-days, up to about twenty thousand lines. Then come the
security identities and the security facts, five thousand to a batch. A
backfill of years holds one batch in memory at a time.

## 5. The wanted list

The collector learns what to fetch from the engine. It runs the hidden
read-only subcommand:

```sh
wealthdb -r refdata-wanted
```

It prints one JSON document, the wanted list (`internal/gold/refdata_wanted.go`).
It is the one way the collector reads gold, and its JSON is a versioned
contract:

- `contract` is `refdata-wanted`, `version` is `1`.
- A change that removes or renames a field, or changes what a field
  means, takes a new version. A field added alone does not.
- A collector refuses a version it does not know.

Top-level fields:

| Field | Meaning |
| --- | --- |
| `generated_at` | when the list was built, RFC 3339 UTC |
| `as_of` | the day it was built for, `YYYY-MM-DD` |
| `settings.max_gap_days` | how far a composition may lie from a day and still describe it |
| `funds[]` | every fund gold wants compositions for, sorted by `ref` |
| `securities[]` | every security gold wants attributes for, sorted by `key` |

A fund:

| Field | Meaning |
| --- | --- |
| `ref` | the fund reference (§3) |
| `isin`, `cusip`, `symbol` | the identifiers known, each `null` where none is; the identifier the reference is made of is always filled |
| `name`, `currency` | the holding's name and the currency it is priced in |
| `roles` | `held`, `proxy` (it stands in for a held fund), `reference` (its lines widen the security master) or `underlying` (a held fund holds it) |
| `held_from`, `held_to` | the days a composition is wanted for; `held_to` is `null` while the fund is still held |
| `compositions` | the days gold already has a composition for, on any basis, ascending |

A fund is wanted when a position holds it as an `etf`, a `fund` or a
`structured_product` whose asset class has lines worth reading: public
equity, fixed income, multi asset, real estate, infrastructure, cash or
other. A metal or crypto fund holds what its asset class already says.
A private fund publishes no lines. A note's basket reaches gold only
through the collector's inbox, since no public source serves one.
The `exposure` config adds proxies and reference funds. A proxy is
wanted over the days of the funds it stands in for; a reference fund
from today. A fund that a held fund holds is wanted from today too, one
level down. It comes from a line of the held fund's newest composition:

- the line's asset type is a fund, and an identifier names the fund;
- a holdings list wins over an index list of the same day;
- a swap fund's basket never counts.

Such a fund states a US symbol only, as a security does below. A fund
named by a ticker with no US listing stays out of the list.

A security:

| Field | Meaning |
| --- | --- |
| `key` | the security key (§3) |
| `isin`, `cusip`, `symbol`, `name`, `currency` | as for a fund |
| `scope` | `direct`: held directly, and always completed. `constituent`: named by a held fund's lines and lacking a sector or a country there; a collector looks these up only when its config allows. A collector skips a scope it does not know. |
| `share_pct` | a constituent's share of net worth through funds, in percent, so a collector asks the biggest first; `null` for a security held directly |
| `asset_class` | the security's asset class: the holding's, or the one its fund lines state, so a lookup that finds companies can skip a bond |
| `missing` | the fields no source states yet: `sector`, `country` and, for a stock held directly, `market_cap` (the fact fields below). A bond's type from a lookup states its sector. A municipal bond's type states its country too. For a constituent, a country only an ISIN's prefix implies counts as missing, and so does a sector or a country a depositary receipt took from a share matched by name (EXPOSURE.md §3). |
| `lookups` | the day each provider last answered for the security, so a collector refreshes what is older than its `max_age`. An answer under another key that found nothing does not count, so the security's own identifier is asked. |

A security is wanted when a position holds it as a `stock` or a `bond`,
or when the look-through of the day reaches it through a fund held today
(an equity or a bond line, any level deep) and its leaf there has no
sector or no country. A line naming a fund, and a fund the look-through
stops at, are no constituents: no company lookup describes a fund. A
constituent states the identifiers its key and its aliases give: a US
symbol only from a fund line on a US listing or a lookup, never a
broker's local ticker, so a lookup by ticker finds the right company.
A security held directly states its own symbol only where the holding
has a US ISIN or is priced in US dollars. Otherwise it states the US
symbol a lookup found, if any. A bank abroad states the local ticker,
which in New York may be another company's.

A fund or security counts as still held while the latest snapshot of
its source lists it and the source was observed within 90 days. A source
that stopped reporting ends its holdings at its last snapshot.

The fact fields a provider writes, and the wanted field each answers:

| Fact field | Answers |
| --- | --- |
| `sector` | `sector`, in the provider's own words (`scheme` names the scheme) |
| `sic` | `sector`, as an SEC SIC code |
| `risk_country`, `hq_country`, `legal_country`, `incorporation` | `country` |
| `market_cap`, `shares_outstanding`, `size` | `market_cap` |
| `issuer_isins` | `sector` and `country` of a depositary receipt: the other ISINs of its issuer, space-separated, among which gold finds the share it stands for |

A gold opened read-only before the exposure tables existed has no
reference data and no exposure config yet. The list then names the
held funds and securities with nothing known, so the first fetch can
run before the first load.

## 6. The exposure config tables

The `exposure` block of `wealthdb.cfg` (DESIGN.md §5.1) and its tags
ledger are written into gold at every load, after the per-source loads,
beside the holding aliases (`gold.SyncExposure`). Each table is replaced
whole, so an entry taken out of the config or the ledger leaves on the
next load:

- `exposure_proxies`: a fund whose composition stands in for another;
- `exposure_reference_funds`: funds fetched for their lines alone;
- `exposure_tags`: the tags ledger's rows, the sector, country and
  currency of a private holding (EXPOSURE.md §9);
- `exposure_custody`: who keeps each source's assets;
- `exposure_settings`: one row of settings.

## 7. The synthetic kind

The `synthetic` kind repeats the six reference tables column for column
([synthetic.md](synthetic.md) §12), and reads them through the same
stream. A generator such as the demo household can write compositions
and security facts beside its accounts.
