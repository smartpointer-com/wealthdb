# cointracking

A read-only scraper for [cointracking.info](https://cointracking.info)
— the aggregator portal that tracks a full crypto portfolio
and transaction history across centralised exchanges + on-chain
wallets. **No first-order crypto integration** lives in wealthdb;
cointracking.info is the single source of truth for crypto, the
same way the per-bank collectors are for fiat.

cointracking.info has no public API. The collector replays the
browser flow (TOTP 2FA on first login; the device-trust cookie
persists for years afterwards) by driving headless Firefox via
Playwright.

Part of the **wealthdb** suite — see
[the architecture overview](../../DESIGN.md) for the bronze →
silver → gold model and [collectors/README.md](../README.md) for
shared collector conventions.

## Status

| Verb | Status | Notes |
| --- | --- | --- |
| `login`    | implemented | Headless Playwright Firefox, CLI-MFA on stdin, persistent profile. |
| `download` | implemented | Per-portfolio SPA loop: 19-column trade CSV + balance CSV + daily-overview CSV per portfolio. |
| `load`     | implemented | DuckDB silver, aggregate-then-window holdings replay with incremental upsert + balance reconciliation + portfolio_prices ingest (per-portfolio quote currency). |
| `fetch-prices` | implemented | USDT-denominated price backfill from Binance public spot (no key, no signup). 1000-day chunked klines, polite rate-limited. Stablecoins emit synthetic 1.0. |
| `prune`    | implemented | Reclaim bronze disk — deletes whole non-complete dumps (crashed / in-progress walks) and strips `screenshots/` (the `download --debug` captures) from complete dumps. A complete dump's load inputs are left intact. |
| `explore`  | implemented | Discovery harness (Camoufox + VNC + HAR + trace + click log). Kept around for re-discovery if cointracking changes their UI. |

The device-trust cookie is multi-year, so once
`login` has been run once the collector slots into
`wealthdb-nightly` like the rest — one MFA prompt every few years,
otherwise unattended.

## Operational quick start

```sh
# 1. Build the image (~3-5 min first time; the base-camoufox image
#    holds the pre-fetched Firefox so this step is fast on a warm cache).
./cointracking build

# 2. Drop credentials into the env file. chmod 0600 enforced by login.py.
#    cat > ~/.secrets/cointracking.env <<'EOF'
#    COINTRACKING_USERNAME=your-cointracking-email
#    COINTRACKING_PASSWORD=your-cointracking-password
#    EOF

# 3. Mint the session. Headless Firefox; prompts for the 6-digit TOTP
#    code on stdin once, ticks "Don't ask again" for the multi-year
#    device-trust cookie. Subsequent runs short-circuit without firing
#    a 2FA push.
./cointracking login

# 4. Pull a fresh bronze dump (a few seconds per linked portfolio).
./cointracking download

# 5. Ingest into the DuckDB silver. Computes daily holdings from the
#    full transaction history via the aggregate-then-window replay;
#    only rewrites positions_daily from the first deviation day
#    onwards. Parses overview.csv into portfolio_prices (one row per
#    [day, portfolio, coin, quote_currency] — CT-derived, used so the
#    gold layer can match CT's per-portfolio totals byte-exactly).
#    Reconciles final balances vs the balance CSV per portfolio.
#    The post-ingest USD price fill runs by default: it pulls missing
#    USDT-denominated prices from Binance at the end (rolls
#    fetch-prices --missing in). Pass --no-fetch-prices to skip it.
./cointracking load

# 6. Cron / launchd: a nightly `./cointracking download && ./cointracking load`
#    is unattended for the multi-year lifetime of the device-trust cookie.
```

Standalone USD-price tools:

```sh
./cointracking fetch-prices --missing   # fill gaps (e.g. after load --no-fetch-prices)
./cointracking fetch-prices             # full re-fetch (corruption recovery)
```

`fetch-prices` reads positions_daily to know which (coin, date) pairs
matter — every coin held in any portfolio on at least one day — and
fetches USDT-denominated daily closes into `coin_prices` via
Binance's `/api/v3/klines?symbol=<TICKER>USDT&interval=1d` endpoint.
The CT-ticker → Binance-symbol mapping is built lazily from
`/api/v3/exchangeInfo` (filter for USDT pairs) and cached in
`coin_mapping`. Both `fetch-prices --missing` and `load`'s default price fill
always re-fetch the latest priced day so the previous run's intraday
snapshot gets upgraded to the close price.

Stablecoins (USDT, USDC, DAI, …) emit synthetic 1.0 USD prices —
they can't be self-quoted on Binance, and for portfolio valuation
the small USDT/USD basis is silently absorbed (the gold layer can
apply a precise FX correction later if ever needed).

No API key required. Optional `BINANCE_API_KEY` env activates
authenticated tiers for higher rate-limit ceilings; unused on the
public endpoint at api.binance.com.

**Coverage gap (small in practice):** Binance retains pair
metadata in `exchangeInfo` long after it purges the kline history
for a delisted coin (e.g. some privacy coins and older altcoins
dropped from the exchange), so a mapping can resolve to a symbol
with no price data. Impact is bounded by the architecture:
`coin_prices` is only read by gold for non-USD portfolios (USD
portfolios use `portfolio_prices` directly), and those non-USD
portfolios in practice hold mostly major coins that Binance covers
fully. A Yahoo Finance secondary-source fallback is parked as an
"if it ever bites" follow-up.

Probe the session without firing a 2FA push:

```sh
./cointracking login --check    # exits 0 if session is valid, 1 if not
./cointracking download --dry-run    # walks navigation, skips exports
./cointracking load --replay-only    # re-runs the holdings replay only
./cointracking load --force          # delete silver + rebuild from all bronze
```

## Re-discovery: when cointracking changes their UI

The `explore` subcommand is kept around for the next time
cointracking changes a selector that the headless flow depends on
(e.g. the "Extended with additional columns" mode dropdown, or the
"Don't ask again" checkbox). It launches Camoufox in the
container's Xvfb display and exposes a VNC port for driving the
live browser interactively; meanwhile it records HAR, Playwright
trace, click log, and any blob downloads:

```sh
./cointracking explore --fresh      # --fresh wipes the profile so 2FA is forced
#  explore: VNC ready on 127.0.0.1:<port>
#  explore: password (single-use):  <16 hex chars>
#  explore: tunnel from your laptop with
#    ssh -L <port>:127.0.0.1:<port> <host>
#  explore: then on the laptop:
#    open vnc://localhost:<port>
```

Artefacts land under `$HOME/.cache/cointracking-debug/<UTC-ts>/`
(network.jsonl + trace-chunks/ + clicks.jsonl + downloads/).
Close the browser window OR Ctrl-C the container — either path
flushes everything to disk.

Override host mounts via env: `COINTRACKING_SECRETS_DIR`,
`COINTRACKING_DATA_DIR`, `COINTRACKING_DEBUG_DIR`.

## Reclaiming disk

`prune` deletes, across every timestamped run dir under the data dir,
whole run dirs that are **not complete dumps** — a walk that crashed
before finalising (`run.json` absent, or carrying the
`status: "in-progress"` marker it drops at run-dir creation). A finished
dump atomically overwrites that marker with `status: "complete"`, so
its `run.json` + `cu_<id>/{trades,balance,overview}.csv.zst` are load
inputs and stay untouched. The one thing it does strip from a complete
dump is `screenshots/` — the `download --debug` captures, which `load`
never reads. (The `explore` harness records its HAR/trace/click logs to an
external `/debug` mount, never into a bronze run dir, so prune never sees
them.)

```sh
./cointracking prune --dry-run    # print the plan, delete nothing
./cointracking prune              # reclaim non-complete dumps
./cointracking prune --min-age-hours 6   # protect anything touched in the last 6h
```

An in-flight guard keeps a download that is still writing safe: a
non-complete dir is a deletion candidate only once nothing under it has
been written for `--min-age-hours` (default 1), keyed on the newest
mtime in the dir, so a multi-portfolio walk whose slug is old but whose
files are fresh is never mistaken for abandoned. `run.json` that is
unreadable or corrupt is left alone, and non-run-dir entries at the data
root (`known_portfolios.json`, the silver `cointracking.duckdb`) are
never touched. A statusless `run.json` from a dump that predates the
status lifecycle is treated as complete (the walk historically wrote it
only once, at the end) and kept. Deleting a non-complete dump does not
touch silver — rows already loaded from it persist until the next
`load --force` rebuild.

The persistent scrape-union cache and the DuckDB silver both live at the
data root alongside the run dirs; `prune` matches only `<UTC-ts>/`
timestamp dirs, so neither is ever in scope.

### Compressing the pre-compression backlog

`download` zstd-compresses every CSV export as it lands, and `load`
reads `.csv.zst` and plain `.csv` alike (DuckDB decompresses natively).
Run dirs written before compression existed can be converted once with
the `recompress` verb, which replaces each plain `cu_<id>/*.csv` inside
a **complete** dump with a compressed twin — the original is unlinked
only after the twin has been decompressed and sha256-verified against
it, and an interrupted sweep is safe to re-run. Unlike `prune` this
rewrites load inputs, so it is strictly manual: never schedule it,
review the plan first, and verify afterwards with `load --force`
(silver must come out identical).

```sh
./cointracking recompress --dry-run   # print the sweep plan, rewrite nothing
./cointracking recompress             # convert complete dumps, with byte accounting
./cointracking load --force           # convergence check: silver must be unchanged
```

## Read-only

See [CLAUDE.md](CLAUDE.md). cointracking.info exposes mutation
surfaces (add transactions by hand, edit address book entries,
delete imports, change account settings). This toolkit is
**read-only** — it only navigates, filters, and exports.
