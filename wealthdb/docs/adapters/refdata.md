# refdata adapter

Projects the **refdata** silver into canonical reference records.
`internal/silver/refdata/`.

The refdata collector (`collectors/refdata/`) downloads reference data
from public sources:

- what a fund holds and what a fund is;
- what a security is;
- a fund's rate sensitivities, and the figures stated for the fund as a
  whole;
- a company's reported figures, by period.

Its silver keeps that data as the providers served it. This adapter
turns it into dated reference records. The exposure report reads the
records of the first two kinds ([EXPOSURE.md](../EXPOSURE.md)).

refdata is a reference source, like `fred`. It emits no accounts, no
positions and no transactions. Nothing of it reaches the returns engine,
so its kind registers no returns policy.

## 1. Silver source

One SQLite DB (`$XDG_DATA_HOME/wealthdb/refdata/refdata.db`). The schema
is `collectors/refdata/migrations/`:

- `0001_initial.sql` holds the compositions, their lines, the fund
  attributes and counterparties, and the security lookups and facts.
- `0002_yield_facts.sql` adds what 0001 lacks: the debt and duration
  columns of a line, and the tables `fund_risk`, `characteristics` and
  `period_facts`.

The adapter's test fixture, `testdata/silver_schema.sql`, is the two
files in order. The collector's tests check that the copy matches the
migrations. A file at schema 1 loads too: the adapter reads the tables
and columns of 0002 only where the file has them.

Conventions:

- `as_of` is the UTC midnight of the day a row describes. For a
  composition it is the day the composition is as of, never the day it
  was fetched or filed. For a security it is the day of the lookup. For
  a company's figure it is the last day of the period it covers.
- `run_at` is the bronze run that wrote the row. A line has none; it
  belongs to its composition's run.
- `dump_runs.loaded_at` is when the silver load wrote a run. It rises
  with every load, and a load that re-parses a run stamps it again.
- Decimals are strings. Weights are in percent of the fund's net assets.
  A yield or a share is in percent, a duration in years.
- A flag is 1, 0 or NULL. Gold holds it as a boolean.
- Labels stay as fetched: a sector, a country or an asset type in the
  provider's own words. Gold maps them when it reports.

## 2. Coverage matrix

| Silver table | Gold table | One row per |
| --- | --- | --- |
| `dump_runs` | none | Drives `Status` and `ChangeWindow` (§4). |
| `compositions` | `fund_compositions` | fund, day and basis |
| `lines` | `fund_lines` | composition line |
| `attributes` | `fund_attributes` | fund and day |
| `counterparties` | `fund_counterparties` | fund, day, role and counterparty |
| `securities` | `security_identities` | security, lookup day and provider |
| `security_facts` | `security_facts` | security, field, lookup day and provider |
| `fund_risk` | `fund_risk` | fund, day, currency and tenor |
| `characteristics` | `fund_characteristics` | fund, day, provider, field and label |
| `period_facts` | `security_period_facts` | security, field, period and provider |

Gold's `securities` is a different table. The exposure pass builds it
from every source that describes a security (DESIGN.md §10.14).

Columns pass through by name. Two things are added:

- A composition's `fetched_at` is the row's `run_at`.
- A line gets a `security_key` (§3), computed from its identifiers.

A line also has the columns of 0002, filled for a debt line or where
an issuer's file states them:

- `coupon_kind`, as the provider writes it (N-PORT's Fixed, Floating,
  Variable, None);
- `is_default`, `in_arrears` and `paid_in_kind`, the default, arrears
  and paid-in-kind flags;
- `exchange_rate`, the rate the provider valued the line at, in units
  of the line's currency per unit of the composition's;
- `duration`, `modified_duration`, `real_duration`, in years;
- `yield_to_maturity`, `yield_to_worst`, `yield_to_call`, `real_yield`,
  in percent;
- `accrual_date`, the day interest accrues from.

A `fund_risk` row states how much the fund's value moves for a move in
rates at one tenor, signed as filed. An interest-rate row names the
curve's currency. A credit-spread row has the currency `XXX`. Filers
follow no one sign convention: gold reads each filing in one sign
(`fund_risk_signed`, YIELD.md §6). A `characteristics` row is
one figure a provider states for the fund as a whole. Its `label` is
empty, or names one part of a breakdown such as a credit quality. A
`period_facts` row is a figure a company reported for one period.

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
join back is exact.

Gold then reads every key through its security aliases
(`security_aliases`, rebuilt at every load):

- A ticker, CUSIP or FIGI that a fund line, a lookup or an instrument
  states beside an ISIN reads as that ISIN.
- Where sources disagree, an account's own instrument wins, then a
  lookup, then a fund line. Within one kind, the ISIN stated most often
  wins, then the newest.
- A fund line's ticker counts only where the line names a US listing: a
  US ISIN, a US exchange, or no exchange and a US-dollar price. A
  broker's symbol counts only where the instrument has a US ISIN or is
  priced in US dollars. A local ticker abroad is often another
  company's US symbol.
- A CUSIP no source ties to an ISIN reads as the one the US rule gives.
- A broker that states a holding's symbol and CUSIP together, with no
  ISIN, lets each stand in for the other. A US CUSIP takes its symbol's
  ISIN only where that ISIN holds the CUSIP (a Canadian share), since
  a ticker may be reused.

A fund held at two brokers under two keys that read as one ISIN is
fetched once. What a provider answered under one key counts for the
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
the as-of days that the rows of the runs loaded since describe. It reads
every reference table that has a run: `compositions`, `attributes`,
`counterparties`, `fund_risk`, `characteristics`, `securities`,
`security_facts` and `period_facts`. A line belongs to its
composition's run. The clock is the load, not the fetch, so a run
fetched earlier but loaded later still reaches gold: an import from
another machine, or a re-parse of kept bronze. A composition for an old
day that arrives late loads like any other. `Snapshots` re-emits every
row inside the window, whichever run wrote it, so gold's windowed
delete leaves the window as silver holds it. A run that wrote nothing
advances the watermark without a window.

`Snapshots` streams the window (`silver.NewReferenceStream`). A batch
holds whole fund-days, up to about twenty thousand records. A fund-day
carries its rate sensitivities and its characteristics. Then come the
security identities, the security facts and the period facts, five
thousand to a batch. A backfill of years holds one batch in memory at a
time.

## 5. The wanted list

The collector learns what to fetch from the engine. It runs the hidden
read-only subcommand:

```sh
wealthdb -r refdata-wanted
```

It prints one JSON document, the wanted list (`internal/gold/refdata_wanted.go`).
It is the one way the collector reads gold, and its JSON is a versioned
contract:

- `contract` is `refdata-wanted`, `version` is `2`.
- A change takes a new version when a collector of the version before
  would misread the list. Such a change removes or renames a field,
  changes what a field means, gives a field a value it did not hold
  before, or adds a field that asks for work the older collector would
  skip.
- A field added alone, that an older collector may ignore, takes no new
  version.
- A collector refuses a version it does not know. The refdata collector
  reads versions 1 and 2.
- Version 1 has no `wants`, `facts` or `periods`. Its `missing` names no
  family of period facts.

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
| `wants` | the facts the fund is wanted for beyond its compositions (below), in the order below; `[]` where none |
| `facts` | per fact, the days gold holds it for the fund, ascending, `YYYY-MM-DD`; a fact gold holds nothing of is absent; `{}` where nothing |

A fund is wanted when a position holds it as an `etf`, a `fund` or a
`structured_product` whose asset class has lines worth reading: public
equity, fixed income, multi asset, real estate, infrastructure, cash or
other. A metal or crypto fund holds what its asset class already says.
A private fund publishes no lines. A note's basket reaches gold only
through the collector's inbox, since no public source serves one.
The `exposure` config adds proxies, reference funds and size funds. A
proxy is wanted over the days the funds it stands in for were held,
else from today. A reference or size fund is wanted from today, in the
role `reference`. A fund that a held fund holds is wanted from today
too, one level down. It comes from a line of the held fund's newest
composition:

- the line's asset type is a fund, and an identifier names the fund;
- a holdings list wins over an index list of the same day;
- a swap fund's basket never counts.

Such a fund states a US symbol only, as a security does below. A fund
named by a ticker with no US listing stays out of the list.

A fund held (role `held`) wants these facts, in this order:

| Fact | Wanted by | What it is |
| --- | --- | --- |
| `risk_metrics` | a fund of class fixed income or multi asset | its N-PORT filing's rate sensitivities (`fund_risk`) |
| `characteristics` | every fund held | the figures its issuer's page states: duration, yields, credit quality |
| `money_market` | a fund of class cash, a money-market fund | its 7-day yields and its weighted average maturity and life |
| `line_durations` | a fund of class fixed income whose newest composition has no line with a duration or a modified duration | the durations of its lines, which come only with a composition that states them, such as an issuer's holdings file |

The class is the one a holding states. A bond fund with no composition
yet wants `line_durations` too. A fund that only the roles `proxy`,
`reference` or `underlying` name wants none of these.

`facts` lists the days gold holds for every fund, whatever its role. The
fund is read through its aliases, as for `compositions`. A collector
skips a day it already has, and reads freshness from the last one. Each
fact has its days from these rows:

- `risk_metrics`: the fund's `fund_risk` rows.
- `money_market`: its `fund_characteristics` rows whose field is
  `seven_day_yield`, `seven_day_gross_yield`,
  `weighted_average_maturity` or `weighted_average_life`.
- `characteristics`: its other `fund_characteristics` rows.
- `line_durations`: its compositions that have a line with a duration
  or a modified duration.

A fund states the identifiers gold knows for it, so a provider can
claim it by the one it reads. Where several state one field, the
holding's or the fund line's wins. Next come the CUSIP and the US symbol
its aliases state (§3). Last comes the one its reference is made of. A size fund named by
its ISIN thus states its ticker once a lookup or a fund line ties the
two.

A security:

| Field | Meaning |
| --- | --- |
| `key` | the security key (§3) |
| `isin`, `cusip`, `symbol`, `name`, `currency` | as for a fund |
| `scope` | `direct`: held directly, and always completed. `constituent`: named by a held fund's lines, and lacking a sector or a country there or a family of period facts; a collector looks these up only when its config allows. A collector skips a scope it does not know. |
| `share_pct` | a constituent's share of net worth through funds, in percent, so a collector asks the biggest first; `null` for a security held directly |
| `asset_class` | the security's asset class: the holding's, or the one its fund lines state, so a lookup that finds companies can skip a bond |
| `missing` | the fields no source states yet: `sector`, `country` and, for a stock held directly, `market_cap` (the fact fields below). Then, for public equity, `earnings`, `dividends` and `financials` (below). A bond's type from a lookup states its sector. A municipal bond's type states its country too. For a constituent, a country only an ISIN's prefix implies counts as missing, and so does a sector or a country a depositary receipt took from a share matched by name (EXPOSURE.md §3). |
| `lookups` | the day each provider last answered for the security, so a collector refreshes what is older than its `max_age_days`. An answer under another key that found nothing does not count, so the security's own identifier is asked. |
| `periods` | per family of period facts, the newest period end gold holds, `YYYY-MM-DD`, the security read through its aliases; a family gold holds nothing of is absent; `{}` where nothing. A collector asks only for later periods. |

A period fact is a figure a company reported for one period
(`security_period_facts`). Its fields fall into three families:
`earnings`, `dividends` and `financials` (the fact fields below). A
security of class public equity wants all three: a stock held directly,
or a constituent. A family is missing while gold holds no period of it
that ended within 135 days of the list's `as_of`: a quarter and the 45
days a quarterly report may take. So the next quarter is asked for once
it is due. A bond or a fund wants none of them.

A security is wanted when a position holds it as a `stock` or a `bond`,
or when the look-through of the day reaches it through a fund held today
(an equity or a bond line, any level deep). Such a constituent is wanted
when its leaf there has no sector or no country, or when it is public
equity and misses a family of period facts. Its `missing` names exactly
what it lacks. A line naming a fund, and a fund the look-through
stops at, are no constituents: no company lookup describes a fund. A
constituent states the identifiers its key and its aliases give: a US
symbol only from a fund line on a US listing or a lookup, never a
broker's local ticker, so a lookup by ticker finds the right company.
A security held directly states its own symbol only where the holding
has a US ISIN or is priced in US dollars. Otherwise it states the US
symbol a lookup found, if any. A bank abroad states the local ticker,
which in New York may be another company's.

A fund or security counts as still held while its account's latest
active snapshot lists it (DESIGN.md §10.7) and the source was observed
within 90 days. A source that stopped reporting ends its holdings at
its last snapshot.

The fact fields a provider writes, and the wanted field each answers.
The security facts come first, then the period facts:

| Fact field | Answers |
| --- | --- |
| `sector` | `sector`, in the provider's own words (`scheme` names the scheme) |
| `sic` | `sector`, as an SEC SIC code |
| `risk_country`, `hq_country`, `legal_country`, `incorporation` | `country` |
| `market_cap`, `shares_outstanding` | `market_cap` |
| `issuer_isins` | `sector` and `country` of a depositary receipt: the other ISINs of its issuer, space-separated, among which gold finds the share it stands for |
| `eps_diluted` | `earnings`, a period fact |
| `dividends_per_share` | `dividends`, a period fact |
| `net_income`, `revenue`, `operating_income`, `interest_expense`, `long_term_debt`, `cash`, `operating_cash_flow`, `capex` | `financials`, a period fact |

A gold opened read-only before the exposure tables existed has no
reference data and no exposure config yet. The list then names the
held funds and securities with nothing known, so the first fetch can
run before the first load. A gold before the fact tables (migration
0134) holds no facts. Its funds list their wants with no days, and its
stocks miss every family.

## 6. The synthetic kind

The `synthetic` kind repeats the nine reference tables column for column
([synthetic.md](synthetic.md) §12), and reads them through the same
stream. A generator such as the demo household can write any of these
records beside its accounts.
