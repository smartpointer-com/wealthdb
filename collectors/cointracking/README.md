# cointracking

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **This collector impersonates a human browser user and holds fully
> privileged financial-account credentials. Read this disclaimer in full
> before configuring any credential.**

This collector **impersonates a human user**: it drives a real browser
session that signs in to CoinTracking with your
credentials and your multi-factor confirmations. The session it holds is
**fully privileged** — the same login a human uses to move money — and
CoinTracking offers no read-only sub-scope, so nothing but this codebase's
own discipline restricts the session to reading. If malicious code were
ever introduced into this repository, its dependency chain, or the
container images it runs, it could act on your accounts with your full
authority and cause **irreversible financial damage, up to the total loss
of the assets reachable from those credentials**.

**You are solely responsible for a thorough, independent security audit**
of this code, its dependency chain, and its runtime images **before**
entrusting it with credentials, and again after every update or rebuild.
If you cannot perform such an audit, do not hand this software real
credentials. Automated access may additionally breach CoinTracking's terms
of service; verifying that your use is permitted is likewise your
responsibility.

**No warranty; no liability.** This software is provided “AS IS”, without
warranty of any kind, express or implied, including but not limited to the
implied warranties of merchantability, fitness for a particular purpose,
title, and non-infringement. To the maximum extent permitted by applicable
law, **SmartPointer AG and the contributors accept no responsibility for,
and shall not be liable for, any claim, damages, or other liability** —
whether in an action of contract, tort, or otherwise — arising from, out
of, or in connection with this software or its use, including without
limitation unauthorized or erroneous transactions, loss of funds or other
assets, credential or data compromise, account suspension or termination,
and any direct, indirect, incidental, special, consequential, or punitive
damages. Your use is entirely at your own risk. See
[LICENSE](../../LICENSE) for the governing terms. This software is not
affiliated with, endorsed by, or sponsored by CoinTracking or any other
financial institution; nothing in this repository is financial, legal, or
tax advice.

A read-only scraper for [cointracking.info](https://cointracking.info).
CoinTracking is an aggregator. It tracks crypto holdings and transactions
across exchanges and on-chain wallets. wealthdb has no direct crypto
integration: CoinTracking is its single source for crypto, the way each
bank collector is the source for its bank.

CoinTracking has no public API. The collector drives headless Firefox
through Playwright. The first `login` asks for a TOTP code. It then ticks
"Don't ask again", which plants a device-trust cookie that lasts for
years. Later runs sign in with no 2FA challenge.

Part of the **wealthdb** suite. See
[the architecture overview](../../DESIGN.md) for the bronze → silver →
gold model and [collectors/README.md](../README.md) for the shared
collector conventions. [DESIGN.md](DESIGN.md) holds the mechanism
detail.

## Verbs

| Verb | What it does |
| --- | --- |
| `login` | Signs in with headless Firefox and reads the TOTP code on stdin. `--check` probes the stored session. |
| `download` | Exports three CSVs per portfolio: the 13-column trade history, the per-wallet balance and the daily balance overview. Always the full history. |
| `load` | Builds the DuckDB silver. Replays the trades into daily holdings, checks them against the balance CSV, and fills missing USD prices. |
| `fetch-prices` | Fills USD prices into silver from Binance (crypto) and the ECB through Frankfurter (fiat cash). No key needed. |
| `prune` | Reclaims bronze and debug-cache disk. |
| `recompress` | Converts plain `.csv` bronze to `.csv.zst`. Manual, one-off. |
| `explore` | Discovery harness: Camoufox over VNC with a redacted network record. |

`./cointracking help` and `./cointracking <verb> --help` list the flags.

## Quick start

```sh
# 1. Build the image.
./cointracking build

# 2. Put the credentials in the env file and chmod it 0600.
#    cat > ~/.secrets/cointracking.env <<'EOF'
#    COINTRACKING_USERNAME=your-cointracking-email
#    COINTRACKING_PASSWORD=your-cointracking-password
#    EOF

# 3. Sign in once. Prompts for the 6-digit TOTP code.
./cointracking login

# 4. Pull a bronze dump of every linked portfolio.
./cointracking download

# 5. Build silver and fill missing USD prices.
./cointracking load
```

After the first `login`, `download` and `load` need no human until the
device-trust cookie expires.

More examples:

```sh
./cointracking login --check          # exit 0 if the stored session is signed in
./cointracking download --dry-run     # walk the pages, export nothing
./cointracking load --no-fetch-prices # build silver offline
./cointracking load --replay-only     # re-run the holdings replay only
./cointracking load --force           # delete silver, rebuild from all bronze
./cointracking fetch-prices --missing # fill price gaps only
./cointracking fetch-prices           # drop and re-fetch every price
```

`download --dry-run` uses the stored session and never signs in. On an
expired session it fails.

## Timezone

CoinTracking writes each trade's date in the timezone set on the
portfolio's account. `load` keeps the date as printed and converts it to
UTC. Name each portfolio's zone in `~/.config/cointracking.cfg` (under
`$XDG_CONFIG_HOME` when set). The wrapper sources that file:

```sh
COINTRACKING_TIMEZONES="cu_1001=Europe/Zurich cu_1002=America/New_York"
```

`load --timezone cu_<id>=<zone>` overrides the file. A portfolio named in
neither is read as UTC. A changed zone applies at the next `load`.
[DESIGN.md](DESIGN.md#timezone) has the details.

## Prices

`load` ends with a price fill, the same as `fetch-prices --missing`:

- It prices every coin held on any day in any portfolio.
- Crypto comes from Binance's public daily klines, in USDT.
- Fiat cash (EUR, CHF, …) comes from the ECB rates through Frankfurter.
- Stablecoins (USDT, USDC, DAI, …) get a fixed 1.0.
- The latest priced day is always fetched again. That row was an
  intraday price when it was written.

`fetch-prices` with no flag drops every fetched price and re-fetches the
whole held range. Use it to recover from bad provider data. An optional
`BINANCE_API_KEY` raises Binance's rate limit.

Binance keeps pair metadata after it purges a delisted coin's history. A
coin can therefore map to a symbol with no prices and stay unpriced.
[DESIGN.md](DESIGN.md#coverage-gap) has the details.

## Output

Everything lands in the collector's data dir
(`$XDG_DATA_HOME/wealthdb/cointracking/` by default):

```
<data-dir>/
├── 20260101T120000Z/            one bronze dump per run
│   ├── run.json                 manifest and status
│   └── cu_<id>/                 one dir per portfolio
│       ├── trades.csv.zst
│       ├── balance.csv.zst
│       └── overview.csv.zst
├── known_portfolios.json        every portfolio seen so far
└── cointracking.duckdb          silver
```

Silver is a DuckDB file, not SQLite. This is a recorded fleet exception;
[DESIGN.md](DESIGN.md#data-model) gives the reasons. Its tables:

| Table | Contents |
| --- | --- |
| `portfolios` | One row per linked CoinTracking account (`cu_<id>`), with its display name and the `timezone` its dates were read in. |
| `wallets` | One row per portfolio and wallet. A wallet is an exchange account or a self-custody wallet: the export's `Exchange` column. |
| `transactions` | One row per trade-CSV row. `type` as printed; buy, sell and fee amounts as `DECIMAL(38, 18)` with their currencies; `comment`, `lpn`. `trade_group`, `tx_id` and `occurred_local` hold the `Group`, `Tx-ID` and `Date` text as printed. `occurred_at` is `occurred_local` in UTC. |
| `positions_daily` | Computed by `load`: the amount per (day, portfolio, wallet, instrument). A row exists only on days the amount changed; a reader forward-fills. |
| `portfolio_prices` | CoinTracking's own daily price per (day, portfolio, coin), from the overview CSV, in the portfolio's main fiat (`quote_currency`). |
| `coin_prices` | Daily USD price per coin or fiat, with its `source` (`binance` or `frankfurter`). |
| `coin_mapping` | CoinTracking ticker → price-provider symbol, per provider. |
| `dump_runs` | One row per loaded dump, with the schema version it was loaded under. |

The columns are in [`migrations/`](migrations/). The gold adapter reads
this file read-only; [its design doc](../../wealthdb/docs/adapters/cointracking.md)
says how the tables map into gold.

## Re-discovery

`explore` is for the day CoinTracking changes a selector the headless
flow depends on. It launches Camoufox in the container's Xvfb display and
serves it over VNC. While the browser is driven by hand, it records a
redacted HAR, a network log, a click log and every downloaded file. A
Playwright trace is opt-in with `--trace`, because nothing redacts one.

```sh
./cointracking explore --fresh   # --fresh wipes the profile, so 2FA is asked again
#  explore: VNC ready on 127.0.0.1:<port>
#  explore: password (single-use):  <16 hex chars>
#  explore: tunnel from your laptop with
#  explore:   ssh -L <port>:127.0.0.1:<port> <host>
#  explore: then on the laptop:
#  explore:   open vnc://localhost:<port>
```

The record lands in a UTC-stamped subdir of the debug dir
(`${XDG_CACHE_HOME:-~/.cache}/wealthdb/debug/cointracking/` by default).
Closing the browser window or pressing Ctrl-C flushes it.
[collectors/README.md](../README.md#the-wrapper--the-wealthdb-collect-contract)
lists the env vars that move the secrets, data and debug dirs.

## Reclaiming disk

`prune` follows the fleet rules in
[collectors/README.md](../README.md#debug-artefacts-run-status-and-pruning-bronze).
For this collector it removes:

- whole dumps that did not finish;
- `screenshots/` from finished dumps, which `download --debug` writes;
- quiet entries of the debug dir, which `explore` writes.

It never touches `known_portfolios.json` or the silver file.

```sh
./cointracking prune --dry-run           # print the plan, delete nothing
./cointracking prune                     # reclaim
./cointracking prune --min-age-hours 6   # spare anything written in the last 6 hours
```

`download` compresses each CSV as it lands, and `load` reads `.csv.zst`
and plain `.csv` alike. `recompress` converts the plain `.csv` files in
finished dumps. It deletes an original only after the compressed copy
decompresses to the same sha256. A stopped sweep is safe to run again.
It rewrites `load` inputs, so it is manual only and never scheduled.

```sh
./cointracking recompress --dry-run   # print the plan, rewrite nothing
./cointracking recompress             # convert, with byte accounting
./cointracking load --force           # check: silver must come out the same
```

## Read-only

The collector only navigates, filters and exports. CoinTracking also
offers write surfaces: manual transaction entry, import management,
account settings. [AGENTS.md](AGENTS.md) lists what is allowed and what
is forbidden.
