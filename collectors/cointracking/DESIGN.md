# cointracking — design notes

This collector mirrors cointracking.info's browser flow: it drives a
real logged-in session, exports the CSVs cointracking already
produces, and loads them into silver + gold. The pipeline runs
discovery → login → download → silver load → gold; each stage is
described below. `explore` is the discovery harness the login and
download flows were authored from, kept for re-discovery when
cointracking changes its UI.

## Why a separate collector

cointracking.info is the aggregator-of-record for all crypto
(centralised exchange accounts + on-chain wallets). Mirroring
the per-bank collector model — one collector per source — gives us:

- A clean silver schema isolated to crypto, so the gold-layer
  reconciliation between fiat and crypto stays explicit.
- Independent re-runs (a crypto-only refresh doesn't have to touch
  the bank collectors and vice versa).
- An obvious extension point if we ever do add a first-order source
  alongside cointracking.

The alternative — directly integrating each exchange API + every
on-chain wallet — was rejected: cointracking already does that
heavy lifting and serves as the canonical view.

## Discovery — the `explore` harness

`explore.py` launches Camoufox in the container's Xvfb display,
opens cointracking.info, and records the live session via
three channels:

1. **HAR** — full network capture via Playwright's `record_har_path`.
   The primary artefact for finding internal REST endpoints. Every
   fetch / XHR / fetch-API / form-submit appears with headers and
   bodies. Playwright writes it whole, so the harness rewrites it
   with its credentials, cookies and query-string tokens out once
   the close has flushed it.
2. **Playwright trace** — opt-in with `--trace`:
   `tracing.start(screenshots=True, snapshots=True, sources=True)`
   captures DOM + screenshot snapshots at every navigation. Open
   with `playwright show-trace`. Off by default because the pinned
   tracer crashes the Camoufox Firefox build, and because a trace
   cannot be redacted after the fact: its DOM snapshots store every
   input's value, a hand-typed password included.
3. **Click log (`clicks.jsonl`)** — a custom JS init script
   listens to every `click` event in the DOM and emits one JSON
   object per click to the page's console; the Python side
   harvests it via `page.on('console')`. Necessary because VNC
   mouse clicks bypass Playwright's API and don't show up in the
   trace as actions.

Stopping: when the last browser window is closed (Camoufox's
persistent context fires `close`) or hits Ctrl-C. A `--max-duration`
safety net (default 1h) prevents a forgotten session from
recording forever.

Artefacts land under `/debug/<UTC-ts>/` (mounted from
`$HOME/.cache/wealthdb/debug/cointracking` on the host), explicitly NOT
under `/data`, so the bronze/silver tree stays clean.

The persistent Camoufox profile dir at
`/secrets/cointracking-profile` carries the post-MFA session cookie
between runs, so subsequent `explore` invocations skip the 2FA
challenge.

## Login — headless Playwright Firefox, CLI-MFA

Pure HTTP was attempted first and abandoned. The explore traces
showed POST 1's `password_login` was a 32-char **session-encrypted
blob** (mixed-case ASCII with `#`/`%`/`^` — not any standard
hash, base64, or hex), computed by JS on /login.php using a
session-issued key. The server accepts `md5(password)` in POST 1
far enough to return the 2FA challenge form (so the trace looked
deceptively similar), but the post-POST-1 session state isn't
fully valid and POST 2 gets bounced back to /login.php with the
fresh unauthenticated form. Reproducing the encryption in Python
would mean reverse-engineering the JS — fragile, breaks on any
cointracking-side tweak.

So `login.py` drives **vanilla Playwright Firefox in headless
mode**. No Camoufox stealth (cointracking doesn't bot-check),
no Xvfb, no VNC, no display. The browser runs the page's JS-side
encryption for free and we keep the contract clean.

Flow:

1. Launch `firefox.launch_persistent_context(user_data_dir=…,
   headless=True)`. The profile dir is `/secrets/cointracking-profile/`
   — the same dir explore.py uses.
2. Navigate to /login.php. If the device-trust cookie from a prior
   run is still valid, the page redirects directly to /dashboard
   and we return early.
3. Fill `username_login` + `password_login` using the same locator
   heuristics explore.py's MutationObserver lands on. Click submit.
4. Wait for `input[name='code_2fa']` to appear.
5. Read a 6-digit code from stdin (the CLI-MFA prompt).
6. Fill `code_2fa`, tick `#dont_ask_again` (plants the multi-year
   `ctfa<user_id>` device-trust cookie), click submit.
7. Wait for navigation to `/dashboard*`.
8. `context.close()` writes the updated cookies + localStorage
   back to the persistent profile.

`--check` opens the profile, GETs /dashboard, checks the URL +
absence of login form. Exits 0/1, no credentials posted, no 2FA
push — safe for cron healthchecks.

Renewal short-circuit (step 2) covers the cron use case: every
nightly run hits /login.php, sees the redirect to /dashboard, and
exits — no 2FA push. The `ctfa<user_id>` cookie is a multi-year
value, so this path stays live until the next genuine session-
cookie rotation.

The Camoufox base image is still used because `explore.py` needs
it. Adding vanilla Firefox via `playwright install firefox` in
the Dockerfile costs ~80 MB; the two browsers coexist cleanly
(separate binary paths, no API conflict).

## Download — headless Playwright Firefox, per-portfolio blob loop

Same browser stack as login.py (vanilla Firefox, `headless=True`,
shared `/secrets/cointracking-profile/`). The login session
carries over automatically.

**Always full history.** CoinTracking serves the full history
quickly enough that there's no payoff in windowing — every run
pulls the complete transaction list per portfolio.

**Portfolio discovery:**
- Master account ID: from the `ctfa<id>` session cookie name.
- Linked-account IDs + display names: from `<a href*="change_user=N">`
  anchors on /enter_coins.php (the in-page portfolio switcher
  shows the OTHER portfolios; the current/master is implicit).
- Union of the two = full list of all portfolios.
- The live scrape is merged with a persistent `known_portfolios.json`
  union cache at the bronze root, so a transient miss in CT's flaky
  linked-user list doesn't drop a portfolio from the download set.

**Per-portfolio loop** (in `download_portfolio()`):
1. `GET /enter_coins.php?change_user=<id>` activates the
   portfolio server-side.
2. Set `<select name="extended">` value="2" → "Extended with
   additional columns". Its CSV export carries 13 columns:
   Type, Buy, Cur., Sell, Cur., Fee, Cur., Exchange, Group,
   Comment, Date, LPN, Tx-ID. Wait for networkidle.
3. Click `button:has-text("Export")` → wait for menu →
   click `text("CSV (Full Export)")`. `page.expect_download()` +
   `save_as()` captures the blob.
4. `GET /balance_by_exchange.php?change_user=<id>` activates the
   portfolio for the balance view.
5. Click Export → exact-text "CSV". Save the blob.
6. `GET /overview.php?change_user=<id>` → Export → CSV → "Comma
   separated" saves the Daily Balance overview (wide-form per-day
   holdings; load.py parses it into portfolio_prices).

Files land in `<bronze-dir>/<UTC-ts>/cu_<id>/{trades,balance,overview}.csv.zst`.

**Bronze compression.** Each CSV export is zstd-compressed in place as
it lands (`collectorkit.compress.compress_file`: atomic tmp+rename,
decompress-and-sha256-verify before the plain file is unlinked, mtime
carried over). CSV-shaped bronze compresses to a small fraction of its
raw size, and this collector is the only nightly-growing bronze tree,
so at-rest weight and backup traffic shrink by roughly an order of
magnitude. The read side is free: DuckDB's `read_csv_auto` decompresses
`.csv.zst` (and `.csv.gz`) natively by extension, so `load` never
materialises an uncompressed file — it only resolves the on-disk
variant via `compress.resolve_variant` (plain `.csv` wins when both
forms coexist). Compression failures downgrade to a warning and leave
the plain CSV in place: every reader accepts both forms, so a
half-adopted tree is a valid tree, not an error state. Pre-compression
run dirs are converted by the manual `recompress` verb (thin wrapper
over `collectorkit.recompress`, prune-grade safety envelope,
verify-then-unlink per file, byte accounting; never scheduled) —
after a sweep, a `load --force` rebuild must produce identical silver.

**`run.json` status lifecycle.** At run-dir creation the walk writes
`run.json` carrying `{"status": "in-progress"}` (via
`bronze.atomic_write_json`). After all portfolios are processed it
atomically overwrites `run.json` with the full manifest — the silver
loader's portfolio list + file map — now carrying
`"status": "complete"`. So a run dir is a **complete** dump
(`status == "complete"`), or a **non-complete** one: a crash before the
end leaves either the `in-progress` marker or (if it died before
`mkdir`) no `run.json` at all. `load.discover_bronze_snapshots` skips a
run dir whose `status` is not complete, so a crashed dump never reaches
silver; `prune` reclaims it. A `run.json` with no `status` key predates
this lifecycle and counts as complete to both `load` and `prune`: such a
dump only ever got a `run.json` at the end, so its presence alone means
the walk finished.

`--dry-run` walks the navigation and prints what it would do but
skips every export-button click — no downloads fire, and **no run dir
is materialised at all** (both the `mkdir` and the manifest write are
gated on the non-dry-run path), so there is no `dry-run` shell for
`prune` to reclaim. Use to verify portfolio discovery + selector
correctness without burning bandwidth.

`--debug` opts a run into retaining bronze-resident diagnostic artefacts:
the DOM + screenshot of two pages, written to `<run>/screenshots/`. The
first is the portfolio-discovery page — the auth gate, and the step whose
late-JS switcher anchors are flaky enough that the run merges a cached
portfolio list to paper over a short scrape; when that happens, this DOM is
the only record of why, since nothing downstream keeps it. The second is
captured only on the per-portfolio failure path, where the page is still on
whichever export surface raised, so it is the DOM the failing selector was
matched against; capturing all three export surfaces for every portfolio
would bury it. `load` never reads either, and `prune` reclaims
`<run>/screenshots/`. Off by default, and a no-op under `--dry-run`, which
materialises no bronze tree. The discovery harness's own diagnostics stay in
`explore.py`'s external `/debug` mount, never a bronze run dir.

## Load — DuckDB silver with incremental positions_daily upsert

Each `load` invocation iterates the bronze tree, processing every
snapshot not already in `dump_runs`. Per snapshot:

1. **transactions** — fully replaced. Each trade CSV is parsed via
   `read_csv_auto(all_varchar=true)` and projected into the silver
   schema. CoinTracking gives a row no unique id: `Tx-ID` is often
   blank or shared, and many rows share a timestamp. So
   `transaction_external_id` is `cu_<id>:<hash>`, where the hash is
   the first 16 hex digits of a sha256 over the row's columns. The
   2nd, 3rd, … copy of an exact duplicate row gets `:2`, `:3`, ….
   The id does not depend on the CSV's row order. A row keeps its
   id across exports until CoinTracking amends it, and gold's order
   (`occurred_at`, then id) is total and the same on every load.
   `Group` and `Tx-ID` land in `trade_group` and `tx_id` as printed.
   `Date` lands in `occurred_local` as printed, and `occurred_at`
   holds it converted to UTC (see "Timezone" below).

2. **portfolios + wallets** — upserted from the run.json manifest +
   the loaded transactions' `Exchange` column. Each portfolio row
   records the timezone its `Date` column was read in.

3. **positions_daily — incremental upsert.** The full new time
   series is computed via the aggregate-then-window replay; we
   then `FULL OUTER JOIN` new vs existing and find the first
   `as_of_date` where they differ (changed amount, new row, or
   removed row — `IS DISTINCT FROM` catches all three). Only rows
   from that cutoff date onwards are deleted + re-inserted. Older
   rows keep their original `snapshot_at`. This means the gold
   layer (which uses `snapshot_at` to detect changes) only ever
   re-processes the actually-changed slice — important because
   transaction edits in CoinTracking can land arbitrarily far
   in the past.

4. **reconciliation** — for each portfolio, the final balance
   per `(wallet, instrument)` from positions_daily is compared
   against the balance.csv. Discrepancies bigger than
   ±10⁻⁸ (the 8-decimal export precision) are logged as
   warnings, not failures. Coverage gaps (CT has it, replay
   doesn't) and unreported non-zero positions (replay has it,
   CT doesn't) both warn. Zero-balance positions only in the
   replay are fine — CT's UI filters them.

5. **dump_runs** — the snapshot is recorded with the cutoff date
   + rows-written count in `payload`, so re-runs can tell which
   snapshot moved which slice of history. The row also records
   the schema version the snapshot was loaded under.

After the loop, the load checks the newest loaded snapshot, whose
rows `transactions` holds. It ingests that snapshot again when one
of two things has changed since it was loaded:

- the schema version, because rows loaded under an older one hold
  NULL in the columns a newer one adds;
- a portfolio's configured timezone, because its `occurred_at` was
  converted from the old zone.

So a migration or a new timezone setting takes effect at the next
`load`, with no new download and no `--force`. A portfolio missing
from the newest export keeps its rows as last loaded.

`--replay-only` skips the bronze ingest and re-runs the
positions_daily incremental upsert against whatever's already
in `transactions`. Useful when iterating on the type-handler
rules.

`--force` deletes the silver DB and rebuilds it from all bronze (the
fleet-wide meaning). Useful for re-validating after a load.py change
without manually clearing the silver.

### Timezone

CoinTracking writes the trade export's `Date` in the timezone set
on the portfolio's account, with no offset. The export does not say
which zone that is. So the zone is a per-deployment setting:

- `load --timezone cu_<id>=<zone>`, repeated per portfolio, names
  each portfolio's IANA zone.
- Without the flag, `load` reads the same items from
  `COINTRACKING_TIMEZONES`, separated by spaces. The wrapper sources
  `$XDG_CONFIG_HOME/cointracking.cfg` (default
  `~/.config/cointracking.cfg`) and forwards that variable.
- A portfolio named in neither is read as UTC.
- An item of another shape, or a zone DuckDB does not know, stops
  the load before it starts. A typo must not pass for UTC.

`occurred_at` is the `Date` wall time read in the zone and converted
to UTC. DuckDB's ICU rules settle the two wall times a daylight-saving
change makes unclear:

- a wall time the change repeats reads as its later occurrence;
- a wall time the change skips reads with the offset in force before
  the change.

The conversion is only as good as the export. CoinTracking's own
imports from exchanges and spreadsheets can leave a row hours off,
whatever the account setting. Silver keeps such rows as exported.
A row stamped at exactly midnight local time, as a date-only import
leaves it, lands on the previous UTC day in a zone east of UTC. A
consumer must not depend on sub-day precision.

A changed zone moves `occurred_at`, so a row near midnight can change
day in `positions_daily` and in gold after its next load. The
transaction id hashes the export's text and does not move.

## Prune — reclaiming bronze disk

`prune.py` is a thin wrapper over the shared
`collectorkit.prune` engine (the one reviewed, unit-tested place that
owns the irreversible `rmtree` of a bronze path). It deletes, across
every `<UTC-ts>/` run dir under the data root, whole run dirs that are
**not complete dumps** — the same non-complete set `load` skips: no
`run.json` (crash before the in-progress marker), or a `run.json` whose
`status` is anything other than `"complete"` (the `in-progress` marker
from a crashed walk). The completeness predicate delegates to
`prune.status_classification` with a legacy fallback of `m is not None`
(a statusless-but-readable manifest is a pre-lifecycle complete dump).

cointracking nominates `debug_subdirs = ("screenshots",)`: that dir is the
one thing a run writes which `load` does not read — the `--debug` captures —
so it is the only thing a complete dump ever gives up. Every other file
(`run.json` and each `cu_<id>/{trades,balance,overview}.csv.zst`; plain
`.csv` in pre-compression dumps) is a `load` input and is never a target.
The discovery harness's traces go to an external `/debug` mount rather than
into a run dir, so prune never sees them.

Safety comes entirely from the shared engine and is identical to every
other collector: a `load` input is never deleted; an unreadable/corrupt
`run.json` is UNKNOWN and skipped; symlinks are never followed; nothing
at the data root that isn't a `<UTC-ts>/` dir is touched (so the
`known_portfolios.json` scrape cache and the `cointracking.duckdb`
silver DB — both of which sit at the root, the silver DB defaulting to
`/data`, the same dir as the bronze root — are out of scope by
construction); an in-flight guard keyed on the newest mtime in the dir
protects a long multi-portfolio walk (`--min-age-hours`, default 1); and
a whole-dir deletion rechecks completeness + quiescence immediately
before the `rmtree`. `prune` runs host-side (like fidelity-web): a pure
file walk needs none of `load`'s in-container deps (DuckDB + the price
clients), and host-side execution bypasses the wrapper's single-writer
guard so `./cointracking prune` can reclaim disk while a `login` /
`download` container is mid-flight (its own age guard protects the
in-flight dump). `entrypoint.sh` keeps a browserless `prune)` arm too,
for a direct `docker run`.

## Page surfaces and endpoints

The cointracking.info surfaces the collector reads:

- **Page surfaces:** /dashboard, /enter_coins.php (trade history),
  /balance_by_exchange.php (current per-wallet holdings),
  /overview.php (Balance By Day — wide-form daily history),
  /export/trades_csv.php (simple CSV endpoint),
  /export/export_html.php (HTML export — strictly inferior, skip).
- **change_user mechanics:** `?change_user=N` query param toggles
  active portfolio on every per-portfolio page. Server-side state
  follows the latest such navigation.
- **Internal REST endpoints:** /ajax/all_current_balance.php
  returns JSON, but with HTML strings embedded — it's the SPA
  render envelope, not clean data. Not worth using.

## Prices — portfolio_prices + coin_prices

Two price tables, populated from different sources:

**`portfolio_prices`** — what CoinTracking reports per portfolio,
scraped from `/overview.php` via the SPA Export →
CSV → Comma separated submenu (a `DataTables buttons-collection`
pattern — the first "CSV" click expands a sub-menu of variants;
we pick comma). One row per (day, portfolio, coin, quote_currency).
Price = `value_in_fiat / amount`; rows where `amount = 0` are
filtered out (the division-by-zero gate). Quote currency is the
portfolio's "main fiat" CT setting and **varies between
portfolios** (e.g. some EUR, some USD); the column
header `<SYM> Value in <FIAT>` carries it.

The master-account view is filtered to a top-N subset of coins
by CT, while linked portfolios show the full per-portfolio set.
The download loop ingests every portfolio's overview; gold-layer
queries against `portfolio_prices` see the union.

**`coin_prices`** — canonical USDT-denominated daily closes per
coin per day, fetched from Binance public spot's `/api/v3/klines`
endpoint. Covers every coin held on any day in any portfolio (the
"held set" from `positions_daily.amount > 0`). The CT-ticker →
Binance-symbol mapping lives in `coin_mapping` and is built lazily
from `/api/v3/exchangeInfo`: filter for pairs with
`quoteAsset=USDT`, key by `baseAsset`. Stored values are
USDT-denominated; the tiny USDT/USD basis is absorbed into the
`price_usd` column (~few bps drift, well within tolerance for
portfolio valuation; the gold layer can apply a precise USDT/USD
FX correction later).

Stablecoins (USDT, USDC, DAI, BUSD, TUSD, …) emit synthetic 1.0
prices because they can't be self-quoted on Binance. Tracked via
a `STABLECOIN_SENTINEL` value in coin_mapping; the kline loop
short-circuits on it.

The `provider` column in `coin_mapping` + `coin_prices.source`
keeps the door open to swapping market-data providers without a
schema migration — `binance.py` is the current implementation;
slotting in a Yahoo Finance secondary source (for delisted-from-
CEX coins) would only need a new client module that exposes the
same `build_mapping` / `ohlcv_historical` interface.

**Three invocation patterns** (`binance.py` is the polite
client; `fetch_prices.py` is the standalone CLI; `load.py` embeds
the same engine):

| Trigger | Mode | Behaviour |
|---|---|---|
| `load` (default) | missing | Fill (held, unpriced) gaps + always re-fetch the latest priced day per coin |
| `fetch-prices --missing` | missing | Same as above; for use after a `load --no-fetch-prices` |
| `fetch-prices` (no flag) | full | Drop every coin_prices row sourced from Binance, re-fetch the full held range. Corruption recovery. |

The "always re-fetch the latest priced day" rule exists because
that row was an intraday snapshot when first written (Binance's
klines include a partial-day kline for the current UTC day; the
next run upgrades it to the daily close once the day closes).

Rate-limit posture: `RATE_LIMIT_DELAY_S = 0.1s` (~600 calls/min)
sits comfortably below Binance's ~1,200/min IP weight ceiling. A
new portfolio's first backfill (one chunked series of ~1,000-day
API calls per held coin, back to its first trade) completes in
well under a minute. Optional `BINANCE_API_KEY` enables authenticated tiers
for higher rate limits — unused on the public endpoint.

**Why coin_prices exists alongside portfolio_prices.** They serve
different roles. `portfolio_prices` is the CT-derived per-portfolio
view — every coin × day in each portfolio's "main fiat" CT
setting, which can be EUR for some portfolios and USD for others.
`coin_prices`
is the cross-source canonical USD reference. For a portfolio
whose CT main-fiat is USD, the gold layer can answer "value of
this position on day X in USD" directly from `portfolio_prices`
— `coin_prices` is not on the read path. For a non-USD portfolio,
`coin_prices` provides the USD reference used to convert
`portfolio_prices` (or equivalently to value `positions_daily`
amounts directly).

### FX rates: Frankfurter / ECB

Non-USD fiat held as cash (EUR, CHF, GBP, …) needs a USD price
too. We get that from `api.frankfurter.app` — a thin wrapper
around the ECB's daily reference rates. Free, no key, no signup.
Inserted into the same `coin_prices` table the crypto path uses,
under `source='frankfurter'`. ECB publishes weekday-only rates;
the client forward-fills across weekends and ECB holidays
(standard FX convention — Friday's close applies through the
weekend until Monday's publish).

### Backfill: first-day-held edge cases

For coins where a portfolio's first purchase pre-dates Binance's
USDT-pair listing date, no kline exists for the gap day.
`backfill_first_day_gaps()` runs after every fetch-prices and
fills each (instrument, gap_date) where a same-instrument
`coin_prices` row exists within 365 days into the future, using
that next-available price. The source tag is the same as the
upstream (`'binance'`) — from the gold layer's POV it's
indistinguishable from a real kline.

### Coverage gap

After fetching + FX + backfill, the remaining USD-derivability
gap is entirely coins with no Binance kline history at all —
niche staked-ETH derivatives, brand-new pre-listing windows,
regulatory-purged delistings (certain privacy coins and tokens
delisted from major CEXes, …). A Yahoo
Finance secondary-source fallback would close that tail; left as
a follow-up — the gold-layer's forward-fill or "unpriced"
sentinel can handle the residual.

## Data model

The semantics the load.py replay implements — how CoinTracking's
transaction vocabulary maps onto per-wallet balances.

**Portfolios = linked CoinTracking user accounts.** Each linked
user maps to one wealthdb portfolio. The URL surface uses
`?change_user=<ID>` to switch the active portfolio; enumerate the
list of `(change_user_id, display_name)` pairs from the
portfolio-switcher dropdown at download time. IDs and display
names are user-specific PII — do NOT hard-code either into source.

**Wallets, not "exchanges".** CoinTracking's per-portfolio
groupings include both custodial exchanges and self-custody
hardware wallets. Use the neutral term **wallet** throughout
silver columns, identifiers, log lines, and user-facing labels.
Source-side translation happens at load time: a CSV column
literally headed "Exchange" goes into a silver `wallet_*` column.
Do NOT hard-code specific exchange or wallet-vendor names in
source — different users hold their coins on different platforms.

**Transfers are paired Withdrawal + Deposit, not linked.**
A move from wallet A to wallet B inside the same portfolio is
recorded as two independent CoinTracking transactions (the amounts
may differ by network/exchange fees). The replay does not pair
them; treat each as standalone. Net effect on portfolio totals is
near-zero modulo the small fee delta; per-wallet positions move
correctly because the two transactions hit different wallets.

**Fees are informative only — except for dedicated fee-only rows.**
The `fee_amount` / `fee_currency` columns on regular Trade /
Deposit / Withdrawal rows are NOT subtracted from buy/sell amounts
during replay — CoinTracking already excludes them from the
recorded amount. They ride through to silver for tax-cost
reporting; the balance equation never reads them.

The CSV also contains dedicated **`Type = "Other Fee"`** rows
(a small minority of rows). These ARE real balance deltas: the
event itself is a fee being deducted from a wallet, recorded as a
`-sell` on the wallet's currency. The replay applies them like a
Withdrawal. The distinction matters: same column name (`Fee`),
two opposite semantics (informative on a regular row, balance-
moving on an `Other Fee` row).

**`Type = "Other Expense"`** is CoinTracking's generic outgoing-
balance type and is likewise a real `-sell` delta. It surfaces as
the sell leg of an exchange **dust sweep** — a periodic conversion
of tiny leftover balances (CoinTracking labels it "Dust Sweeping"
in the `Comment` column) that books each swept dust balance as an
`Other Expense` sell and the consolidated proceeds as a single
`Income (non taxable)` buy. Both legs must stay routed: drop the
sell side and the proceeds land while the dust never leaves,
stranding each swept balance at exactly the swept amount.

**Type handler set (validated against captured balance.csv).**
CoinTracking has accumulated a ~19-type vocabulary over time;
the replay routes each row by `type`. The two lists live as the
`BUY_TYPES` / `SELL_TYPES` constants in `load.py` — a single source
of truth shared by the replay SQL and the unhandled-type guard:

  - **buy side** (`+buy_amount` on `buy_currency`):
    Trade, Deposit, Staking, Reward / Bonus, Income, Income
    (non taxable), Airdrop, Airdrop (non taxable), Gift / Tip,
    Gift
  - **sell side** (`-sell_amount` on `sell_currency`):
    Trade, Withdrawal, Other Fee, Other Expense, Lost, Stolen,
    Spend, Donation, Gift / Tip, Gift, Expense (non taxable)

`Gift / Tip` and `Gift` appear on both lists — they're
direction-ambiguous (the same type covers incoming gifts and
outgoing tips). The `buy_amount IS NOT NULL` /
`sell_amount IS NOT NULL` filters route each row to exactly one
branch based on which CSV column is populated.

**Unlisted types are silently dropped — so the loader makes it
loud.** A `type` on neither list contributes nothing to the replay:
an outgoing type left off the sell side leaves a wallet's balance
too high, an incoming one too low, with no error. That is precisely
how `Other Expense` escaped notice until reconciliation flagged it.
Because CoinTracking keeps growing the vocabulary (margin, lending,
derivatives, …), `warn_unhandled_transaction_types()` runs on every
load and logs a WARNING naming any type whose populated leg the
replay would drop, with the dropped buy/sell-leg counts. It is a
standing tripwire for the next unlisted type — not a substitute for
adding the handler.

With the set above, every portfolio reconciles against its
per-wallet balance.csv with zero discrepancies beyond ±10⁻⁸ (the
8-decimal CSV export precision).

**Silver is DuckDB, not SQLite — the exception in this repo.**
Every other collector's silver is SQLite + JSON1; cointracking
breaks that pattern for two specific reasons:

  - **Window-function replay at load time.** The holdings
    calculation collapses to a single `SUM(daily_delta) OVER
    (PARTITION BY portfolio, wallet, instrument ORDER BY date
    ROWS UNBOUNDED PRECEDING)`. Doing the same work as a Python
    loop with `decimal.Decimal` is correct but tedious to
    maintain and slower; DuckDB executes the whole replay in
    milliseconds. No other silver in the repo needs this kind of
    computation — they're all shape transformations — so the
    SQLite default is right for them and the DuckDB exception is
    contained here.
  - **Native `DECIMAL(38, 18)`.** Exact arithmetic for amount
    columns, no `TEXT`-with-Python-Decimal round-trips, no `REAL`
    precision loss. CoinTracking's CSV currently truncates to 8
    decimals anyway, so the extra width is unused today but
    future-proofs the table if their export ever widens.

Cost of the exception:
  - The Go gold adapter for cointracking uses the DuckDB Go driver
    (`duckdb-go`) instead of `modernc.org/sqlite`. Adapter-level
    concern; doesn't touch the rest of the gold engine.
  - `collectorkit.silver` is SQLite-shaped — cointracking's
    load.py does not import it. The DuckDB equivalents (a
    one-file apply-migrations helper) live inline in load.py.
  - One more wheel in the image (`duckdb`, ~30 MB).

**The replay** lives in `load.py` as the constant
`REPLAY_SQL_TEMPLATE` (the buy/sell `type` filters formatted in from
`BUY_TYPES` / `SELL_TYPES`) and runs against the latest
`dump_runs.snapshot_at`. Shape:

```
WITH deltas AS (                              -- explode each row
    -- buy side: +amount on buy_currency for types with a buy
    SELECT … buy_amount AS delta FROM transactions
    WHERE type IN ('Trade','Deposit','Staking','Income(*)','Airdrop(*)')
    UNION ALL
    -- sell side: -amount on sell_currency for types with a sell
    -- (incl. "Other Fee" + "Other Expense" rows — see fee semantics
    --  above; abbreviated — SELL_TYPES in load.py is authoritative)
    SELECT … -sell_amount AS delta FROM transactions
    WHERE type IN ('Trade','Withdrawal','Other Fee','Other Expense',
                   'Lost','Spend','Gift / Tip','Expense (non taxable)')
),
daily_deltas AS (                             -- pre-aggregate by day
    SELECT portfolio, wallet, instrument, as_of_date, SUM(delta)
    FROM deltas
    GROUP BY portfolio, wallet, instrument, as_of_date
)
SELECT … SUM(daily_delta) OVER (              -- cumulative running sum
    PARTITION BY portfolio, wallet, instrument
    ORDER BY as_of_date
    ROWS UNBOUNDED PRECEDING
) AS amount
FROM daily_deltas
```

The pre-aggregate stage matters: the window function then runs
over one row per (portfolio, wallet, instrument, day) instead of
one per raw transaction. At typical transaction-history scales
that's roughly an order-of-magnitude cut, and the factor only
grows as the transaction history does. It also keeps the resulting query plan
free of the row-discarding `QUALIFY` filter the naive version
would need.

## Silver schema

The silver schema mirrors the shape cointracking canonicalises,
using the data-model vocabulary above:

- `portfolios` — one row per linked CoinTracking user account, with
  the `timezone` its trade dates were read in.
- `wallets` — one row per (portfolio, wallet); covers custodial
  exchanges and self-custody hardware wallets uniformly.
- `transactions` — raw rows from the 13-column "CSV (Full Export)"
  trade CSV, one per CSV row, with the source `type` preserved and
  amounts as `DECIMAL(38, 18)`. `trade_group`, `tx_id` and
  `occurred_local` hold the `Group`, `Tx-ID` and `Date` text as
  printed; `occurred_at` is `occurred_local` in UTC.
- `positions_daily` — COMPUTED by load.py via the transaction
  replay: `(as_of_date, portfolio, wallet, instrument) → amount`.
  Written only on days where state actually changed; gold queries
  forward-fill.
- `portfolio_prices` — CT-reported per-portfolio prices parsed from
  overview.csv, in each portfolio's "main fiat" quote currency.
- `coin_prices` — canonical USD reference prices per coin per day
  (Binance USDT-denominated crypto + Frankfurter/ECB FX for fiat
  cash).
- `coin_mapping` — CT-ticker → market-data-provider-id translation,
  keyed on (instrument, provider).

Concrete columns are in
[`migrations/0001_initial.sql`](migrations/0001_initial.sql),
[`migrations/0002_prices.sql`](migrations/0002_prices.sql) and
[`migrations/0003_group_txid_local_time.sql`](migrations/0003_group_txid_local_time.sql).

The Go gold adapter (`wealthdb/internal/silver/cointracking/`) opens
this DuckDB silver read-only and projects portfolios → accounts →
positions → transactions into the canonical gold layer.

## Future work

**Graduate `explore` to a shared discovery tool.** Every web-based
collector in wealthdb goes through the same discovery loop: stand up
a headed browser, capture clicks + HAR, study the artefacts, write
`login.py` / `download.py` from them (schwab-web and fidelity-web
ship a `vnc-login` subcommand for the same purpose; the private-
market collectors carry their own `explore.py`). Rather than keep a
per-collector copy, `explore.py` could be lifted into a top-level
discovery tool (target home: `shared/explore/` or `tools/explore/`,
name TBD) taking the target URL + profile dir + debug dir as args
and producing the same artefact triple — HAR + Playwright trace +
click log — plus a shared VNC port walk and click-recorder JS:

```sh
wealthdb-explore <target-name> [--url URL] [--profile-dir DIR] [--debug-dir DIR]
```

The per-collector `explore` / `vnc-login` subcommands could then be
retired in favour of the shared tool. Deferred: generalising now
would slow immediate work while the per-collector harnesses still
differ in ergonomics.

**`documents` table.** A content-hash-tracked silver table for
exported PDF reports, if bulk PDF archival is ever wanted. CSV
exports are the primary signal today; no PDF fetch path exists.

**Float-vs-Decimal validation.** The replay uses `DECIMAL(38, 18)`
throughout. Running the CTE chain a second time with the amount
columns cast through `DOUBLE` and diffing the resulting
`positions_daily` snapshots — three-way against CoinTracking's
reported per-coin balances — would reveal whether CT computes
internally in doubles, arbitrary precision, or rounds only at
display, and hence whether a float fast-path is ever safe.

## Why Camoufox

The `explore` harness runs Camoufox, matching schwab-web /
fidelity-web: aggregator portals often sit behind Cloudflare or an
Akamai-style bot detector, and Camoufox's stealth-Firefox posture
(`os="macos"` + `humanize=True` + `geoip=True`) clears that without
ad-hoc fingerprint patching. Discovery then established that
cointracking does NOT bot-check the login flow, so `login.py` /
`download.py` drive vanilla headless Playwright Firefox instead
(see the Login section above); Camoufox stays as the base image the
`explore` harness depends on.
