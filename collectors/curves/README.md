# curves

Part of the **wealthdb** suite. See [the architecture overview](../../DESIGN.md)
for the bronze → silver → gold model and [collectors/README.md](../README.md)
for the shared collector conventions.

A collector for the public government yield curves of two currencies:

- **EUR**: the euro area yield curve of the
  [ECB Data Portal](https://data.ecb.europa.eu/data/datasets/YC). It is
  fitted to the bonds of the triple-A rated euro area governments.
- **CHF**: the spot rates of Swiss Confederation bonds from the
  [SNB data portal](https://data.snb.ch/en/topics/ziredev).

The `curves` map of `wealthdb.cfg`'s `yield` block
([config fields](../../wealthdb/docs/DESIGN.md#51-fields)) picks one
series per tenor for a currency's curve. The yield report takes its
benchmark from that curve. The income projection derives its forward
rates from it. The `fred` collector serves the US dollar's curve the
same way.

There is no login and no key. Both portals are public. A run reads two
APIs:

- the ECB's data API, `https://data-api.ecb.europa.eu/service/data`;
- the SNB's warehouse API, `https://data.snb.ch/api/warehouse/cube`: a
  cube's data, and its latest-update endpoint.

## Tools

| Script | Purpose |
| --- | --- |
| [`download.py`](download.py) | Ask each portal for its series over the requested window, into bronze (each raw CSV answer + a `run.json` manifest). Read-only. |
| [`load.py`](load.py) | Parse bronze into the silver `rates` table. Rows upsert by date, so a re-fetch overwrites a revised day. Already-loaded runs are skipped. |
| [`prune.py`](prune.py) | Reclaim bronze disk: delete non-complete (crashed) run dirs and `--debug` traces. A complete dump's `load` inputs are never touched. |

`login` is a no-op, since there is no session. `login --check` asks each
portal for its cheapest answer and reports it:

- the ECB for its newest ten-year rate;
- the SNB for the latest update of its cube, since it asks clients not
  to poll its data (see Terms).

It writes nothing. `download --dry-run` asks the portals as a download
does and writes no bronze.

## Series

A download fetches every series of both currencies. The series are a
built-in map in [download.py](download.py), `RATE_SERIES`.
`download --series` fetches a comma-separated subset. A currency code
(`EUR`, `CHF`) stands for every series of that currency.

| currency | series ids | tenors | from |
| --- | --- | --- | --- |
| EUR | `YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_<tenor>`, e.g. `…SR_10Y` | 3M, 6M, 9M, then every year from 1Y to 30Y | 2004-09-06 |
| CHF | `SNB1A.SNB.NSS.KZS.EID.J<years>M<months>`, e.g. `…J10M0` (10Y), `…J01M6` (18M) | every half year from 1Y to 30Y | 1988-01-04 |

An EUR id is the ECB's own series key. Its parts are:

- `YC`, the dataflow;
- `B`, business days;
- `U2`, the euro area;
- `EUR`, the currency;
- `4F`, the ECB;
- `G_N_A`, the triple-A rated central governments;
- `SV_C_YM`, the Svensson model with continuous compounding, fitted to
  yields;
- `SR_<tenor>`, the spot rate of the tenor.

A CHF id is the SNB's warehouse cube and the item of its
time-to-maturity dimension, `LAUFZEIT`. The id names the cube as the
API's paths do, with dots. The answer names the same cube
`SNB1A@SNB.NSS.KZS.EID`. A half-year tenor takes the config's form in
months: `J01M6` is `18M`.

Both portals publish business-daily. Each adds a day's rates on the
next business day.

To add a series, add it to `RATE_SERIES` with its portal, currency,
tenor, basis and compounding.

### Basis

Every series is a **spot rate**: the zero rate, or yield of a single
payment at the tenor, in percent. Both portals fit a
Nelson-Siegel-Svensson model. Each silver row's payload states the
rate's compounding:

- **EUR, continuous**: `"basis":"spot","compounding":"continuous"`. Each
  series' title on the portal states "Svensson model - continuous
  compounding", and `C` in `SV_C_YM` stands for it. The published rates
  equal the model's, computed from the parameters the ECB publishes
  beside them.
- **CHF, annual**: `"basis":"spot","compounding":"annual"`. The SNB's
  [explanations](https://data.snb.ch/en/topics/ziredev/doc/explanations_ziredev)
  ("Calculation of yields on Swiss franc bonds") state that "the
  published bond yields for various maturities are spot rates". In its
  price equation the spot rate is "the (annualised) interest rate of a
  discount bond". The model itself works with "continuous compounded
  spot rates". The published rates are the annual ones: each equals
  e^(R/100) − 1, in percent, of the model's continuous rate R, computed
  from the parameters the SNB publishes (cube `SNB1A.SNB.NSS.KOF.EID`).
  Checked on 2026-10-21 for 1Y to 20Y over five days.

The engine reads a spot curve as its zero curve. It converts each rate
exactly to the engine's semiannual compounding, and bootstraps nothing.
The ECB also publishes par yields. A par curve would have to be
bootstrapped, which adds interpolation error between the published
tenors. The par yields come from the same fitted model, so they add
nothing.

## Usage

```sh
./curves login --check                     # ask each portal for its cheapest answer
./curves download                          # last ~90 days of every series (default)
./curves download --series CHF             # the franc's series only
./curves download --series EUR --lookback 2026-10-01
./curves load
./curves prune --dry-run
```

`download` honours the shared
[`--lookback` window](../README.md#collector-cli-conventions). An early
date such as `--lookback 1950-01-01` fetches each series' whole history,
since each portal answers from its own start. The directory overrides
follow the
[shared contract](../README.md#the-wrapper--the-wealthdb-collect-contract).
`--secrets-dir` is accepted but unused.

A run asks each portal once, for all of its series. It asks the SNB's
latest-update endpoint first (see Terms). It skips the SNB's data when
an earlier complete run already holds the same publication for this
run's window and series. The manifest then names that run.

Each `download` writes one UTC-timestamped bronze run dir under
`$XDG_DATA_HOME/wealthdb/curves/`. It holds a `run.json` manifest and one
CSV answer per portal asked: `ecb.csv`, `snb.csv`. The manifest records
what each series is (its portal, currency, tenor, basis and compounding),
so `load` rebuilds silver from bronze alone. It follows the
[shared `status` lifecycle](../README.md#debug-artefacts-run-status-and-pruning-bronze),
so `load` never ingests a crashed walk. The silver DB is `curves.db`,
beside the run dirs.

`download --debug` adds `screenshots/http-trace.jsonl` to the run dir.
Each line records one request: its URL, status, timing, size and cache
headers. Bodies are not duplicated. `load` ignores the trace and `prune`
reclaims it.

### In wealthdb.cfg

```jsonc
"silver_sources": [
  { "id": "curves", "kind": "curves", "path": "$XDG_DATA_HOME/wealthdb/curves/curves.db" }
],
"yield": {
  "curves": {
    "EUR": { "3M": "curves:YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_3M",
             "2Y": "curves:YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_2Y",
             "10Y": "curves:YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_10Y" },
    "CHF": { "1Y": "curves:SNB1A.SNB.NSS.KZS.EID.J01M0",
             "2Y": "curves:SNB1A.SNB.NSS.KZS.EID.J02M0",
             "10Y": "curves:SNB1A.SNB.NSS.KZS.EID.J10M0" }
  }
}
```

After adding the source, `wealthdb load curves` merges it into gold.

### Reclaiming disk

```sh
./curves prune --dry-run   # print the deletion plan, delete nothing
./curves prune             # delete it
```

`prune` works within the
[shared safety envelope](../README.md#debug-artefacts-run-status-and-pruning-bronze).
It removes whole non-complete run dirs and the `screenshots/` trace that
`--debug` leaves in a complete one. Every other file in a complete dump
(`run.json` and each `<portal>.csv`) is a `load` input and stays.

## Silver schema

One row per `(observation date, series)` in `rates`, the columns of the
`fred` collector's table of the same name:

| column | meaning |
| --- | --- |
| `as_of` | observation date at 00:00 UTC (Unix seconds) |
| `series_id` | the series id, e.g. `YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_10Y` |
| `value` | the value in percent, as a decimal string, as the portal states it |
| `payload` | `{"currency":…,"tenor":…,"basis":…,"compounding":…}`, the series' meaning from `RATE_SERIES` as the run's manifest records it |

The table upserts by date, so a later fetch overwrites a revised day.
`load --force` deletes `curves.db` and rebuilds it from bronze.

## Terms

**ECB.** The Data Portal links the ECB's
[disclaimer and copyright](https://www.ecb.europa.eu/services/disclaimer/html/index.en.html)
page (checked 2026-10-21). It states that "users of this website may
make free use of the information obtained directly from it", provided
that "it must appear accurately and the ECB must be cited as the
source". It adds: "If the information is modified by the user (e.g. by
seasonal adjustment of statistical data or calculation of growth rates)
this must be stated explicitly." The zero curve and the forward rates
the engine derives are such a modification: they are wealthdb's
figures, computed from the ECB's. The page sets no limit on automated
access.

**SNB.** The data portal links the SNB's
[copyright](https://www.snb.ch/en/srv/disclaimer_copyright) page
(checked 2026-10-21). It states that the SNB's "information and data may
be saved, translated (with reference to the source), transmitted or used
in other ways, for non-commercial purposes, compatible with the purpose
of such information or data". The data portal's API help adds that the
data endpoints "should not be used to regularly check the availability
of new data (polling)", and that the SNB may block an address whose use
disrupts the portal. A run therefore asks the latest-update endpoint
before the data, and `login --check` asks that endpoint alone.

## Caveats

- **Business days only.** There are no weekend or holiday rows. The
  yield report's curve window is in
  [YIELD.md §4](../../wealthdb/docs/YIELD.md#4-benchmark-income-and-dividends).
- **No CHF tenor under a year.** The SNB states the Confederation's
  spot rates from 1Y. A franc curve's benchmark is its shortest tenor
  the config names, 1Y when it names it. The income projection then
  resets a floating franc holding on the one-year forward rate, not a
  three-month one.
- **Gaps in the franc's history.** Before 1998 the SNB holds about one
  rate a week. Its 30Y series starts on 1997-12-29, and some long tenors
  have no rate on some days.
