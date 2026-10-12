# cointracking — design notes

This collector drives a real signed-in cointracking.info session,
exports the CSVs CoinTracking already produces, and loads them into a
DuckDB silver. The stages are discovery, login, download and load; each
is described below. `explore` is the discovery harness the login and
download flows were written from. It stays for re-discovery when
CoinTracking changes its UI. [README.md](README.md) has the verbs, the
examples and the silver tables.

## Why a separate collector

cointracking.info is the aggregator of record for all crypto: exchange
accounts and on-chain wallets. One collector per source, like the bank
collectors, has these effects:

- The silver schema holds crypto only, so the gold-side reconciliation
  between fiat and crypto stays explicit.
- A crypto refresh runs without touching the bank collectors, and the
  other way round.
- A direct crypto source can be added beside it later.

Integrating each exchange API and every on-chain wallet directly is
rejected. CoinTracking already does that work and serves as the
canonical view.

## Discovery — the `explore` harness

`explore.py` launches Camoufox in the container's Xvfb display, opens
cointracking.info, and records the live session. It is built on
`collectorkit.explore`. The main channels:

1. **HAR** — full network capture via Playwright's `record_har_path`.
   The primary artefact for finding internal REST endpoints. Every
   fetch, XHR and form submit appears with headers and bodies.
   Playwright writes it whole, so the harness rewrites it once the close
   has flushed it, with credentials, cookies and query-string tokens
   masked.
2. **Playwright trace** — opt-in with `--trace`:
   `tracing.start(screenshots=True, snapshots=True, sources=True)`
   captures DOM and screenshot snapshots at every navigation. Open it
   with `playwright show-trace`. It is off by default for two reasons.
   The pinned tracer crashes the Camoufox Firefox build. And a trace
   cannot be redacted after the fact: its DOM snapshots store every
   input's value, a hand-typed password included.
3. **Click log (`clicks.jsonl`)** — a JS init script listens to every
   `click` event in the DOM and emits one JSON object per click to the
   page's console. The Python side harvests it via `page.on('console')`.
   This is needed because VNC mouse clicks bypass Playwright's API and
   do not show up in the trace as actions.

The harness also writes a line-flushed `network.jsonl` and saves every
downloaded file under `downloads/`.

Recording stops when the last browser window closes (Camoufox's
persistent context fires `close`) or on Ctrl-C. `--max-duration`
(default 1 hour) stops a forgotten session.

Artefacts land under `/debug/<UTC-ts>/`, mounted from the host debug dir
(`${XDG_CACHE_HOME:-~/.cache}/wealthdb/debug/cointracking`). They never
land under `/data`, so the bronze tree holds only `load` inputs. `prune`
reclaims the debug dir.

The persistent profile dir `/secrets/cointracking-profile` carries the
post-MFA session cookie between runs. A later `explore` therefore skips
the 2FA challenge unless `--fresh` wipes the profile.

## Login — headless Playwright Firefox, CLI-MFA

Pure HTTP does not work. POST 1's `password_login` is a 32-char
**session-encrypted blob** (mixed-case ASCII with `#`/`%`/`^`, not any
standard hash, base64 or hex). JS on /login.php computes it with a
session-issued key. The server accepts `md5(password)` in POST 1 far
enough to return the 2FA challenge form, so the trace looks deceptively
similar. But the session state after POST 1 is not fully valid, and
POST 2 bounces back to /login.php with a fresh unauthenticated form.
Reproducing the encryption in Python would mean reverse-engineering the
JS, which breaks on any CoinTracking-side tweak.

So `login.py` drives **vanilla Playwright Firefox in headless mode**.
There is no Camoufox stealth (CoinTracking does not bot-check), no Xvfb,
no VNC and no display. The browser runs the page's JS-side encryption.

Flow:

1. Launch `firefox.launch_persistent_context(user_data_dir=…,
   headless=True)` on `/secrets/cointracking-profile/`, the same dir
   `explore.py` uses.
2. GET /dashboard. If the profile is still signed in, exit 0. No
   credentials are posted.
3. Navigate to /login.php. If it lands on an authenticated /dashboard,
   return.
4. Fill `username_login` and `password_login` and click submit.
5. Wait for `input[name='code_2fa']`. If the page reaches the
   dashboard instead, the device-trust cookie was still valid; return.
6. Read the code from stdin. A code that is not 6 digits draws a
   warning and is submitted as entered.
7. Fill `code_2fa`, tick `#dont_ask_again` (it plants the multi-year
   `ctfa<user_id>` device-trust cookie), and click submit.
8. Wait for navigation to `/dashboard*`, then probe /dashboard again.
9. `context.close()` writes the updated cookies and localStorage back
   to the profile.

`--check` runs step 2 only. It exits 0 when /dashboard shows no login
form and 1 otherwise. It posts no credentials and raises no 2FA
challenge.

Because of step 2, running `login` against a signed-in profile costs
nothing. The `ctfa<user_id>` cookie keeps that path live until the next
genuine session-cookie rotation.

The image builds on `base-camoufox` because `explore.py` needs it. The
Dockerfile adds vanilla Firefox with `playwright install firefox`
(about 80 MB). The two browsers coexist cleanly: separate binary paths,
no API conflict.

## Download — headless Playwright Firefox, per-portfolio blob loop

`download` uses the same browser stack as `login.py` and the same
profile, so the session carries over. It never signs in. An
unauthenticated session stops the run with a pointer to `login`.

**Always full history.** CoinTracking serves the full history quickly,
and the holdings replay needs every row. So every run pulls the complete
transaction list per portfolio. `--lookback` parses and logs a warning
that it has no effect.

**Portfolio discovery:**

- Master account ID: from the `ctfa<id>` session cookie name.
- Linked-account IDs and display names: from the
  `<a href*="change_user=N">` anchors on /enter_coins.php. The in-page
  portfolio switcher shows the other portfolios; the current one (the
  master) is implicit.
- The union of the two is the full portfolio list.
- The live scrape is merged with a persistent `known_portfolios.json`
  union cache at the bronze root. CoinTracking's linked-user list is
  flaky, and the cache keeps a transient miss from dropping a portfolio
  from the download set.

**Per-portfolio loop** (in `download_portfolio()`):

1. `GET /enter_coins.php?change_user=<id>` activates the portfolio
   server-side.
2. Set `<select name="extended">` to value "2", "Extended with
   additional columns". Its CSV export carries 13 columns: Type, Buy,
   Cur., Sell, Cur., Fee, Cur., Exchange, Group, Comment, Date, LPN,
   Tx-ID. Wait for networkidle.
3. Click `button:has-text("Export")`, wait for the menu, and click
   `text("CSV (Full Export)")`. `page.expect_download()` and `save_as()`
   capture the blob.
4. `GET /balance_by_exchange.php?change_user=<id>` activates the
   portfolio for the balance view.
5. Click Export, then the exact-text "CSV" item. Save the blob.
6. `GET /overview.php?change_user=<id>`, then Export → CSV → "Comma
   separated". This saves the Daily Balance overview: wide-form,
   per-day holdings, which `load` parses into `portfolio_prices`.

Files land in `<bronze-dir>/<UTC-ts>/cu_<id>/{trades,balance,overview}.csv.zst`.

A portfolio that raises is logged and recorded under `failures` in
`run.json`, and the loop moves on. The manifest's `portfolios` list
holds only the portfolios that succeeded. `download` exits nonzero only
when every portfolio fails.

The collector fetches no PDFs. The CSV exports are the whole signal.

**Bronze compression.** Each CSV export is zstd-compressed in place as
it lands (`collectorkit.compress.compress_best_effort`). The write is an
atomic tmp+rename, the plain file is unlinked only after a
decompress-and-sha256 check, and the mtime is carried over. Every run
writes the full history, so uncompressed bronze grows fast; compression
cuts its size by roughly an order of magnitude. The read side is free:
DuckDB's `read_csv_auto` decompresses `.csv.zst` (and `.csv.gz`)
natively by extension. `load` only resolves the on-disk variant via
`compress.resolve_variant`; plain `.csv` wins when both forms exist. A
compression failure is a warning and leaves the plain CSV in place.
Every reader accepts both forms, so a mixed tree is valid. The manual
`recompress` verb converts plain-CSV run dirs. It is a thin wrapper over
`collectorkit.recompress`, with prune's safety envelope and completeness
predicate. After a sweep, a `load --force` rebuild must produce
identical silver.

**`run.json` status.** The fleet lifecycle is in
[collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze).
Here the walk writes the `in-progress` marker right after creating the
run dir, and the full manifest with `"status": "complete"` after the
last portfolio. A crash before the `mkdir` leaves no `run.json`. A
`run.json` with no `status` key is an older dump; it counts as complete,
because such a dump got its `run.json` only at the end.

`--dry-run` discovers the portfolios and walks the navigation, but skips
every export click. It creates no run dir and does not update
`known_portfolios.json`, so `prune` has no `dry-run` shell to reclaim.
It checks portfolio discovery and the selectors without fetching data.

`--debug` keeps two diagnostic captures, the DOM and a screenshot, under
`<run>/screenshots/`:

- The portfolio-discovery page. It is the auth gate, and its switcher
  anchors are injected by late JS. When the scrape comes back short and
  the cache fills the gap, this DOM is the only record of why.
- The page a portfolio was on when its download failed. The page is
  still on the export surface that raised, so this is the DOM the
  failing selector was matched against. Capturing all three surfaces
  for every portfolio would bury it.

`load` never reads either capture, and `prune` reclaims
`<run>/screenshots/`. `--debug` is off by default and does nothing under
`--dry-run`, which creates no run dir.

## Load — DuckDB silver with incremental positions_daily upsert

`load` walks the bronze tree oldest first. It skips a run dir whose
status is not complete and a snapshot already in `dump_runs`. Each
remaining snapshot is ingested in one DuckDB transaction. A snapshot
that fails rolls back, is logged, and the loop goes on to the next one.
Per snapshot:

1. **transactions** — replaced for every portfolio in the manifest.
   Each trade CSV is parsed via `read_csv_auto(all_varchar=true)` and
   projected into the silver schema. CoinTracking gives a row no unique
   id: `Tx-ID` is often blank or shared, and many rows share a
   timestamp. So `transaction_external_id` is `cu_<id>:<hash>`, where
   the hash is the first 16 hex digits of a sha256 over the row's
   columns. The 2nd, 3rd, … copy of an exact duplicate row gets `:2`,
   `:3`, …. The id does not depend on the CSV's row order. A row keeps
   its id across exports until CoinTracking amends it, and gold's order
   (`occurred_at`, then id) is total and the same on every load.
   `Group` and `Tx-ID` land in `trade_group` and `tx_id` as printed.
   `Date` lands in `occurred_local` as printed, and `occurred_at` holds
   it converted to UTC (see "Timezone" below). A portfolio missing from
   the manifest keeps the rows it was last loaded with.

2. **portfolios + wallets** — upserted from the `run.json` manifest and
   the loaded transactions' `Exchange` column. Each portfolio row
   records the timezone its `Date` column was read in.

3. **positions_daily — incremental upsert.** The full new time series
   is computed via the aggregate-then-window replay. A `FULL OUTER JOIN`
   of new against existing finds the first `as_of_date` where they
   differ: a changed amount, a new row, or a removed row
   (`IS DISTINCT FROM` catches all three). Only rows from that cutoff
   date onwards are deleted and re-inserted. Older rows keep their
   original `snapshot_at`. Gold uses `snapshot_at` to detect changes, so
   it re-processes only the changed slice. This matters because a
   transaction edit in CoinTracking can land arbitrarily far in the
   past.

4. **reconciliation** — for each portfolio, the final balance per
   `(wallet, instrument)` from `positions_daily` is compared against
   `balance.csv`. A difference above ±10⁻⁸ (the 8-decimal export
   precision) is logged as a warning, not a failure. A coverage gap
   (CoinTracking has it, the replay does not) and an unreported
   non-zero position (the replay has it, CoinTracking does not) both
   warn. A zero position only in the replay is fine: CoinTracking's UI
   filters those out.

5. **dump_runs** — the snapshot is recorded with the cutoff date and
   the rows-written count in `payload`, so a later run can tell which
   snapshot moved which slice of history. The row also records the
   schema version the snapshot was loaded under.

After the loop, `load` checks the newest loaded snapshot, whose rows
`transactions` holds. It ingests that snapshot again when one of two
things has changed since it was loaded:

- the schema version, because rows loaded under an older one hold
  NULL in the columns a newer one adds;
- a portfolio's configured timezone, because its `occurred_at` was
  converted from the old zone.

So a migration or a new timezone setting takes effect at the next
`load`, with no new download and no `--force`.

`--replay-only` skips the bronze ingest. It re-runs the
`positions_daily` upsert against whatever is already in `transactions`.
It helps when iterating on the type-handler rules.

`--force` deletes the silver DB and rebuilds it from all bronze, the
fleet-wide meaning.

`--scratch-dir DIR` runs the load against a working copy of the silver
DB in `DIR` and moves the finished file back over `--silver-db` on
success. The container entrypoint passes `--scratch-dir /tmp`. DuckDB's
many small writes then stay on the container's own disk instead of the
slow bind mount, and the silver reaches `/data` in one move. A failed
load leaves the original silver untouched.

### Timezone

CoinTracking writes the trade export's `Date` in the timezone set on
the portfolio's account, with no offset. The export does not say which
zone that is. So the zone is a per-deployment setting:

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

`prune.py` is a thin wrapper over the shared `collectorkit.prune`
engine. The engine's safety envelope is described in
[collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze).
cointracking's configuration:

- **Completeness:** `prune.status_classification` with a legacy
  fallback of `m is not None`. A readable manifest with no `status` is
  a complete older dump. `load` and `recompress` use the same rule.
- **Debug subdirs:** `debug_subdirs = ("screenshots",)`, the `--debug`
  captures. Every other file in a run dir is a `load` input: `run.json`
  and each `cu_<id>/{trades,balance,overview}.csv.zst` (plain `.csv` in
  older dumps).
- **Debug cache:** the wrapper passes `--debug-dir` (the host debug
  dir), so `prune` also reclaims `explore`'s recordings once they are
  quiet.
- **Data-root files:** `known_portfolios.json` and the silver
  `cointracking.duckdb` sit at the bronze root, beside the run dirs.
  `prune` matches only `<UTC-ts>/` dirs, so neither is in scope.

`prune` runs host-side. A file walk needs none of `load`'s
in-container dependencies (DuckDB and the price clients). Running
host-side also bypasses the wrapper's single-writer guard, so `prune`
can reclaim disk while a `login` or `download` container runs; the age
guard protects the dump in flight. `entrypoint.sh` keeps a
browserless `prune)` arm too, for a direct `docker run`.

## Page surfaces and endpoints

The cointracking.info surfaces the collector reads:

- /dashboard: the session probe of `login` and `login --check`.
- /login.php: the sign-in form and the 2FA challenge.
- /enter_coins.php: the trade history and the portfolio switcher.
- /balance_by_exchange.php: current per-wallet holdings.
- /overview.php: Balance By Day, the wide-form daily history.

**change_user mechanics:** the `?change_user=N` query parameter switches
the active portfolio on every per-portfolio page. Server-side state
follows the latest such navigation.

Surfaces found in discovery and not used:

- /export/trades_csv.php: a simple CSV endpoint.
- /export/export_html.php: an HTML export, strictly inferior.
- /ajax/all_current_balance.php: JSON with HTML strings embedded. It is
  the SPA render envelope, not clean data.

## Prices — portfolio_prices + coin_prices

Two price tables, filled from different sources.

**`portfolio_prices`** holds what CoinTracking reports per portfolio. It
comes from the `/overview.php` export: Export → CSV → Comma separated.
The menu is a DataTables `buttons-collection`; the first "CSV" click
opens a sub-menu of variants, and the loop picks comma. One row per
(day, portfolio, coin, quote_currency). Price is
`value_in_fiat / amount`. Rows with `amount = 0` are dropped, which
guards the division. The quote currency is the portfolio's "main fiat"
CoinTracking setting. It can differ from one portfolio to the next. The
column header `<SYM> Value in <FIAT>` carries it.

CoinTracking filters the master-account overview to a top-N subset of
coins, while a linked portfolio's overview shows its full set. The
download loop fetches every portfolio's overview, so `portfolio_prices`
holds the union.

**`coin_prices`** holds USDT-denominated daily closes per coin, from
Binance public spot's `/api/v3/klines` endpoint. It covers every coin
held on any day in any portfolio: the "held set" from
`positions_daily.amount > 0`. The CoinTracking-ticker → Binance-symbol
mapping lives in `coin_mapping`. It is built lazily from
`/api/v3/exchangeInfo`: filter for pairs with `quoteAsset=USDT`, and key
by `baseAsset`. Stored values are USDT-denominated. The small USDT/USD
basis (a few bps) goes into `price_usd` as is, well within tolerance
for portfolio valuation.

Stablecoins (USDT, USDC, DAI, BUSD, TUSD, …) get a synthetic 1.0,
because they cannot be self-quoted on Binance. `coin_mapping` marks
them with a `STABLECOIN_SENTINEL` value, and the kline loop
short-circuits on it.

The `provider` column in `coin_mapping` and `coin_prices.source` allow
another market-data provider without a schema migration. `binance.py`
is the current implementation. A second source, for coins delisted from
every exchange, needs only a client module with the same
`build_mapping` / `ohlcv_historical` interface.

**Three invocation patterns.** `binance.py` is the polite client,
`fetch_prices.py` the standalone CLI, and `load.py` embeds the same
engine:

| Trigger | Mode | Behaviour |
|---|---|---|
| `load` (default) | missing | Fill (held, unpriced) gaps, and always re-fetch the latest priced day per coin |
| `fetch-prices --missing` | missing | Same as above; for use after `load --no-fetch-prices` |
| `fetch-prices` (no flag) | full | Delete every `binance` and `frankfurter` row in `coin_prices`, then re-fetch the full held range. Corruption recovery. |

Each mode fetches the crypto prices, then the fiat rates, then runs
the first-day backfill below.

The latest priced day is always re-fetched because it was an intraday
price when first written. Binance's klines include a partial kline for
the current UTC day; the next run replaces it with the close.

Rate limits: `RATE_LIMIT_DELAY_S = 0.1` s (about 600 calls a minute)
sits well below Binance's ~1,200/min IP weight ceiling. One call returns
up to 1,000 daily klines. A new portfolio's first backfill takes one
chunked series of calls per held coin, back to its first trade, and
finishes in well under a minute. An optional `BINANCE_API_KEY` enables
authenticated tiers with higher rate limits; the public endpoint does
not need it.

**Why coin_prices exists beside portfolio_prices.** The two serve
different roles. `portfolio_prices` is CoinTracking's own valuation, in
each portfolio's main fiat. `coin_prices` is a USD reference shared
across portfolios, for fiat as well as crypto. How gold combines them is
in the [adapter doc](../../wealthdb/docs/adapters/cointracking.md).

### FX rates: Frankfurter / ECB

Fiat held as cash (EUR, CHF, GBP, …) needs a USD price too. It comes
from `api.frankfurter.app`, a thin wrapper around the ECB's daily
reference rates. It is free, with no key and no signup. The rates go
into the same `coin_prices` table under `source='frankfurter'`. The ECB
publishes on weekdays only. The client forward-fills across weekends and
ECB holidays, the standard FX convention: Friday's rate holds until
Monday's.

### Backfill: first-day-held edge cases

A portfolio can first buy a coin before Binance lists its USDT pair.
No kline exists for those days. `backfill_first_day_gaps()` runs after
every price fetch. It fills each held, unpriced (instrument, day) that
has a same-instrument `coin_prices` row within the next 365 days, with
the earliest such price. The new row keeps the `source` of the row it
copies, so gold cannot tell it from a fetched price.

### Coverage gap

After the fetch, the FX rates and the backfill, a coin stays unpriced
only when Binance has no kline history for it at all. That is a coin
before its listing, or one whose history Binance purged after a
delisting. The `exchangeInfo` pair metadata can outlive that history,
so a mapping can resolve to a symbol with no prices. A second price
source would close that tail. Until then, gold's forward-fill or its
"unpriced" sentinel handles it.

## Data model

The semantics the `load.py` replay implements: how CoinTracking's
transaction vocabulary maps onto per-wallet balances.

**Portfolios = linked CoinTracking user accounts.** Each linked user
maps to one wealthdb portfolio. The URL surface uses
`?change_user=<ID>` to switch the active portfolio. The list of
`(change_user_id, display_name)` pairs comes from the portfolio
switcher at download time. IDs and display names are personal data;
never hard-code either into source.

**Wallets, not "exchanges".** CoinTracking's per-portfolio groupings
include both custodial exchanges and self-custody hardware wallets.
Silver columns, identifiers, log lines and labels use the neutral term
**wallet**. The translation happens at load time: the CSV column headed
"Exchange" goes into a silver `wallet_*` column. Never hard-code
specific exchange or wallet-vendor names in source; every account holds
its coins on different platforms.

**Transfers are paired Withdrawal + Deposit, not linked.** A move from
wallet A to wallet B in the same portfolio is two independent
CoinTracking transactions. The amounts may differ by network or
exchange fees. The replay does not pair them; each stands alone. The
net effect on portfolio totals is near zero, apart from the fee. The
per-wallet positions move correctly, because the two transactions hit
different wallets.

**Fees are informative only, except on dedicated fee rows.** The
`fee_amount` / `fee_currency` columns on regular Trade, Deposit and
Withdrawal rows are NOT subtracted from the buy or sell amounts during
the replay. CoinTracking already excludes them from the recorded
amount. They ride through to silver for tax-cost reporting; the balance
equation never reads them.

The CSV also holds dedicated **`Type = "Other Fee"`** rows. These ARE
real balance deltas. The event is a fee deducted from a wallet,
recorded as a `-sell` on the wallet's currency. The replay applies
them like a Withdrawal. The distinction matters: the same column name
(`Fee`) has two opposite meanings. It is informative on a regular row
and moves the balance on an `Other Fee` row.

**`Type = "Other Expense"`** is CoinTracking's generic outgoing type and
is likewise a real `-sell` delta. It is the sell leg of an exchange
**dust sweep**: a periodic conversion of tiny leftover balances.
CoinTracking labels it "Dust Sweeping" in the `Comment` column. It
books each swept dust balance as an `Other Expense` sell and the
consolidated proceeds as one `Income (non taxable)` buy. Both legs must
stay routed. Drop the sell side, and the proceeds land while the dust
never leaves, which strands each swept balance at exactly the swept
amount.

**Type handler set.** CoinTracking has a vocabulary of about 19 types,
and the replay routes each row by `type`. The two lists are the
`BUY_TYPES` / `SELL_TYPES` constants in `load.py`. They are the single
source of truth for the replay SQL and the unhandled-type guard:

- **buy side** (`+buy_amount` on `buy_currency`): Trade, Deposit,
  Staking, Reward / Bonus, Income, Income (non taxable), Airdrop,
  Airdrop (non taxable), Gift / Tip, Gift
- **sell side** (`-sell_amount` on `sell_currency`): Trade, Withdrawal,
  Other Fee, Other Expense, Lost, Stolen, Spend, Donation, Gift / Tip,
  Gift, Expense (non taxable)

`Gift / Tip` and `Gift` are on both lists. They are direction-ambiguous:
the same type covers incoming gifts and outgoing tips. The
`buy_amount IS NOT NULL` / `sell_amount IS NOT NULL` filters route each
row to exactly one branch, by which CSV column is populated.

**An unlisted type is dropped silently, so the loader makes it loud.**
A `type` on neither list adds nothing to the replay. An outgoing type
missing from the sell side leaves a wallet's balance too high, an
incoming one too low, with no error. CoinTracking keeps growing its
vocabulary (margin, lending, derivatives, …). So
`warn_unhandled_transaction_types()` runs on every load. It logs a
WARNING naming any type whose populated leg the replay would drop, with
the dropped buy- and sell-leg counts. It is a standing tripwire, not a
substitute for adding the handler. The reconciliation step (Load, step
4) is the second check: a missing handler shows up as a per-wallet
mismatch against `balance.csv`.

**Silver is DuckDB, not SQLite — the exception in this repo.** Every
other collector's silver is SQLite + JSON1. cointracking breaks that
pattern for two reasons:

- **Window-function replay at load time.** The holdings calculation
  collapses to one `SUM(daily_delta) OVER (PARTITION BY portfolio,
  wallet, instrument ORDER BY date ROWS UNBOUNDED PRECEDING)`. The same
  work as a Python loop with `decimal.Decimal` is correct but tedious to
  maintain and slower. DuckDB runs the whole replay in milliseconds. No
  other silver needs this kind of computation; they are all shape
  transformations. So the SQLite default is right for them, and the
  DuckDB exception stays here.
- **Native `DECIMAL(38, 18)`.** Exact arithmetic for amount columns,
  with no `TEXT`-with-Python-Decimal round-trips and no `REAL`
  precision loss. CoinTracking's CSV truncates to 8 decimals, so the
  extra width is headroom for a wider export. The 8-decimal export also
  hides whether CoinTracking computes in doubles or in exact decimals.

Cost of the exception:

- The Go gold adapter uses the DuckDB Go driver (`duckdb-go`) instead
  of `modernc.org/sqlite`. That stays inside the adapter.
- `collectorkit.silver`'s SQLite helpers (`open_db`,
  `apply_migrations`) do not fit. `load.py` keeps its own small
  DuckDB migration runner and uses only the file-level helpers
  (`silver.reset`, `silver.own_only`).
- One more wheel in the image (`duckdb`, about 30 MB).

**The replay** is the constant `REPLAY_SQL_TEMPLATE` in `load.py`, with
the buy and sell `type` filters formatted in from `BUY_TYPES` /
`SELL_TYPES`. It runs against the latest `dump_runs.snapshot_at`. Its
shape:

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

The pre-aggregate stage matters. The window function then runs over one
row per (portfolio, wallet, instrument, day) instead of one per raw
transaction. At typical history sizes that cuts the rows by roughly an
order of magnitude, and the factor grows with the history. It also
keeps the query plan free of the row-discarding `QUALIFY` filter the
naive version would need.

## Silver schema

The tables and what they hold are in
[README.md](README.md#output); the columns are in
[`migrations/`](migrations/). The Go gold adapter
(`wealthdb/internal/silver/cointracking/`) opens this DuckDB silver
read-only and maps portfolios, positions, prices and transactions into
gold; its [design doc](../../wealthdb/docs/adapters/cointracking.md)
has the detail.

## Why Camoufox

The `explore` harness runs Camoufox, like schwab-web and fidelity-web.
Aggregator portals often sit behind Cloudflare or an Akamai-style bot
detector. Camoufox's stealth-Firefox posture (`os="macos"`,
`humanize=True`, `geoip=True`) clears that without ad-hoc fingerprint
patching. CoinTracking does not bot-check the login flow, so `login.py`
and `download.py` drive vanilla headless Playwright Firefox instead (see
the Login section above). Camoufox stays as the base image that
`explore` needs.
