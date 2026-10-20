# fred

Part of the **wealthdb** suite — see [the architecture overview](../../DESIGN.md)
for the bronze → silver → gold model and [collectors/README.md](../README.md)
for shared collector conventions.

A collector for two families of public daily series from the US
Federal Reserve, pulled via the [FRED API](https://fred.stlouisfed.org/docs/api/):

- **Historic foreign-exchange rates** from the **H.10** release. FRED
  gives **USD-centric daily reference rates back to 1971** (1999 for
  EUR). A daily account source such as `ubs-psn` supplies fresh rates
  but no deep history; `fred` fills that history.
- **US Treasury yields** from the **H.15** release: the yield curve from
  one month to thirty years. The `curves` map of `wealthdb.cfg`'s
  `yield` block ([config fields](../../wealthdb/docs/DESIGN.md#51-fields))
  picks one of these series per tenor for the dollar's curve. Gold
  derives forward rates from that curve.

## Tools

| Script | Purpose |
| --- | --- |
| [`download.py`](download.py) | Fetch each FRED series of both families for the requested window into bronze (raw JSON + a `run.json` manifest). Read-only. |
| [`load.py`](load.py) | Parse bronze into the silver `fx_rates` and `rates` tables. Rows upsert by date (so a re-fetch overwrites FRED revisions); already-loaded runs are skipped. |
| [`prune.py`](prune.py) | Reclaim bronze disk: delete non-complete (crashed) run dirs and `--debug` traces. A complete dump's `load` inputs are never touched. |

FRED authenticates with an API key, not a session, so a bare `login` is
a no-op. `login --check` is the credential probe. It resolves the key
and asks FRED for a single observation, so a key that is present but
rejected fails there rather than at the next `download`. It writes
nothing. `download --dry-run` fetches with the key and writes no bronze.
Neither one mints a session or raises a challenge.

## Series

A download fetches every series of two families. Each family is a
built-in map in [download.py](download.py). `download --series` fetches
a comma-separated subset, and one list may mix the families.

### FX rates

`FX_SERIES` covers the major reserve and reference currencies against
USD. Each FRED series is one currency vs USD, so a USD-base deployment
needs no triangulation. The stored `(base, quote)` follows the canonical
convention, the same direction `ubs-psn` uses: `(base, quote, mid)`
means "**1 quote = mid base**".

| FRED series | base / quote | Meaning |
| --- | --- | --- |
| `DEXSZUS` | CHF / USD | Swiss Francs per USD (1 USD = mid CHF) |
| `DEXUSEU` | USD / EUR | USD per Euro (1 EUR = mid USD) |
| `DEXJPUS` | JPY / USD | Yen per USD |
| `DEXUSUK` | USD / GBP | USD per Pound |
| `DEXCAUS` | CAD / USD | Canadian Dollars per USD |
| `DEXUSAL` | USD / AUD | USD per Australian Dollar |
| `DEXCHUS` | CNY / USD | Chinese Yuan per USD |
| `DEXHKUS` | HKD / USD | Hong Kong Dollars per USD |

To add a currency, add its FRED H.10 series to `FX_SERIES`, and check
its quote direction. Examples: `DEXSDUS` SEK, `DEXNOUS` NOK, `DEXDNUS`
DKK, `DEXUSNZ` NZD, `DEXSIUS` SGD, `DEXKOUS` KRW, `DEXINUS` INR,
`DEXSFUS` ZAR.

### Rates

`RATE_SERIES` covers the US Treasury yield curve. Values are in percent,
as FRED serves them. Each series has a currency, a tenor and a basis:

- `constant_maturity` is a Treasury par yield at a fixed maturity. It is
  bond-equivalent and compounds semiannually.
- `discount` is a bill's secondary-market rate on a discount basis.

| FRED series | currency | tenor | basis |
| --- | --- | --- | --- |
| `DGS1MO` | USD | 1M | constant_maturity |
| `DGS3MO` | USD | 3M | constant_maturity |
| `DGS6MO` | USD | 6M | constant_maturity |
| `DGS1` | USD | 1Y | constant_maturity |
| `DGS2` | USD | 2Y | constant_maturity |
| `DGS3` | USD | 3Y | constant_maturity |
| `DGS5` | USD | 5Y | constant_maturity |
| `DGS7` | USD | 7Y | constant_maturity |
| `DGS10` | USD | 10Y | constant_maturity |
| `DGS20` | USD | 20Y | constant_maturity |
| `DGS30` | USD | 30Y | constant_maturity |
| `DTB3` | USD | 3M | discount |

To add a rate, add its FRED series to `RATE_SERIES` with its currency,
tenor and basis.

## Usage

```sh
./fred download                       # last ~90 days (default)
./fred download --lookback 1950-01-01 # each series' whole history
./fred download --lookback all        # the last 30 years
./fred download --lookback 2010-01-01 # explicit backfill start
./fred download --series DEXSZUS,DEXUSEU
./fred download --series DGS2,DGS10,DTB3
./fred load
```

`download` honours the shared
[`--lookback` window](../README.md#collector-cli-conventions). FRED
clamps it to each series' own start date. The directory overrides follow
the
[shared contract](../README.md#the-wrapper--the-wealthdb-collect-contract).

Each `download` writes one UTC-timestamped bronze run dir under
`$XDG_DATA_HOME/wealthdb/fred/`. It holds a `run.json` manifest and one
`<series_id>.json` per fetched series. The manifest records what each
series is (an FX pair's base and quote, a rate's currency, tenor and
basis), so `load` rebuilds silver from bronze alone. It follows the
[shared `status` lifecycle](../README.md#debug-artefacts-run-status-and-pruning-bronze),
so `load` never ingests a crashed walk. The silver DB is `fred.db`,
beside the run dirs.

`download --debug` adds `screenshots/http-trace.jsonl` to the run dir.
Each line records one FRED request: status, timing, size and any
rate-limit headers. Bodies are not duplicated. The `api_key` query
parameter is redacted, never persisted. `load` ignores the trace and
`prune` reclaims it. A `--dry-run` writes no run dir, so it captures
nothing.

### Reclaiming disk

```sh
./fred prune --dry-run   # print the deletion plan, delete nothing
./fred prune             # delete it
```

`prune` works within the
[shared safety envelope](../README.md#debug-artefacts-run-status-and-pruning-bronze).
It removes whole non-complete run dirs and the `screenshots/` trace that
`--debug` leaves in a complete one. Every other file in a complete dump
(`run.json` and each `<series_id>.json`) is a `load` input and stays.
The in-flight guard (`--min-age-hours`) protects a long backfill that
is still running.

**Credentials.** The API key lives in `~/.secrets/fred.env` as
`FRED_API_KEY=...`, and the wrapper sources it. `download.py --api-key`
takes the key directly, with `FRED_API_KEY` as the fallback. A free key
comes from <https://fred.stlouisfed.org/docs/api/api_key.html>.

**API version.** The collector uses FRED's classic ("v1") endpoint
`https://api.stlouisfed.org/fred/series/observations` with the `api_key`
query parameter. The newer v2 API uses `Authorization: Bearer` auth and
offers no advantage for a read-only time-series fetch. `--base-url`
points the download at another endpoint.

## Silver schema

One row per `(observation date, base, quote)` in `fx_rates`:

| column | meaning |
| --- | --- |
| `snapshot_at` | observation date at 00:00 UTC (Unix seconds) |
| `base_currency_iso` / `quote_currency_iso` | `(base, quote, mid)` means **1 unit of quote = mid units of base** (mid = base per quote — the canonical `FxRateChange` convention, same as `ubs-psn`'s `fx_rates`) |
| `mid` | the rate, as a decimal string (FRED H.10 are mid reference rates; no bid/ask) |
| `payload` | `{"series_id":…,"value":…}` |

One row per `(observation date, series)` in `rates`:

| column | meaning |
| --- | --- |
| `as_of` | observation date at 00:00 UTC (Unix seconds) |
| `series_id` | the FRED series ID, e.g. `DGS10` |
| `value` | the value in percent, as a decimal string |
| `payload` | `{"currency":…,"tenor":…,"basis":…}`, the series' meaning from `RATE_SERIES` as the run's manifest records it |

Both tables upsert by date, so a later fetch overwrites a revised day.
`load --force` deletes `fred.db` and rebuilds both tables from bronze.

## Gold FX precedence

`fred` works best as a **historic / fallback** FX source. It reaches back
to 1971, but its weekly refresh lags the last few days (see caveats).
Where a deployment also has a fresher daily source, that source should
win on the days they overlap, and `fred` should fill the rest.

The per-source **`fx_priority`** field in `wealthdb.cfg` sets the winner.
Lower number = higher priority; absent/null = lowest. `fred` carries no
built-in priority. Gold's FX views apply the ranking per UTC day; see
[wealthdb/docs/DESIGN.md §13.2](../../wealthdb/docs/DESIGN.md#132-fx-rate-precedence-among-silvers).

For example, a deployment whose daily rates come from `ubs-psn` gives
`ubs` a lower number than `fred`. The bank's same-day rate then wins, and
`fred` backfills the history:

```jsonc
"silver_sources": [
  { "id": "ubs",  "kind": "ubs",  /* … */ "fx_priority": 0 },
  { "id": "fred", "kind": "fred", "path": "$XDG_DATA_HOME/wealthdb/fred/fred.db", "fx_priority": 1 }
]
```

A deployment with no daily FX source can add `fred` and leave
priorities unset; `fred` then supplies every rate. After adding the
source, `wealthdb load fred` merges it into gold.

## Caveats

- **Weekly refresh.** FRED refreshes the daily H.10 series once a week,
  so the tail lags by up to about 9 days. A daily source such as
  `ubs-psn` covers the fresh tail; `fred` covers history.
- **Business days only.** There are no weekend or holiday rows. FRED
  emits `.` for a missing day, and the loader skips it. Gold converts
  with the latest rate at or before each day.
- **Fixing.** FRED's H.10 rates are noon New York; `ubs-psn` uses UBS's own
  fixing. They differ intraday, so gold takes one source per day and
  never blends them.
