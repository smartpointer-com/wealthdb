---
name: wealthdb-ro
description: Query the user's consolidated cross-institution investment portfolio — holdings, account and portfolio balances, net worth, asset allocation, transaction history, investment returns (time-weighted TWR & money-weighted MWR/XIRR), and categorised spending on the cash and card accounts — through the read-only `wealthdb` CLI. Use whenever a question is about current holdings, what an account or portfolio is worth, allocation, money in/out, how an account / portfolio / the whole portfolio has performed over a period, or what was spent and on what.
---

# wealthdb — portfolio queries (read-only)

`wealthdb` is a **read-only** command-line tool over one canonical database
that merges every configured bank, broker, pension, and crypto source.
The point-in-time portfolio views live under one parent command,
**`wealthdb holdings <view>`** (`<view>` = positions, accounts, portfolios,
sources, or global); **`wealthdb transactions`** is the separate money-in/out
ledger. You only ever *read* from it. In a configured deployment, just run
the command; no setup, no paths, no flags required to connect.

## Hard rules (do not break)
- Allowed, all read-only: `wealthdb holdings <view>` (`<view>` is `global`, `sources`, `portfolios`, `accounts`, or `positions`), `wealthdb returns <view>` (`<view>` is `accounts`, `portfolios`, `sources`, or `global`), `wealthdb spending <view>` (`<view>` is `summary`, `categories`, or `transactions`), and `wealthdb transactions` — the data queries — plus `wealthdb status`, `snapshots`, `help` (harmless diagnostics — run freely).
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
| **What was spent** — on what, where, how much per month | `spending <view>` |

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
- `-d` (as-of date) applies to **holdings** views; `transactions`, `returns` and `spending` take a positional date range/window instead, not `-d`. `-p` (privacy/redact) is accepted by every query command, but it redacts **per column**, not wholesale: identifier-shaped account / transaction ids are partly masked (last 2-4 characters kept), but the mask only fires on a value that is alphanumeric and contains a digit — an `account` column showing a nickname rather than a number, or an id written with dashes or spaces, prints in full; amounts, quantities and prices are masked in a format-dependent way (rendered `*****.**` / `***` in table output, an empty cell in CSV, and dropped from the object entirely in `-f json` — a redacted JSON listing has no `value_<CCY>` key at all); category and asset-class labels stay legible so a redacted listing is still readable; and the free-text columns are masked (`merchant`, `counterparty`, `description` and `merchant_signature` print `***` in every format). Elsewhere, treat a narrative column as unredacted unless it actually prints `***`.
- `-C COLS` — choose columns: comma-separated names, `all`, or a delta like `-C +name,-quantity`. (Not on `holdings global`, which is a single fixed row.)
- `holdings positions` only: `--with-cash` — add one cash-balance row per account+currency.
- `returns` only: `--method`, `--period`, `--annualize`, `--netting`, `--inception` (see the Returns section).
- `spending` only: `--period`, `--level` (see the Spending section).

## Columns you can rely on (each view is `wealthdb holdings <view>`)
- **global** (always exactly one row): `min_snapshot_date, max_snapshot_date, cash_balance_<CCY>, positions_value_<CCY>, total_value_<CCY>`. The two dates are the earliest/latest of the per-account snapshot dates, so you can see how stale any part of the total is.
- **positions**: `silver_source, snapshot_date, account, symbol, name, asset_class, currency, quantity, market_value, value_<CCY>`
- **accounts**: `silver_source, snapshot_date, account, account_kind, tax_wrapper, management_style, base_currency, positions_value, cash_balance, total_value, total_value_<CCY>`
- **sources**: one row per institution — `silver_source, snapshot_date, tax_wrapper, management_style, base_currency, positions_value, cash_balance, total_value, total_value_<CCY>` (base columns blank when the source mixes currencies / tax wrappers)
- **portfolios**: like accounts but the label column is `portfolio` (plus one sentinel row per source for accounts the bank didn't group)
- **transactions**: `silver_source, date, account, kind, symbol, description, currency, gross_amount, net_amount, value_<CCY>`
- **spending summary**: `period, txn_count, spend_<CCY>, refunds_<CCY>, net_spend_<CCY>`
- **spending categories**: the same plus `category` and `share_%`
- **spending transactions**: `silver_source, date, account, merchant, category, currency, net_amount, value_<CCY>` (add `category_primary`, `spend_detailed`, `provenance`, `counterparty`, `description` with `-C`). `category` is the display label; `spend_detailed` is the taxonomy value behind it, which is what a filter or a comparison should use.
- **the issuer's own view**: `issuer_category` (add with `-C`) is what the CARD PROVIDER called the line, translated into our vocabulary. It is a second opinion kept for reference, it disagrees with ours by design, and it must never be summed or mixed with the category columns. Blank means the issuer published nothing — not that it said "other".
- **Narrative columns, handle with care:** `counterparty`, `description` and `merchant_signature` are raw statement narratives — whatever text the bank put on the line — so they can name a private individual rather than a business, along with an address or a phone-shaped group. `merchant` is a name taken off such a narrative and belongs with them. Request them only when the question actually needs them, and never echo them wholesale into a summary.

`silver_source` is the institution (e.g. `schwab`, `ubs`, `fidelity`, …); the configured sources are your silver DBs under `$WEALTHDB_DATA_ROOT`. The value column shows as `value_USD`, `total_value_CHF`, etc. — matching your `-x`. Use `-C all` to list every column for a command.

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
- **MWR** (money-weighted / XIRR) — the return *actually earned* on the money, which
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
external cash flows — MWR undefined; e.g. manually-valued private holdings,
which are `nav_only`), `nav_only` / `nav_only_capital_call_risk`
(value-only source or a private-market window with no observed flows; its TWR
may omit capital-call timing — caveat it),
`since_data_inception` (since-inception means since the **first snapshot**, not
account opening), `staggered_inception` / `unmatched_transfers` /
`empty_bucket` (coarse-grain or stale-data approximations), `stale_snapshot`
(the end-of-bucket valuation is over ~3× the source's own snapshot cadence
old — treat the figure as stale). **Account-grain
returns are exact; portfolios/sources/global are best-effort.** Returns are
**not additive across grains** — don't sum account returns to get a portfolio
return; query the grain you want. Some sources are pure cash plumbing
(deposit banks): they emit **no returns rows at any view by design** — not
missing data; their balances and flows still feed the sources/global
aggregates, and global always includes everything.

```sh
# Per-account TWR, quarterly, for 2025 (parseable)
wealthdb returns accounts 2025 -f json
# Whole-portfolio TWR + MWR since inception, annualized, in CHF
wealthdb returns global --method both -x CHF -f json
# Monthly TWR per institution over the last two years
wealthdb returns sources --period monthly 2024-01-01 - -f json
```

## Spending — what was spent (`wealthdb spending <view>`)
Answers "where did the money go?", over the **cash and card accounts only**.
Investment activity is not spending and never appears here; neither do
own-account moves (card payments, funding wires, mortgage payments) —
those are transfers between accounts the product already tracks.

`<view>` is `summary` (one row per period), `categories` (one row per period
and category, with its share) or `transactions` (one row per spending line,
with merchant, category and the tier that decided it).

Window is positional like `transactions`, defaulting to the **trailing twelve
months**: `spending summary 2026`, `spending categories 2026-01-01 -`.
Buckets: `--period daily|weekly|monthly|quarterly|annual|total` (default
monthly). `--level primary|detailed` picks how coarse the `categories`
vocabulary is (default primary).

**Amounts are sign-split magnitudes**, not signed ledger amounts: `spend` and
`refunds` are both POSITIVE, and `net_spend = spend − refunds` is the number a
budget cares about. Categories reconcile — for any period, the category rows
sum to that period's summary row.

A category of `(uncategorized)` is the backlog: rows nothing could place. Say
so when it is a material share rather than folding it into a conclusion.
Category names in the CLI render as labels — "Cash withdrawal", "Card
spend", "Gift", "Other" — while the values behind them keep the taxonomy's
own spelling (`cash_withdrawal`, `card_spend`, `gift`, `other`). Both name
the same thing; quote whichever the reader is looking at.

`cash_withdrawal` is *unattributable* spending (ATM cash — what it bought has
no record), not a category of purchase. `card_spend` is the same idea for a
card bill with no purchases behind it: a card wealthdb does not itemise.
`gift` is a cash gift or family support: spending in its own right, with no
merchant behind it. The `merchant` column is filled only when a line's
resolved category is a vendored one *and* the merchant store has a name for
its signature. It is blank by design — never missing data — on every delta
line (`cash_withdrawal`, `card_spend`, `gift`, and `other`, the bucket for a
line placed nowhere else), on every uncategorised line, and on a vendored
line the store never named a signature for; `provenance` (add with `-C`) says
which tier decided.

```sh
# Monthly spend for 2026 so far
wealthdb spending summary 2026 -f json
# Where the money went last year, coarse categories, one bucket
wealthdb spending categories 2025 --period total -f json
# The individual lines behind a surprising month, in CHF
wealthdb spending transactions 2026-03 -x CHF -f json
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
- **Silver** (per-source, source-shaped inputs to gold): `$WEALTHDB_DATA_ROOT/<collector>/<collector>.db`, **SQLite** — one directory per collector (except `cointracking/cointracking.duckdb`, which is DuckDB).
- **Bronze** (raw, as-downloaded CSV/JSON/PDF): `$WEALTHDB_DATA_ROOT/<collector>/<UTC-timestamp>/`. Rarely needed for analysis.

Always open these **read-only** (you have no write access, and a scheduled collection job may be writing — while a `wealthdb load` holds the gold file, even a read-only open fails with a DuckDB lock error, which means retry, not corruption). Don't assume column names — inspect with DuckDB `SHOW TABLES` / `DESCRIBE <table>` first. Raw SQL bypasses `-p` entirely: the narrative columns above come back unredacted from gold and silver alike, so the same restraint applies with more force.

## Gotchas
- There are **no row-filter flags** (no `--source`, `--account`, `--symbol`, `--merchant`, `--category`). To filter by source, account, asset class, merchant, etc., request `-f json` and filter/aggregate in your own code.
- A source contributes nothing before its first collected snapshot. If a holding is absent or zero for an early date, that is missing history, not a real zero — say so rather than reporting $0.
- A blank value column means no FX path to your `-x` currency existed for that line.
