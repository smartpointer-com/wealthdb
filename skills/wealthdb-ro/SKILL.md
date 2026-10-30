---
name: wealthdb-ro
description: Query the household's consolidated portfolio through the read-only `wealthdb` CLI — holdings and net worth, account balances, allocation and look-through exposure (class, sector, size, country, currency, credit, maturity, issuer, legal situs), transactions, returns (TWR, MWR), realized and unrealized gains with cost basis and lots, yields and rate sensitivity (duration, DV01), spending and income by category, the income the holdings are expected to pay and when, and the household cash flow statement and projection. Use for any question about what is owned, what it is worth or yields, how it performed, what was gained, spent or received, or where the cash went and will go.
---

# wealthdb — portfolio queries (read-only)

`wealthdb` is a read-only CLI over one database that merges every bank,
card, broker, pension and crypto source, plus the property, loans and
private holdings recorded by hand; a house, a mortgage or a venture fund
is an account like any other. Just run the command: no setup, paths or
connection flags.

## Hard rules

1. **Run only** `holdings`, `exposure`, `yield`, `transactions`,
   `returns`, `gains`, `spending`, `income`, `cashflow`, `status`,
   `snapshots`, `help`, `version`. Everything else writes or serves other
   clients. Put `-r` between `wealthdb` and the command on every call
   (`wealthdb -r holdings global`); an `-r` after `transactions` or
   `gains realized` means newest first instead.
2. **One figure for a whole window: `--period total`.** The default is
   one row per month (per quarter for `returns`); never add rows up
   yourself. Income's forward views and `cashflow projection` refuse it
   and print a `total` row already; `cashflow sankey` takes no `--period`.
3. **No row filters** (`--source`, `--account`, `--symbol` do not exist):
   run the view, then `| grep -i text` the table.
4. **Read tables; use JSON only to sort.** `-f json | jq`: the output is
   one array (`.[]` or `map(...)`); keys are the table headers with the
   currency suffix (`value_USD`, `twr_pct`), so pass `-x USD` to know the
   key; every value is a string (`tonumber`); an empty column is omitted
   (`map(select(.value_USD))` first). A return that cannot be computed is
   `"n/a"`, reason in `quality`.
5. **Signs.** In line views (`transactions`, `spending transactions`,
   `cashflow transactions`) money leaving is negative. In `summary`,
   `categories` and `types`, `spend`, `refunds` and `income` are positive.
6. `wealthdb -r help <command>` prints everything this file leaves out
   (`help positions` for a holdings view's columns); `-C all` prints every
   column.
7. **One recipe call normally answers.** Once the figure is in hand,
   answer with it and its `quality`; do not re-run views to confirm it.

## Recipes

Windows: `2025` a year, `2026-03` a month, `2025-01-01 2025-06-30` a
range. `-d YYYY-MM-DD` is the as-of day of `holdings`, `exposure`, `yield`.

- **Net worth or total cash, now / at a date / in CHF** — `wealthdb -r holdings global`
  (one row: cash, positions, total), then `-d 2025-12-31` or `-x CHF`
- **Each account, with its tax wrapper** — `wealthdb -r holdings accounts`;
  one institution: `wealthdb -r holdings sources | grep -i schwab`; one
  wrapper, property or loan: `wealthdb -r holdings accounts | grep -i roth`
  (or `401`, `mortgage`)
- **Largest holdings** —
  `wealthdb -r holdings positions -x USD -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber) | reverse | .[0:5]'`
  (a house or a loan is a position too)
- **A holding's cost basis and unrealized gain** —
  `wealthdb -r holdings positions -C +cost_basis,unrealized_gain,basis_stamp | grep -i btc`
  (`basis_stamp`: `stated/…` the institution's, `derived/…` computed,
  `rebuilt/…` from the trades); its lots: `wealthdb -r gains lots | grep -i btc`
- **Allocation once funds are looked through** — `wealthdb -r exposure classes`;
  then `sectors`, `sizes`, `countries`, `currencies` (`-d 2025-12-31` for a
  past day, `--period quarterly 2025` for a series)
- **One company, directly and through funds** —
  `wealthdb -r exposure securities | grep -i <company>` (`value` is the total)
- **Largest issuers, stock, bonds and deposits together** —
  `wealthdb -r exposure issuers | head -12`; how much sits with the top
  ten: `wealthdb -r exposure issuers -f json | jq '.[9].cumulative_share_pct'`
- **The debt by rating, maturity, coupon kind** — `wealthdb -r exposure credit`
  (`grade` IG or HY), `maturities`, `coupons`; the stocks by cycle:
  `wealthdb -r exposure cycles`
- **Where holdings legally sit** (an estate question) — `wealthdb -r exposure situs`;
  one fund's: `wealthdb -r exposure funds -C fund,name,domicile,situs,situs_basis`
- **How far to trust exposure** — `wealthdb -r exposure coverage`
  (`| grep undeclared`: private holdings with no record; `-f ndjson`: the
  records still to write); each fund's data age, and which fund stands in
  for it: `wealthdb -r exposure funds` (`via` reads `proxy` or `declared`;
  `proxy` is a key)
- **What the holdings yield** — `wealthdb -r yield global`
  (`income_yield_pct`: the next twelve months' expected income over value;
  the `total` row weighs the currencies); per account: `wealthdb -r yield accounts`
- **Cash that earns too little** — `wealthdb -r yield cash` (`gap_pct` is
  the rate less the currency's benchmark; term deposits and money-market
  funds sit in the same table)
- **A fund's yield beside its rate risk** — `wealthdb -r yield funds`
  (`implied_yield`, `duration`, `dv01`; a fund is a row here, not in
  `securities`); one bond or company:
  `wealthdb -r yield securities -C +rating,payout,dividend_growth,coverage | grep -i <name>`
  (`rating` AAA to B or `unrated`; `coverage` is operating income over
  interest expense)
- **Highest dividend yield** —
  `wealthdb -r yield securities -f json | jq 'map(select(.dividend_yield_pct)) | sort_by(.dividend_yield_pct|tonumber) | reverse | .[0:5]'`
- **What a rate rise does** — `wealthdb -r yield global` (`dv01`: the
  value lost for a rise of 0.01%, mortgages netted); per currency and
  tenor: `wealthdb -r yield curve`; to the income:
  `wealthdb -r yield global -C +income_100bp`
- **Return of the portfolio / each account** —
  `wealthdb -r returns global 2025 --period total` / `wealthdb -r returns accounts 2025 --period total`
  (the account column is `entity`); best account:
  `wealthdb -r returns accounts 2025 --period total -f json | jq 'map(select(.twr_pct != "n/a")) | sort_by(.twr_pct|tonumber) | reverse | .[0:3]'`
- **Realized gains of a year** — `wealthdb -r gains summary 2025 --period total`
  (`realized`); unrealized now: `wealthdb -r gains summary --period total`
  (`unrealized_end`; never sum positions); lots sold: `wealthdb -r gains realized 2025`
- **Holdings that gained most** —
  `wealthdb -r gains positions 2025 -x USD -f json | jq 'map(select(.gain_USD and .symbol)) | sort_by(.gain_USD|tonumber) | reverse | .[0:5]'`
  (one row per account and holding)
- **Where gains are missing** — `wealthdb -r gains coverage 2025` (`verdict`
  `no_basis`, `no_realized` or `partial`: missing, not zero); against the
  statements: `wealthdb -r gains check 2025`
- **Spent in a window / by category** — `wealthdb -r spending summary 2025 --period total` /
  `wealthdb -r spending categories 2025 --period total` (`--level detailed`
  for groceries, restaurants, flights …, then `| grep -i grocer`)
- **Biggest purchases in a month** —
  `wealthdb -r spending transactions 2026-03 -x USD -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber) | .[0:5]'`
- **Income / by type** — `wealthdb -r income summary 2025 --period total` /
  `wealthdb -r income types 2025 --period total | grep -i dividend`
- **This year's income, received and still expected** —
  `wealthdb -r income projection 2026` (a row per month, then `total` and
  `monthly_average`; `status` reads `received`, `expected` or `mixed`;
  `basis` says what the expected part rests on)
- **What the holdings will pay next** — `wealthdb -r income projection`
  (the months after this one); by date: `wealthdb -r income calendar`
  (kind `principal` is a maturity, outside the income totals); per
  holding: `wealthdb -r income positions 2026` (`next_12m`); per account:
  `wealthdb -r income accounts`
- **Net cash flow / by class** — `wealthdb -r cashflow summary 2025 --period total` /
  `wealthdb -r cashflow flows 2025 --period total --level class`
  (`| grep -i -E 'mortgage|retirement|education'` for those payments)
- **The months ahead** — `wealthdb -r cashflow projection --period quarterly`
  (the expected income against the trailing year's outflows, mortgages to
  their maturity; `quality` says which part rests on what)
- **Largest transactions** —
  `wealthdb -r transactions 2025 -x USD -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber|fabs) | reverse | .[0:10]'`
- **Is the data current / which dates exist** — `wealthdb -r status` /
  `wealthdb -r snapshots <source>`

## Pick the command

| The question | Command | Views |
|---|---|---|
| Net worth, balances, holdings (as of a day) | `holdings` | `global`, `sources`, `portfolios`, `accounts`, `positions` |
| What is **really owned** once funds are looked through; where holdings legally sit | `exposure` | `classes`, `sectors`, `sizes`, `countries`, `currencies`, `credit`, `maturities`, `coupons`, `cycles`, `situs`, `securities`, `issuers`, `funds`, `coverage` |
| What the holdings **yield**; rates, duration, rate risk; cash against its benchmark | `yield` | `global`, `sources`, `portfolios`, `accounts`, `positions`, `securities`, `funds`, `cash`, `curve`, `coverage` |
| Trades, dividends, fees, cash in and out | `transactions` | — |
| How something **performed** (return %) | `returns` | `global`, `sources`, `portfolios`, `accounts`; `stated` (a statement's periods beside the engine's; no `-x`, no return flags) |
| **Gained or lost**; cost basis; tax lots | `gains` | `summary`, `sources`, `portfolios`, `accounts`, `positions`, `realized`, `lots`, `coverage`, `check` |
| **Spent**, on what | `spending` | `summary`, `categories`, `transactions` |
| **Received**, from whom; what the holdings are **expected to pay** | `income` | `summary`, `types`, `transactions`; forward: `projection`, `calendar`, `sources`, `portfolios`, `accounts`, `positions` |
| Where the cash came from and went; mortgage and plan payments; the months ahead | `cashflow` | `summary`, `flows`, `sankey`, `transactions`, `coverage`, `projection` |

## Dates

`holdings`, `exposure`, `yield`: `-d YYYY-MM-DD` is the day (default
today); each account gives its latest snapshot on or before it. `exposure`
and `yield` take `--period monthly|quarterly|annual [FROM [TO]]` for a
series instead.

Every other command takes the window as positionals, never `-d`: `2026`,
`2026-06`, `2026-06-15`, `2026-01-01 2026-06-30`, `2026-01-01 -` (to
today), `- 2026-06-30` (from the start), `- today` (all time). Defaults:
`transactions` the past 30 days; `returns` since the first snapshot; the
rest the trailing twelve months. Resolve "last year" and "this year" from
today's date (`2025`, `2026-01-01 today`).

Income's forward views (`projection`, `calendar`, `sources`, `portfolios`,
`accounts`, `positions`) and `cashflow projection` take a window that may
lie in the future; a lone FROM is its own span (`2027` is all of 2027),
the default is the months after this one, and an open bound (`-`) is
refused. `cashflow projection` starts at next month at the earliest.

## Flags and columns

`-f table|json|csv` (`ndjson` on `exposure coverage`, `yield coverage`)
· `-x CCY` output currency · `-C COLS` columns (names, `all`, or
`+add,-drop`; not on `holdings global`) · `-p` hides amounts, ids and
typed names · `--period` on `returns`, `gains`, `spending`, `income`,
`cashflow`; a series on `exposure` (not `securities`, `issuers`, `funds`)
and on `yield global|sources|portfolios|accounts|curve`; of income's
forward views only `projection`, never `total` · `--level` on `spending`,
`income`, `cashflow` · `--converted` on `yield`'s aggregates: one row per
entity across currencies · `--rates forward`, `--maturing roll|idle`,
`--shift -50` on income's forward views and `cashflow projection`.

`-C` takes column names, not headers: `value`, `share`, `twr` print as
`value_USD`, `share_pct`, `twr_pct`.

Account attributes, for `grep` or `-C`: `silver_source` (`schwab`,
`ubs`, …), `account_kind`, `tax_wrapper` (`roth_ira`, `529`, `pillar_3a`,
`trust_*`), `management_style`. `category`, `type`, `class`, `group` are
labels; the taxonomy ids are `category_id` (`spend_detailed` on lines),
`type_id` (`income_type_id`, `income_detailed`) and `class_id`/`group_id`
(`financing.mortgage`).

`counterparty`, `description`, `merchant_signature` and `payer_signature`
are raw statement text that can name a private person: request them only
when needed; `merchant`, `payer` and cashflow's `name` can name one too.
Never echo any of them wholesale.

## Reading the figures

- **Returns.** TWR (default) is the strategy's return, cash flow timing
  removed; MWR (`--method mwr`, XIRR) is the return on the money as it
  went in and out. Both are after fees and taxes paid. `quality` explains
  every `n/a`: `nonpositive_base` (a loan or an unpriced opening: no
  return), `mwr_no_flows`, `nav_only…` (rough), `since_data_inception`,
  `stale_snapshot`. Account rows are exact, coarser views best-effort;
  returns never add across views. Cash accounts and deposit banks have no
  rows of their own (their money still counts in `global`); a card is in
  no return at all.
- **Exposure.** Every view sums to net worth: `share_pct` is of net worth
  (a loan is negative); `class_share_pct` is within the row's own
  dimension (its asset class in `sectors` and `sizes`; the debt and cash
  in `credit` and `maturities`; the debt in `coupons`; public equity in
  `cycles`), so those rows sum to 100. `unknown` is real money nothing
  describes; `(none)` means the dimension does not apply. In `quality`
  each flag reads `flag=NN%` of the row: `proxy` (another fund's
  composition stands in), `declared` (the household's own record; a
  declared split is marginal), `aged`, `forward`, `partial`, `unmapped`,
  `undeclared`, `unmatched`, `by_name`, `opaque`, `no_composition`.
  `--country domicile` counts a company where it is registered rather
  than where its business is. `situs` is not exposure: it looks through
  no fund (an Irish ETF of US stocks is Irish there, US in `countries`)
  and answers estate questions only.
- **Yield.** A rate belongs to a currency: an aggregate row is one entity
  in one currency, and `total` weighs them (`mixed`). `income_yield_pct`
  is the headline; `implied_yield_pct` is what prices imply;
  `realized_yield_pct` what the past twelve months paid. `duration` is
  in years; `dv01` the value lost per basis point (a mortgage's is
  negative). A stock has `earnings_yield` and `dividend_yield` instead.
  `quality` tags read `stated=61% aged<1%`: `implied`, `stated`,
  `indicated` (the last dividend at its frequency), `realized`,
  `declared`, `aged`, `stale_price`, `floating`, `default`, `thin`,
  `partial`, `units`, `unknown` (blank, not zero).
- **Spending, income, cash flow.** Own-account moves (card payments,
  transfers, mortgage payments) are never spending; `net_spend` is what a
  budget counts; `(uncategorized)` is the backlog. `income` is gross and
  defaults to `--level detailed`; `income summary -C +withheld` shows the
  tax withheld beside it, and on the forward views `withheld` is the tax
  expected at each holding's situs (blank where no rate is configured); a
  private fund's `distribution` is a `capital_return`, not income.
  `cashflow`: positive is cash arriving; `operating`, `investing`,
  `financing` (the mortgage) and `vehicles` (retirement, education and
  health plans) sum to `net_cash_flow`, and differ from `income`'s and
  `spending`'s totals. `cashflow coverage` (`status` `measured`,
  `obscured`, `opening`, `ended`, `unmeasurable`) says how far each
  account's statement can be trusted. `sankey --period`, `sankey --level
  section` and `coverage -x` are refused.
- **Gaps.** A source holds nothing before its first snapshot: an early
  zero is missing history, not a real zero. A blank value means no
  exchange rate to the `-x` currency. `wealthdb -r status` gives each
  source's last observed date; `wealthdb -r snapshots <source>` the dates
  gold has.
- An agent without a shell uses the MCP server instead (`wealthdb mcp`,
  mcp/README.md).
