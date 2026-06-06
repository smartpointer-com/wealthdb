# cointracking — design notes

This is a Phase-1 skeleton. Most decisions are deferred until
`explore` produces real traces; what is locked in here is the
shape of the discovery harness and the contract with the rest of
the suite.

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

## Phase 1: discovery via `explore`

`explore.py` launches Camoufox in the container's Xvfb display,
opens cointracking.info, and records the live session via
three channels:

1. **HAR** — full network capture via Playwright's `record_har_path`.
   The primary artefact for finding internal REST endpoints. Every
   fetch / XHR / fetch-API / form-submit appears with headers and
   bodies.
2. **Playwright trace** — `tracing.start(screenshots=True,
   snapshots=True, sources=True)` captures DOM + screenshot
   snapshots at every navigation. Open with `playwright show-trace`.
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
`$HOME/.cache/cointracking-debug` on the host), explicitly NOT
under `/data` — the bronze/silver tree stays clean until `download`
exists.

The persistent Camoufox profile dir at
`/secrets/cointracking-profile` carries the post-MFA session cookie
between runs, so subsequent `explore` invocations skip the 2FA
challenge.

## Phase 2: login (done) + download (TBD)

### login — headless Playwright Firefox, CLI-MFA

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
exits — no 2FA push. The `ctfa<user_id>` cookie is multi-year
a multi-year value, so this path stays live until the next
genuine session-cookie rotation.

The Camoufox base image is still used because `explore.py` needs
it. Adding vanilla Firefox via `playwright install firefox` in
the Dockerfile costs ~80 MB; the two browsers coexist cleanly
(separate binary paths, no API conflict).

### download — headless Playwright Firefox, per-portfolio blob loop

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
  shows the 4 OTHER portfolios; the current/master is implicit).
- Union of the two = full list of 5 portfolios.

**Per-portfolio loop** (in `download_portfolio()`):
1. `GET /enter_coins.php?change_user=<id>` activates the
   portfolio server-side.
2. Set `<select name="extended">` value="2" → "Extended with
   additional columns" — the 19-column CSV mode (Trade ID,
   Imported From, Add Date, address/hash fields). Wait for
   networkidle.
3. Click `button:has-text("Export")` → wait for menu →
   click `text("CSV (Full Export)")`. `page.expect_download()` +
   `save_as()` captures the blob.
4. `GET /balance_by_exchange.php?change_user=<id>` activates the
   portfolio for the balance view.
5. Click Export → exact-text "CSV". Save the blob.

Files land in `<bronze-dir>/<UTC-ts>/cu_<id>/{trades,balance}.csv`.
After all portfolios are processed, write `run.json` to the
snapshot dir as the silver loader's manifest.

`--dry-run` walks the navigation and prints what it would do but
skips every export-button click — no downloads fire, no bronze
tree materialises. Use to verify portfolio discovery + selector
correctness without burning bandwidth.

### load — DuckDB silver with incremental positions_daily upsert

Each `load` invocation iterates the bronze tree, processing every
snapshot not already in `dump_runs`. Per snapshot:

1. **transactions** — fully replaced. Each blob CSV is parsed via
   `read_csv_auto(all_varchar=true)` and projected into the silver
   schema. `transaction_external_id` is synthesized as
   `cu_<id>:r<row_seq>:<Trade ID || Tx-ID || 'synth'>` because
   neither CoinTracking column is globally unique (1779 distinct
   Trade IDs out of 1801 populated; 1797 empty). Row order in the
   CSV is deterministic so re-loads produce stable IDs.

2. **portfolios + wallets** — upserted from the run.json manifest +
   the loaded transactions' `Exchange` column.

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
   snapshot moved which slice of history.

`--replay-only` skips the bronze ingest and re-runs the
positions_daily incremental upsert against whatever's already
in `transactions`. Useful when iterating on the type-handler
rules.

`--force` re-loads snapshots already in `dump_runs`. Useful for
re-validating after a load.py change without manually clearing
the table.

### Other questions answered by the explore traces

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

### Phase 3: prices (portfolio_prices + coin_prices)

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
| `load --fetch-prices` | missing | Fill (held, unpriced) gaps + always re-fetch the latest priced day per coin |
| `fetch-prices --missing` | missing | Same as above; for use after a `load` without `--fetch-prices` |
| `fetch-prices` (no flag) | full | Drop every coin_prices row sourced from Binance, re-fetch the full held range. Corruption recovery. |

The "always re-fetch the latest priced day" rule exists because
that row was an intraday snapshot when first written (Binance's
klines include a partial-day kline for the current UTC day; the
next run upgrades it to the daily close once the day closes).

Rate-limit posture: `RATE_LIMIT_DELAY_S = 0.1s` (~600 calls/min)
sits comfortably below Binance's ~1,200/min IP weight ceiling. A
new portfolio's first backfill (~25 coins, 8 years each in
1,000-day chunks = ~80 API calls) completes in well under a
minute. Optional `BINANCE_API_KEY` enables authenticated tiers
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
regulatory-purged delistings (XMR, DASH, NANO, …). A Yahoo
Finance secondary-source fallback would close that tail; left as
a follow-up — the gold-layer's forward-fill or "unpriced"
sentinel can handle the residual.

## Data model — locked-in facts

These hold regardless of what the trades CSV looks like; recorded
here so the load.py replay doesn't accidentally re-litigate them.

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
(~8% of the dataset observed). These ARE real balance deltas: the
event itself is a fee being deducted from a wallet, recorded as a
`-sell` on the wallet's currency. The replay applies them like a
Withdrawal. The distinction matters: same column name (`Fee`),
two opposite semantics (informative on a regular row, balance-
moving on an `Other Fee` row).

**Type handler set (validated against captured balance.csv).**
CoinTracking has accumulated a 16-type vocabulary over time;
the replay routes each row by `type`:

  - **buy side** (`+buy_amount` on `buy_currency`):
    Trade, Deposit, Staking, Reward / Bonus, Income, Income
    (non taxable), Airdrop, Airdrop (non taxable), Gift / Tip,
    Gift
  - **sell side** (`-sell_amount` on `sell_currency`):
    Trade, Withdrawal, Other Fee, Lost, Stolen, Spend,
    Donation, Gift / Tip, Gift, Expense (non taxable)

`Gift / Tip` and `Gift` appear on both lists — they're
direction-ambiguous (the same type covers incoming gifts and
outgoing tips). The `buy_amount IS NOT NULL` /
`sell_amount IS NOT NULL` filters route each row to exactly one
branch based on which CSV column is populated.

After the type-handler set was completed, all 5 portfolios
reconcile against the per-wallet balance.csv with zero
discrepancies beyond ±10⁻⁸ (the 8-decimal CSV export precision).

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
  - The Go gold adapter for cointracking will use `go-duckdb`
    instead of `mattn/go-sqlite3`. Adapter-level concern; doesn't
    touch the rest of the gold engine.
  - `collectorkit.silver` is SQLite-shaped — cointracking's
    load.py does not import it. The DuckDB equivalents (a
    one-file apply-migrations helper) live inline in load.py.
  - One more wheel in the image (`duckdb`, ~30 MB).

**The replay** lives in `load.py` as the constant `REPLAY_QUERY`
and runs against the latest `dump_runs.snapshot_at`. Shape:

```
WITH deltas AS (                              -- explode each row
    -- buy side: +amount on buy_currency for types with a buy
    SELECT … buy_amount AS delta FROM transactions
    WHERE type IN ('Trade','Deposit','Staking','Income(*)','Airdrop(*)')
    UNION ALL
    -- sell side: -amount on sell_currency for types with a sell
    -- (incl. "Other Fee" rows — see fee semantics above)
    SELECT … -sell_amount AS delta FROM transactions
    WHERE type IN ('Trade','Withdrawal','Other Fee','Lost','Spend',
                   'Gift / Tip','Expense (non taxable)')
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
one per raw transaction. At the current 3.5k-row scale that's a
~10× cut; the order-of-magnitude factor only grows as the
transaction history does. It also keeps the resulting query plan
free of the row-discarding `QUALIFY` filter the naive version
would need.

**Float-vs-Decimal validation experiment.** Same setup as before,
trivially expressible in DuckDB: run the CTE chain once with the
amount columns left as `DECIMAL(38, 18)` and once cast through
`DOUBLE`, then diff the resulting `positions_daily` snapshots.
The three-way comparison against CoinTracking's reported current
per-coin balance tells us whether they compute internally in
doubles, in arbitrary precision, or round only at display. This
informs whether the float fast-path is ever safe to trust.

## Phase 3: silver schema + gold adapter

Pending Phase 2. The silver schema mirrors the shape cointracking
already canonicalises, using the locked-in vocabulary above:

- `portfolios` — one row per linked CoinTracking user account.
- `wallets` — one row per (portfolio, wallet); covers custodial
  exchanges and self-custody hardware wallets uniformly.
- `transactions` — raw rows from `/export/trades_csv.php`, one
  per CSV row, with the source `type` preserved and amounts as
  `DECIMAL(38, 18)`.
- `positions_daily` — COMPUTED by load.py via the transaction
  replay: `(as_of_date, portfolio, wallet, instrument) → amount`.
  Written only on days where state actually changed; gold queries
  forward-fill.
- `documents` — exported PDF reports (if we ever fetch them)
  tracked by content hash. CSV exports are the primary signal;
  PDFs are optional bulk archive.

Concrete columns are in [`migrations/0001_initial.sql`](migrations/0001_initial.sql).
The actual bronze-ingest path waits until `download.py` lands.

## Phase 4: graduate `explore` to a shared bootstrap tool

Every web-based collector in wealthdb has gone through the same
discovery loop: stand up a headed browser, capture clicks + HAR,
study the artefacts, write `login.py` / `download.py` from them.
The cointracking `explore` command is the third time the pattern
has been written by hand (schwab-web's `vnc-login` and
fidelity-web's `vnc-login` are earlier rougher cuts).

Rather than delete `explore.py` once cointracking is fully
implemented, the plan is to lift it out of this collector into a
top-level discovery tool (target home: `shared/explore/` or
`tools/explore/`, name TBD). The collector wrapper would stop
shipping its own `explore` subcommand; the shared tool would take
the target URL + profile dir + debug dir as args and produce the
same artefact triple (HAR + Playwright trace + click log).

The shared tool would also save us from re-deriving the VNC port
walk, the click-recorder JS, and the artefact layout for each new
collector. Concretely, the API would look like:

```sh
wealthdb-explore <target-name> [--url URL] [--profile-dir DIR] [--debug-dir DIR]
```

…and the existing `vnc-login` subcommands on schwab-web /
fidelity-web could be retired in favour of the shared tool too
(they currently do similar things with slightly different
ergonomics).

Punted to after the cointracking explore phase produces working
traces — generalising now would slow the immediate discovery work
without any net benefit.

## Why Camoufox

Same reasoning as schwab-web / fidelity-web: cointracking.info
probably uses Cloudflare or an Akamai-style bot detector behind
its login (most aggregator portals do). Camoufox's stealth-Firefox
posture gets us past that without ad-hoc fingerprint patching, and
its `os="macos"` + `humanize=True` + `geoip=True` profile matches
what schwab-web / fidelity-web settled on after their own anti-bot
investigations.

If `explore` reveals cointracking doesn't actually need stealth, a
later move to plain Chromium would shed the Camoufox dependency.
Pragmatically: start strict, relax if telemetry says we can.
