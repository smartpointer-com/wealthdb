# fred

Part of the **wealthdb** suite — see [the architecture overview](../../DESIGN.md)
for the bronze → silver → gold model and [collectors/README.md](../README.md)
for shared collector conventions.

A collector for **historic foreign-exchange rates** from the US Federal
Reserve's **H.10** release, pulled via the [FRED API](https://fred.stlouisfed.org/docs/api/).
It fills the gap left by `ubs-psn`, which supplies fresh (almost) daily
FX rates but no deep history: FRED gives **USD-centric daily reference
rates back to 1971** (1999 for EUR). USD-native — no triangulation
through EUR — so USD-base deployments need no conversion.

## Tools

| Script | Purpose |
| --- | --- |
| [`download.py`](download.py) | Fetch each currency's FRED series for the requested window into bronze (raw JSON + a `run.json` manifest). Read-only. |
| [`load.py`](load.py) | Parse bronze into the silver `fx_rates` table. Rates upsert by date (so a re-fetch overwrites FRED revisions); already-loaded runs are skipped. |
| [`prune.py`](prune.py) | Reclaim bronze disk: delete non-complete (crashed / in-flight) run dirs. Complete dumps are never touched. |

There is **no `login`** — FRED authenticates with an API key, not a
session.

## Currencies

The default series cover the major reserve/reference currencies against
USD; add or trim `FX_SERIES` to match your own holdings. Each FRED
series is one currency vs USD. The stored `(base, quote)` follows the
canonical convention — `(base, quote, mid)` means "**1 quote = mid
base**" — the same direction `ubs-psn` uses:

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

To add a currency, add its FRED H.10 series (e.g. `DEXSDUS` SEK,
`DEXNOUS` NOK, `DEXDNUS` DKK, `DEXUSNZ` NZD, `DEXSIUS` SGD, `DEXKOUS`
KRW, `DEXINUS` INR, `DEXSFUS` ZAR — verify the quote direction) to
`FX_SERIES` in [download.py](download.py).

## Usage

```sh
./fred download                       # last ~90 days (default)
./fred download --lookback all        # full history (1971→, per series)
./fred download --since 2010-01-01    # explicit backfill start
./fred load
./fred prune --dry-run                # preview reclaimable bronze
```

`download` accepts the shared `--since` / `--until` / `--lookback`
window flags; FRED clamps to each series' own start date. The directory
overrides (`--secrets-dir` / `--data-dir` / `--silver-db`) follow the
[shared contract](../README.md#anatomy-of-a-collector).

Each `download` mints one UTC-timestamped bronze run dir holding a
`run.json` manifest plus one `<series_id>.json` per fetched series. The
manifest carries a `status` field: `"in-progress"` while the walk runs,
atomically overwritten with `"complete"` at the end. A crashed walk thus
leaves `status: "in-progress"`, which `load` skips (no partial rates leak
into silver) and `prune` reclaims. fred writes no debug artefacts, so the
uniform `--debug` flag exists for cross-collector consistency but
currently gates nothing.

### Reclaiming disk

```sh
./fred prune --dry-run   # print the deletion plan, delete nothing
./fred prune             # delete it
```

`prune` removes whole non-complete run dirs across the bronze tree — a
walk that crashed before writing a terminal `run.json`, or one that
carries `status: "in-progress"`. A complete dump is never touched: every
file in it (`run.json` and each `<series_id>.json`) is a `load` input, so
fred nominates no debug artefacts to reclaim and silver stays reproducible
from bronze alone. Deleting a non-complete dump surfaces on the next
`load --force` rebuild. `prune` runs host-side like `load`, and an
in-flight guard (`--min-age-hours`, default 1, keyed on the newest write
in the dir) keeps it from removing a long `--lookback all` backfill that
is still running. An unreadable or corrupt `run.json` is left alone, as is
the silver `fred.db` and anything else at the bronze root that is not a
timestamped run dir.

**Credentials:** put your key in `~/.secrets/fred.env` as
`FRED_API_KEY=...` (the wrapper sources it automatically). Get a free key
at <https://fred.stlouisfed.org/docs/api/api_key.html>.

**API version:** this uses FRED's classic ("v1") endpoint
`https://api.stlouisfed.org/fred/series/observations` with the `api_key`
query parameter. The newer v2 API uses `Authorization: Bearer` auth and
offers no advantage for this read-only time-series fetch, so v1 is the
default; `--base-url` is overridable if that ever changes.

## Silver schema

One row per `(observation date, base, quote)` in `fx_rates`:

| column | meaning |
| --- | --- |
| `snapshot_at` | observation date at 00:00 UTC (Unix seconds) |
| `base_currency_iso` / `quote_currency_iso` | `(base, quote, mid)` means **1 unit of quote = mid units of base** (mid = base per quote — the canonical `FxRateChange` convention, same as `ubs-psn`'s `fx_rates`) |
| `mid` | the rate, as a decimal string (FRED H.10 are mid reference rates; no bid/ask) |
| `payload` | `{"series_id":…,"value":…}` |

## Gold FX precedence

`fred` is best treated as a **historic / fallback** FX source: it reaches
back to 1971 but its weekly refresh lags the last few days (see caveats),
so wherever a deployment also has a fresher daily source, that source
should win on the days they overlap and `fred` should fill the rest.

How the winner is chosen is **not hard-coded** — it's the per-source
**`fx_priority`** field in `wealthdb.cfg`. For each conversion the gold FX
resolver (`internal/gold/fx.go`) picks, per day, the rate from the
highest-priority source that covers that day, falling back to the next.
Lower number = higher priority; absent/null = lowest; ties broken by the
order sources are listed in the config. Which sources exist and how
they rank is entirely up to the deployment — `fred` carries no built-in
priority.

For example, a deployment whose daily rates come from `ubs-psn` would give
`ubs` a lower number than `fred` so the bank's same-day rate wins and
`fred` backfills the history:

```jsonc
"silver_sources": [
  { "id": "ubs",  "kind": "ubs",  /* … */ "fx_priority": 0 },
  { "id": "fred", "kind": "fred", "path": "$XDG_DATA_HOME/wealthdb/fred/fred.db", "fx_priority": 1 }
]
```

A deployment with no daily FX source at all can just add `fred` and leave
priorities unset — `fred` then supplies every rate. After adding the
source, rebuild the `wealthdb` binary (the `fred` adapter is compiled in)
and run `wealthdb load fred`.

## Caveats

- **Weekly refresh.** The Fed discontinued the *daily* H.10 update in
  2009; FRED now refreshes the daily series once a week, so the most
  recent few days lag (today's run topped out at ~9 days ago). Use
  `ubs-psn` for the fresh tail; `fred` for history.
- **Business days only.** No weekends/holidays (FRED emits `.`, which the
  loader skips). The gold FX layer forward-fills.
- **Fixing.** FRED = noon New York; `ubs-psn` = UBS's own; ECB = 16:00
  CET. They differ intraday — gold shouldn't blend sources within a day.
