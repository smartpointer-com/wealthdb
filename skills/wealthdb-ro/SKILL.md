---
name: wealthdb-ro
description: Query the owner's consolidated cross-institution investment portfolio — holdings, account and portfolio balances, net worth, asset allocation, transaction history, and investment returns (time-weighted TWR & money-weighted MWR/XIRR) — through the read-only `wealthdb` CLI. Use whenever a question is about what the owner holds, what an account or portfolio is worth, allocation, money in/out, or how an account / portfolio / the whole portfolio has performed over a period.
---

# wealthdb — portfolio queries (read-only)

`wealthdb` is a **read-only** command-line tool over one canonical database
that merges every bank, broker, pension, and crypto source the owner uses.
The point-in-time portfolio views live under one parent command,
**`wealthdb holdings <view>`** (`<view>` = positions, accounts, portfolios,
sources, or global); **`wealthdb transactions`** is the separate money-in/out
ledger. You only ever *read* from it. In a configured deployment, just run
the command; no setup, no paths, no flags required to connect.

## Hard rules (do not break)
- Allowed, all read-only: `wealthdb holdings <view>` (`<view>` is `global`, `sources`, `portfolios`, `accounts`, or `positions`), `wealthdb returns <view>` (`<view>` is `accounts`, `portfolios`, `sources`, or `global`), and `wealthdb transactions` — the data queries — plus `wealthdb status`, `snapshots`, `help` (harmless diagnostics — run freely).
- NEVER run anything that writes or mutates: `load`, `reload`, `reset`, `init`, `config`, and `wealthdb-collect` are forbidden. If you think you need to write, you are wrong — stop and just query.
- Add `-f json` whenever you will parse the output in code.
- Every monetary amount is a decimal **string** (e.g. `"12345.67"`). Convert to a number before doing arithmetic.

## Pick the right command
| You want… | Use |
|---|---|
| One grand total for everything — net worth in a single row | `holdings global` |
| Net worth / totals, one row per institution (silver source) | `holdings sources` |
| Net worth / totals, one row per portfolio (top level) | `holdings portfolios` |
| Balances per individual account | `holdings accounts` |
| Every individual holding (one row per instrument) | `holdings positions` |
| Trades, dividends, interest, fees, cash in/out over time | `transactions` |
| **How an account / portfolio / everything performed** over a period (return %) | `returns <view>` |

- The `holdings` views (`global`, `sources`, `portfolios`, `accounts`, `positions`) are **point-in-time**: a snapshot as of one date.
- `transactions` is a **date range** of events.
- Totals reconcile across the holdings views: `global` ≈ sum of `sources` ≈ sum of `portfolios` ≈ sum of `accounts` ≈ `positions --with-cash` (to within rounding).

## Dates
**holdings views (global / sources / portfolios / accounts / positions)** — `-d YYYY-MM-DD` is the as-of date (default: today). Each source contributes its latest snapshot on or before that date.

**transactions** — give the range as positional arguments (default: past 30 days):
| Argument | Meaning |
|---|---|
| `2026` | whole year |
| `2026-06` | whole month |
| `2026-06-15` | single day |
| `2026-01-01 2026-06-30` | inclusive range |
| `2026-01-01 -` | from date to today |
| `- 2026-06-30` | start of data to date |
| `- today` | all time |

## Flags (the query commands)
- `-f json|csv|table` — output format. Default is `table` (for humans). Use `json` to parse.
- `-x CCY` — currency for value columns. Default is the configured base currency. E.g. `-x CHF`, `-x EUR`.
- `-d` (as-of date) applies to **holdings** views; `transactions` and `returns` take a positional date range/window instead, not `-d`. `-p` (privacy/redact) works on all.
- `-C COLS` — choose columns: comma-separated names, `all`, or a delta like `-C +name,-quantity`. (Not on `holdings global`, which is a single fixed row.)
- `holdings positions` only: `--with-cash` — add one cash-balance row per account+currency.
- `returns` only: `--method`, `--period`, `--annualize`, `--netting`, `--inception` (see the Returns section).

## Columns you can rely on (each view is `wealthdb holdings <view>`)
- **global** (always exactly one row): `min_snapshot_date, max_snapshot_date, cash_balance_<CCY>, positions_value_<CCY>, total_value_<CCY>`. The two dates are the earliest/latest of the per-account snapshot dates, so you can see how stale any part of the total is.
- **positions**: `silver_source, snapshot_date, account, symbol, name, asset_class, currency, quantity, market_value, value_<CCY>`
- **accounts**: `silver_source, snapshot_date, account, account_kind, tax_wrapper, management_style, base_currency, positions_value, cash_balance, total_value, total_value_<CCY>`
- **sources**: one row per institution — `silver_source, snapshot_date, tax_wrapper, management_style, base_currency, positions_value, cash_balance, total_value, total_value_<CCY>` (base columns blank when the source mixes currencies / tax wrappers)
- **portfolios**: like accounts but the label column is `portfolio` (plus one sentinel row per source for accounts the bank didn't group)
- **transactions**: `silver_source, date, account, kind, symbol, description, currency, gross_amount, net_amount, value_<CCY>`

`silver_source` is the institution (`schwab`, `ubs`, `fidelity`, `swissquote`, `viac`, `relevate`, `cointracking`, `angellist`, `carta`, `equityzen`, `manual`). The value column shows as `value_USD`, `total_value_CHF`, etc. — matching your `-x`. Use `-C all` to list every column for a command.

Slice/group using these account attributes:
- `account_kind`: brokerage, cash, custody, crypto, … 
- `tax_wrapper`: taxable_personal, roth_ira, 529, pillar_3a, vested_benefits, trust_*, …
- `management_style`: self_directed, advisory, discretionary, automated

## Examples
```sh
# Whole-portfolio net worth in one row (USD)
wealthdb holdings global -f json

# Net worth in CHF as of a past date
wealthdb holdings global -d 2025-12-31 -x CHF -f json

# Current net worth by portfolio (USD), parseable
wealthdb holdings portfolios -f json

# Total value in CHF as of a past date
wealthdb holdings portfolios -d 2025-12-31 -x CHF -f json

# Every holding right now, including cash
wealthdb holdings positions --with-cash -f json

# Per-account balances as of year-end
wealthdb holdings accounts -d 2025-12-31 -f json

# All dividends/interest/trades in H1 2026
wealthdb transactions 2026-01-01 2026-06-30 -f json

# Full transaction history, newest first
wealthdb transactions - today -r -f json
```

## Returns — performance over time (`wealthdb returns <view>`)
Answers "how did it do?", not "what is it worth?". `<view>` is `accounts`,
`portfolios`, `sources`, or `global` (no `positions`). Two methods:
- **TWR** (time-weighted, the default & headline) — the return of the strategy,
  stripping out the timing of deposits/withdrawals. Use for "how did the
  investments perform?".
- **MWR** (money-weighted / XIRR) — the return *the owner actually earned*, which
  depends on when money went in/out. Use for "what did I actually make?". Add
  `--method both` to see both, or `--method mwr`.

Window is positional like `transactions` (default: since first snapshot → today):
`returns accounts 2025`, `returns global 2024-01-01 -`. Buckets: `--period
monthly|quarterly|annual|total` (default quarterly) — you get one row per bucket
plus a since-inception summary row. Other flags: `--annualize auto|always|never`,
`-x CCY` (historic FX), `-f json`, `-p`.

Columns: `silver_source, entity, period, start_<CCY>, end_<CCY>, net_flow_<CCY>,
twr_% , mwr_% , twr_ann_%, mwr_ann_%, quality`. Returns are **after fees and
taxes paid**.

**Read the `quality` column — it is load-bearing.** A `twr`/`mwr` of `n/a`
ALWAYS has a reason there; never report a blank or a bogus number. Common tags:
`nonpositive_base` (a mortgage/liability or net-negative entity — no meaningful
return; shown on its own line and excluded from rollups), `mwr_no_flows` (no
external cash flows — MWR undefined; e.g. manually-valued private holdings and
Carta/EquityZen, which are `nav_only`), `nav_only` / `nav_only_capital_call_risk`
(value-only source; its TWR omits capital-call timing — caveat it),
`since_data_inception` (since-inception means since the **first snapshot**, not
account opening), `staggered_inception` / `unmatched_transfers` /
`empty_bucket` (coarse-grain or stale-data approximations), `stale_snapshot`
(the end-of-bucket valuation is over ~3× the source's own snapshot cadence
old — treat the figure as stale). **Account-grain
returns are exact; portfolios/sources/global are best-effort.** Returns are
**not additive across grains** — don't sum account returns to get a portfolio
return; query the grain you want.

```sh
# Per-account TWR, quarterly, for 2025 (parseable)
wealthdb returns accounts 2025 -f json
# Whole-portfolio TWR + MWR since inception, annualized, in CHF
wealthdb returns global --method both -x CHF -f json
# Monthly TWR per institution over the last two years
wealthdb returns sources --period monthly 2024-01-01 - -f json
```

## Diagnostics (read-only, safe — use when data looks missing or stale)
- `wealthdb status` — one line per source: gold-side counts, the loaded watermark, and a `*` if newer data is waiting (not your concern to load — just a signal the data may be behind).
- `wealthdb status <source>` — detailed breakdown for one source (e.g. `wealthdb status fidelity`); add `-v` for taxonomy detail.
- `wealthdb snapshots <source>` (or `-a` for all sources) — the dates gold actually has data for, oldest first. Use to confirm whether a date you expected exists before concluding a balance is zero.
- `wealthdb help [<command>]` — built-in usage.

## Direct database access (advanced, optional)
Prefer the commands above — they hide schema, snapshot, and FX details. Drop
to raw SQL only when the CLI genuinely can't express what you need. All data
lives under the **`WEALTHDB_DATA_ROOT`** environment variable (set in a
configured deployment), and you have read-only access:
- **Gold** (the merged, canonical store the commands read — query this one): `$WEALTHDB_DATA_ROOT/wealthdb.db`, a **DuckDB** database.
- **Silver** (per-source, source-shaped inputs to gold): `$WEALTHDB_DATA_ROOT/<collector>/<collector>.db`, **SQLite** — except `cointracking/cointracking.duckdb` (DuckDB). Collectors: `schwab-web, schwab-api, ubs-web, ubs-psn, swissquote, fidelity-web, relevate, viac, cointracking, carta, angellist, equityzen, manual, fred`.
- **Bronze** (raw, as-downloaded CSV/JSON/PDF): `$WEALTHDB_DATA_ROOT/<collector>/<UTC-timestamp>/`. Rarely needed for analysis.

Always open these **read-only** (you have no write access, and a scheduled collection job may be writing). Don't assume column names — inspect with DuckDB `SHOW TABLES` / `DESCRIBE <table>` first.

## Gotchas
- There are **no row-filter flags** (no `--source`, `--account`, `--symbol`). To filter by source, account, asset class, etc., request `-f json` and filter/aggregate in your own code.
- A source contributes nothing before its first collected snapshot. If a holding is absent or zero for an early date, that is missing history, not a real zero — say so rather than reporting $0.
- A blank value column means no FX path to your `-x` currency existed for that line.
