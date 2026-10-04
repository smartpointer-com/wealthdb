---
name: wealthdb-ro
description: Query the user's consolidated cross-institution investment portfolio through the read-only `wealthdb` CLI — holdings, account and portfolio balances, net worth, asset allocation, transaction history, investment returns (TWR and MWR/XIRR), categorised spending, categorised income, and the household cash flow statement with its Sankey. Use whenever a question is about current holdings, what an account or portfolio is worth, allocation, money in or out, how something performed over a period, what was spent and on what, what was received and from whom, or where the household's cash came from and where it went.
---

# wealthdb — portfolio queries (read-only)

`wealthdb` is a read-only CLI over one database that merges every configured
bank, card, broker, pension and crypto source, plus the property, loans and
private holdings recorded by hand. A house, a mortgage and a venture fund are
accounts like any other. In a configured deployment just run the command: no
setup, no paths, no connection flags.

## Hard rules

1. **Run only these commands.** `holdings`, `transactions`, `returns`,
   `spending`, `income`, `cashflow`, `status`, `snapshots`, `help`, `version`.
2. **Anything else is forbidden**, whether or not it is listed here. `load`,
   `reload`, `reset`, `init`, `config`, `compact`, `categorize`,
   `resolve-symbols`, `web-config`, `web-materialize` and `wealthdb-collect`
   all write; `mcp-serve` and `mcp-config` serve other clients. If you think you need to write, you are wrong — just query.
3. **One number for a whole window: add `--period total`.** The default is
   one row per month. Never add monthly rows up yourself; let the command
   total them.
4. **There are no row-filter flags.** `--source`, `--account`, `--category`,
   `--symbol` do not exist. Run the view, then filter the output. The table
   is one row per line, so `| grep -i text` picks the rows for a name, a
   wrapper, a category or a source; use `-f json | jq` only to sort.
5. **Read tables; parse JSON.** The default table is for reading. Add
   `-f json` only when you pipe into `jq`. JSON keys are the table headers,
   currency suffix included: `value_USD`, `net_spend_USD`, `total_value_USD`,
   `net_USD`, `"twr_%"`. There is no `.value` or `.net_spend` key. Every money
   value is a decimal **string** (`"12345.67"`): `tonumber` before comparing
   or sorting. The output is one JSON **array**: start every filter with `.[] |` or
   `map(...)`; a bare `select(...)` fails with "Cannot index array". A column
   with no value is **omitted** from the object, so write
   `map(select(.value_USD))` before sorting on it, never compare to `""`. A
   key with a `%` needs quotes: `."twr_%"`, `."share_%"`. A return that cannot
   be computed is the string `"n/a"`, with the reason in `quality`.
6. **Signs.** In the line views (`transactions`, `spending transactions`,
   `cashflow transactions`) money leaving is **negative**: the biggest
   purchase is the most negative value. In `summary`, `categories` and `types`,
   `spend`, `refunds` and `income` are positive magnitudes.
7. **Run `wealthdb help <command>` for anything this file does not cover**
   (e.g. `wealthdb help cashflow`); `wealthdb <command> -h` prints the same
   full help, and `wealthdb help` lists every command.

## Recipes

The command for each common question. Replace the window: `2025` is a whole
year, `2026-03` a whole month, `2025-01-01 2025-06-30` a range.

- **Net worth now / at a date / in CHF** — `wealthdb holdings global`, then
  `-d 2025-12-31` or `-x CHF`. One row: cash, positions, total.
- **What each account holds, with its tax wrapper** — `wealthdb holdings accounts`
- **One institution's total** — `wealthdb holdings sources | grep -i schwab`
  (one row per institution; never add its accounts up by hand)
- **One wrapper, one account, a property, a loan** —
  `wealthdb holdings accounts | grep -i roth` (or `401`, `mortgage`)
- **Largest holdings** —
  `wealthdb holdings positions -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber) | reverse | .[0:5]'`
  (a house or a loan is a position too; add `select(.asset_class != "real_estate")` for securities only)
- **Return of the whole portfolio / of each account over a window** —
  `wealthdb returns global 2025 --period total` / `wealthdb returns accounts 2025 --period total`
- **Return of one account** —
  `wealthdb returns accounts 2025 --period total | grep -i 'joint brokerage'`
  (the account column is `entity`; read `twr_%`, and `quality` if it is `n/a`)
- **Best account by return** —
  `wealthdb returns accounts 2025 --period total -f json | jq 'map(select(."twr_%" != "n/a")) | sort_by(."twr_%"|tonumber) | reverse | .[0:3]'`
  (a return that cannot be computed is the string `"n/a"`; drop those rows first)
- **Total spent in a window** — `wealthdb spending summary 2025 --period total`
- **Spending by category** — `wealthdb spending categories 2025 --period total`
  (broad groups) or add `--level detailed` (groceries, restaurants, flights, …)
- **One named category** —
  `wealthdb spending categories 2026 --period total --level detailed | grep -i grocer`
- **Biggest purchases in a month** —
  `wealthdb spending transactions 2026-03 -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber) | .[0:5]'`
  (spending lines are negative, so the most negative come first)
- **Total income / income by type** —
  `wealthdb income summary 2025 --period total` / `wealthdb income types 2025 --period total`
- **One income type (dividends, interest, salary)** —
  `wealthdb income types 2025 --period total | grep -i dividend`
- **Net cash flow of a year** — `wealthdb cashflow summary 2025 --period total`
- **Where the cash went, by class** — `wealthdb cashflow flows 2025 --period total --level class`
- **Mortgage payments, retirement or education contributions** —
  `wealthdb cashflow flows 2025 --period total --level class | grep -i -E 'mortgage|retirement|education'`
- **Largest transactions of any kind** —
  `wealthdb transactions 2025 -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber|fabs) | reverse | .[0:10]'`
- **Is the data current / which dates exist** — `wealthdb status` / `wealthdb snapshots <source>`

## Pick the command

| The question | The command |
|---|---|
| Net worth, one single row | `holdings global` |
| Net worth per institution | `holdings sources` |
| Net worth per portfolio | `holdings portfolios` |
| Balance per account | `holdings accounts` |
| Every individual holding | `holdings positions` |
| Trades, dividends, interest, fees, cash in and out over time | `transactions` |
| How something **performed** over a period (return %) | `returns <view>` |
| **What was spent**, on what | `spending <view>` |
| **What was received**, from whom | `income <view>` |
| **Where the household's cash came from and went** | `cashflow <view>` |
| Mortgage or loan payments, money into retirement, education or health plans | `cashflow flows` (never `spending`: own-account moves are not spending) |

| Command | Views |
|---|---|
| `holdings` | `global`, `sources`, `portfolios`, `accounts`, `positions` |
| `returns` | `global`, `sources`, `portfolios`, `accounts` |
| `spending` | `summary`, `categories`, `transactions` |
| `income` | `summary`, `types`, `transactions` |
| `cashflow` | `summary`, `flows`, `sankey`, `transactions`, `coverage` |
| `transactions` | none — it is one command |

`holdings` views are **point-in-time** (a snapshot as of one date). Everything
else covers a **date range**. Holdings totals reconcile: `global` ≈ sum of
`sources` ≈ sum of `portfolios` ≈ sum of `accounts` ≈ `positions --with-cash`,
to within rounding.

## Dates

**`holdings` only:** `-d YYYY-MM-DD` is the as-of date (default: today). Each
source contributes its latest snapshot on or before that date.

**Every other command** takes the window as positional arguments — never `-d`:

| Argument | Meaning |
|---|---|
| `2026` | that whole year |
| `2026-06` | that whole month |
| `2026-06-15` | that single day |
| `2026-01-01 2026-06-30` | inclusive range |
| `2026-01-01 -` | that date to today |
| `- 2026-06-30` | start of data to that date |
| `- today` | all time |

Defaults when the window is omitted: `transactions` the past 30 days;
`returns` since the first snapshot; `spending`, `income` and `cashflow` the
trailing twelve months. "Last year" and "this year so far" are calendar
windows: resolve them from today's date and pass them explicitly (`2025`, or
`2026-01-01 today`).

## Flags

| Flag | Meaning |
|---|---|
| `-f table\|json\|csv\|csv_plain` | output format (default `table`) |
| `-x CCY` | output currency (default: the configured base currency) |
| `-C COLS` | columns: names, `default`, `all`, or a delta like `-C +quality,-net_flow` |
| `-p` | redact (see below) |
| `--period` | bucket size, on `returns`, `spending`, `income`, `cashflow` |
| `--level` | vocabulary grain, on `spending`, `income`, `cashflow` |

`-C all` lists every available column for a view. Use the **registry name** in
`-C`, not the rendered header: ask for `value`, `net_flow`, `total_value_outccy`
or `share`, and the header comes back as `value_USD`, `net_flow_CHF`,
`total_value_CHF` or `share_%`, matching your `-x`.

**What `-p` redacts**, per column:

- Amounts, quantities and prices → `*****.**` in table, an empty CSV cell, and
  **dropped from the object entirely** in JSON.
- Free text — `merchant`, `payer`, `counterparty`, `description`,
  `merchant_signature`, `payer_signature` → `***`.
- Identifier-shaped account and transaction ids → partly masked. The mask only
  fires on a value that is alphanumeric **and** contains a digit, so an account
  column showing a nickname prints in full.
- Categories, income types, cashflow sections/classes/groups and all `share_%`
  columns stay legible, so a redacted listing is still readable.

Treat any other column as unredacted unless it actually prints `***`.

## Default columns per view

| View | Columns |
|---|---|
| `holdings global` | `min_snapshot_date, max_snapshot_date, cash_balance_outccy, positions_value_outccy, total_value_outccy` (always exactly one row) |
| `holdings positions` | `silver_source, snapshot_date, account, symbol, position_key, asset_class, vehicle, currency, quantity, market_value, value` |
| `holdings accounts` | `silver_source, snapshot_date, account, account_kind, tax_wrapper, management_style, base_currency, positions_value, cash_balance, total_value, total_value_outccy` |
| `holdings sources` | as `accounts`, one row per institution (base columns blank when the source mixes currencies or wrappers) |
| `holdings portfolios` | as `accounts`, with `portfolio` as the label column, plus one sentinel row per source for accounts the bank did not group |
| `transactions` | `silver_source, date, account, kind, symbol, instrument_id, currency, net_amount, value` |
| `returns <view>` | `silver_source, entity, period, start_value, end_value, net_flow, twr, mwr, quality` |
| `spending summary` | `period, txn_count, spend, refunds, net_spend` |
| `spending categories` | the same plus `category` and `share` |
| `spending transactions` | `silver_source, date, account, merchant, category, currency, net_amount, value` |
| `income summary` | `period, txn_count, income, reversals, net_income` |
| `income types` | the same plus `type` and `share` |
| `income transactions` | `silver_source, date, account, kind, payer, income_type, provenance, currency, net_amount, value` |
| `cashflow summary` | `period, operating_in, operating_out, operating, investing, financing, vehicles, net_cash_flow` |
| `cashflow flows` | `period, section, class, group, txn_count, inflow, outflow, net, share` |
| `cashflow sankey` | `stage, source, target, value, share` |
| `cashflow transactions` | `silver_source, date, account, kind, section, class, group, name, currency, net_amount, value` |
| `cashflow coverage` | `period, silver_source, account, currency, ledger, measured, gap, status` |

`silver_source` is the institution (`schwab`, `ubs`, `fidelity`, …). Slice or
group by these account attributes, available via `-C` where the view has them:

- `account_kind`: brokerage, cash, custody, crypto, …
- `tax_wrapper`: taxable_personal, roth_ira, 529, pillar_3a, vested_benefits, trust_*, …
- `management_style`: self_directed, advisory, discretionary, automated

**Display label vs. taxonomy value.** `category`, `income_type`, `class` and
`group` are display labels ("Cash withdrawal", "Consumption"). The values
behind them keep the taxonomy spelling (`cash_withdrawal`, `consumption`) and
live in `spend_detailed`, `income_type_id`, `class_id` and `group_id`. **Filter
and compare on the value, quote whichever the reader is looking at.**

**Narrative columns, handle with care.** `counterparty`, `description`,
`merchant_signature` and `payer_signature` are raw statement narratives, so they
can name a private individual along with an address or a phone-shaped group.
`merchant` and `payer` are names taken off such a narrative; on an inbound wire
a `payer` is usually a person. Request them only when the question needs them,
and never echo them wholesale into a summary.

## Examples

```sh
wealthdb holdings global -f json                      # net worth, one row
wealthdb holdings global -d 2025-12-31 -x CHF -f json # net worth at year-end, in CHF
wealthdb holdings positions --with-cash -f json       # every holding, cash included
wealthdb holdings accounts -d 2025-12-31 -f json      # per-account balances at year-end
wealthdb transactions 2026-01-01 2026-06-30 -f json   # everything booked in H1
wealthdb transactions - today -r -f json              # full history, newest first
```

## Returns — performance over time

Answers "how did it do?", not "what is it worth?". Two methods:

- **TWR** (time-weighted, the default) — the return of the strategy, stripping
  out the timing of deposits and withdrawals. Use for "how did the investments
  perform?".
- **MWR** (money-weighted / XIRR) — the return actually earned on the money,
  which depends on when money went in and out. Use for "what did I make?".

`--method twr|mwr|both`. `--period monthly|quarterly|annual|total` (default
quarterly) gives one row per bucket plus a since-inception summary row. Also
`--annualize auto|always|never`, `--netting`, `--inception` — see
`wealthdb returns -h`. Returns are **after fees and taxes paid**.

**Read the `quality` column — it is load-bearing.** A `twr` or `mwr` of `n/a`
always has a reason there; never report a blank or a bogus number.

| Tag | Means |
|---|---|
| `nonpositive_base` | a liability or net-negative entity — no meaningful return; shown on its own line and left out of rollups |
| `mwr_no_flows` | no external cash flows, so MWR is undefined |
| `nav_only`, `nav_only_capital_call_risk` | value-only source, or a private-market window with no observed flows — its TWR may omit capital-call timing, so caveat it |
| `since_data_inception` | since-inception means since the **first snapshot**, not since the account opened |
| `staggered_inception`, `unmatched_transfers`, `empty_bucket` | coarse-grain or stale-data approximations |
| `stale_snapshot` | the end-of-bucket valuation is far older than the source's own snapshot cadence — treat the figure as stale |

**Account-grain returns are exact; portfolios, sources and global are
best-effort.** Returns are **not additive across grains** — never sum account
returns to get a portfolio return; query the grain you want. Some sources are
pure cash plumbing and emit **no returns rows at any view by design**; their
balances and flows still feed the sources and global aggregates.

```sh
wealthdb returns accounts 2025 -f json
wealthdb returns global --method both -x CHF -f json
wealthdb returns sources --period monthly 2024-01-01 - -f json
```

## Spending — what was spent

Covers every account except those the config excludes. Investment activity is
never spending, and neither are own-account moves (card payments, funding
wires, mortgage payments) — those are transfers the product already tracks.

- `--period daily|weekly|monthly|quarterly|annual|total` (default monthly;
  `total` for one figure over the window).
- `--level primary|detailed` sets how coarse `categories` is (default primary:
  about a dozen broad groups such as "Food and drink"). A category someone
  names — groceries, restaurants, flights, gyms — is a **detailed** category:
  add `--level detailed` and `grep` for it.
- Mortgage payments, card payments and transfers to own accounts are not
  spending and appear in no spending view. Mortgage and loan payments are in
  `cashflow flows` under the `financing` section.
- **Amounts are sign-split magnitudes, not signed ledger amounts:** `spend` and
  `refunds` are both POSITIVE, and `net_spend = spend − refunds` is the number
  a budget cares about.
- Category rows sum to the summary row for the same period.
- `(uncategorized)` is the backlog — rows nothing could place. Say so when it
  is a material share rather than folding it into a conclusion.
- `cash_withdrawal` and `card_spend` are *unattributable* spending, not kinds
  of purchase: ATM cash, and a card bill with no itemised purchases behind it.
  `gift` is a cash gift or family support.
- `merchant` is the store's name for the signature, falling back to the raw
  signature where no store named one — so a merchant that reads like a raw
  narrative fold is normal, not missing data.
- `provenance` (via `-C`) says which tier decided the category.

```sh
wealthdb spending summary 2026 -f json
wealthdb spending categories 2025 --period total -f json
wealthdb spending transactions 2026-03 -x CHF -f json
```

## Income — what was received

The mirror of spending: same window default, same `--period`, same
`-f`/`-C`/`-x`/`-p`.

- `income` and `reversals` are both POSITIVE and `net_income = income −
  reversals`. A reversal is a receipt clawed back, netted inside its own type.
- **Gross as booked.** Tax withheld at source is on the SPENDING side as
  `Withholding tax`, so `income − spending` subtracts it exactly once.
  `-C +withheld` shows it as a memo beside the income it came from; it is never
  subtracted from `net_income`.
- **A `distribution` is capital until proven income.** A private fund returns
  contributed capital first, so its distributions are `capital_return` and stay
  OUT of the base. A public fund's realised-gain payout (`capital_gain`) stays
  in, as `Distributions`.
- **The payer of a dividend is the instrument** — the company, issuer or
  protocol. On a deposit it is the payer's name or the raw signature fold; on
  an own-account move or a gift it is blank.
- `(uncategorized)` is the backlog, as on the spending side. Most income is
  placed by its transaction kind, so a large uncategorised share means bank
  deposits specifically.
- `--level` defaults to **`detailed`** here, not `primary`: the income taxonomy
  has one primary, so the primary level folds everything into `Income`.

```sh
wealthdb income types 2025 --period annual -f json
wealthdb income summary 2026 -C +withheld -f json
wealthdb income transactions 2026-03 -f json
```

## Cash flow — where the household's cash came from and went

The cash flow statement, and the edge list of its Sankey diagram.

- **The household is the accounts in its own tax wrappers.** Retirement plans,
  trusts, charitable and education vehicles are *vehicles* it pays into and
  draws on, and a move across that boundary is a flow. Moves between two of the
  household's own accounts are invisible.
- **Positive is cash arriving, negative is cash leaving.**
- Buying and selling is shown **net per period**, never as two gross bands.
- A node is a **net**, and `--level` decides what nets: `section`, `class` or
  `group` (default `group`). `share` is over the hub at the level drawn.
- `--investing whole|class` (default `whole`) nets investing as one node or
  gives each asset class its own.
- `operating`, `investing`, `financing` and `vehicles` sum to
  `net_cash_flow`. `operating_in` and `operating_out` are positive
  MAGNITUDES and `operating` is their difference, so do not add those
  two into the total yourself.
- `vehicles` is every plan together: retirement, education and health. For
  one of them, read `cashflow flows --level class` and take its row
  ("Retirement savings", "Education savings", "Health"); `financing` holds
  the mortgage.

**`operating_in` and `operating_out` are NOT what `wealthdb income` and
`wealthdb spending` report**, which is why they are not named after them. The
totals here are smaller, because the vehicles' own income and spending are
outside the boundary and the two families' scopes differ. Never present one as
a check on the other.

**Three flags are refused rather than ignored**, so a mistake is an error and
not a wrong answer: `sankey --period` (a diagram is a window, not a series —
loop over the years), `sankey --level section` (no inner column to draw), and
`coverage -x` (every account is reported in its own currency).

**`coverage` answers "which accounts can the statement be trusted on?"** — per
account and period, the cash delta its transactions imply against the delta its
own balances show. Read `status` first:

| `status` | Means |
|---|---|
| `measured` | a real disagreement — sort by `gap` |
| `obscured` | the account carries more unsigned FX volume than the gap, so nothing can be concluded |
| `opening` | the balance series began mid-period, so the difference is not a delta |
| `unmeasurable` | no balance history at all |

```sh
wealthdb cashflow summary 2025 --period quarterly -f json
wealthdb cashflow sankey 2025 -f json              # the diagram's edges
wealthdb cashflow flows 2025 --level class -f json
wealthdb cashflow coverage 2025 -f json            # where the statement is weak
```

`wealthdb transactions` also carries `cashflow_section`, `cashflow_class` and
`cashflow_group` behind `-C`, beside the spending and income columns. They are
blank where the statement does not draw the row (a move inside the household,
or a transaction kind with no canonical direction).

## Diagnostics (read-only, safe)

- `wealthdb status` — one line per source: gold-side counts, the loaded
  watermark, and a `*` if newer data is waiting. Not yours to load; it just
  signals the data may be behind.
- `wealthdb status <source>` — detail for one source; add `-v` for taxonomy and
  coverage counters.
- `wealthdb snapshots <source>` (or `-a`) — the dates gold actually has data
  for, oldest first. Use it to confirm a date exists before concluding a balance
  is zero.

## Gotchas

- **There are no row-filter flags** — no `--source`, `--account`, `--symbol`,
  `--merchant`, `--category`. To filter, `grep -i` the table or request
  `-f json` and `select` in `jq`.
- A source contributes nothing before its first collected snapshot. An absent
  or zero holding at an early date is missing history, not a real zero — say so
  rather than reporting $0.
- A blank value column means no FX path to your `-x` currency existed for that
  line.
- Raw SQL against the databases under `$WEALTHDB_DATA_ROOT` is possible but
  rarely needed, and it bypasses `-p` entirely. Prefer the commands above; they
  hide schema, snapshot and FX details.

## Without a shell

This skill is for an agent with a shell. An agent without one reaches the
same reports through the MCP server (`wealthdb mcp`, see mcp/README.md): its
tools take filters, sort and paging where this skill pipes through `grep` and
`jq`, and its privacy endpoint redacts as `-p` does.
