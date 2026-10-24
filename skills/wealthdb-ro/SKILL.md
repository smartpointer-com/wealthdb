---
name: wealthdb-ro
description: Query the user's consolidated cross-institution investment portfolio through the read-only `wealthdb` CLI — holdings, account and portfolio balances, net worth, asset allocation, transaction history, investment returns (TWR and MWR/XIRR), look-through exposure by asset class, sector, size, country, currency, credit rating, maturity and issuer, each holding's legal situs, realized and unrealized gains with their cost basis and tax lots, yields and interest-rate sensitivity (income yield, implied and realized yield, cash against its benchmark, duration and DV01), categorised spending, categorised income and the income the holdings are expected to pay, and the household cash flow statement with its Sankey. Use whenever a question is about current holdings, what an account or portfolio is worth, allocation, money in or out, what is really owned once every fund is looked through, how something performed over a period, what was gained or lost (realized or unrealized, or a holding's cost basis), what the holdings yield or how they move with interest rates, what was spent and on what, what was received and from whom, what the holdings will pay and when, or where the household's cash came from and where it went.
---

# wealthdb — portfolio queries (read-only)

`wealthdb` is a read-only CLI over one database that merges every configured
bank, card, broker, pension and crypto source, plus the property, loans and
private holdings recorded by hand. A house, a mortgage and a venture fund are
accounts like any other. Just run the command: no setup, paths or connection
flags.

## Hard rules

1. **Run only these commands:** `holdings`, `exposure`, `yield`,
   `transactions`, `returns`, `gains`, `spending`, `income`, `cashflow`,
   `status`, `snapshots`, `help`, `version`. Every other command writes or
   serves other clients and is forbidden. Never write; just query. Put `-r`
   before the command on every call (`wealthdb -r holdings global`):
   without it a report may open the database read-write. A `-r` after
   `transactions` or `gains realized` is a different flag. It lists the
   newest first (`--reverse`) and is not read-only. The read-only `-r`
   always sits between `wealthdb` and the command.
2. **One number for a whole window: add `--period total`.** The default is
   one row per month (per quarter for `returns`). Never add rows up yourself.
   Income's forward views are the exception: they refuse it, and
   `projection`'s `total` row already sums the window.
3. **There are no row-filter flags** (`--source`, `--account`, `--category`,
   `--symbol` do not exist). Run the view, then `| grep -i text` the table
   (one row per line) for a name, wrapper, category or source.
4. **Read tables; parse JSON only to sort.** `-f json | jq`: the output is one
   JSON **array** (start with `.[] |` or `map(...)`). Keys are the table
   headers, currency suffix included (`value_USD`, `net_spend_USD`,
   `twr_pct`). The suffix is the output currency (the config's default, or
   `-x`); the recipes pass `-x USD` so the key is known. Every value is a
   **string**: `tonumber` before comparing. An empty column is **omitted**,
   so `map(select(.value_USD))` before sorting.
   A return that cannot be computed is `"n/a"`, reason in `quality`.
5. **Signs.** In line views (`transactions`, `spending transactions`,
   `cashflow transactions`) money leaving is negative. In `summary`,
   `categories` and `types`, `spend`, `refunds` and `income` are positive.
6. **`wealthdb -r help <command>`** prints everything this file leaves out.

## Recipes

Windows: `2025` a year, `2026-03` a month, `2025-01-01 2025-06-30` a range.

- **Net worth or total cash, now / at a date / in CHF** — `wealthdb -r holdings global`
  (one row: cash, positions, total), then `-d 2025-12-31` or `-x CHF`
- **Each account, with its tax wrapper** — `wealthdb -r holdings accounts`
- **One institution's total** — `wealthdb -r holdings sources | grep -i schwab`
- **One wrapper, account, property or loan** —
  `wealthdb -r holdings accounts | grep -i roth` (or `401`, `mortgage`)
- **Largest holdings** —
  `wealthdb -r holdings positions -x USD -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber) | reverse | .[0:5]'`
  (a house or a loan is a position too)
- **Allocation once funds are looked through** — `wealthdb -r exposure classes`
- **Sectors, company sizes, countries, currencies** — `wealthdb -r exposure sectors`,
  `sizes`, `countries`, `currencies` (`-d 2025-12-31` for a past day)
- **One company, directly and through funds** —
  `wealthdb -r exposure securities | grep -i <company>` (`value` is the total)
- **The largest issuers, stock, bonds and deposits together** —
  `wealthdb -r exposure issuers | head -12` (`cumulative_share_pct` answers
  "how much sits with the top ten")
- **The debt by credit, maturity and coupon kind** — `wealthdb -r exposure
  credit` (`grade` IG or HY), `maturities`, `coupons`; the stocks by
  cycle: `wealthdb -r exposure cycles` (`class_share_pct` is the share
  within what the view describes, `share_pct` the share of net worth)
- **Exposure over time** — `wealthdb -r exposure countries --period quarterly 2025`
- **How far to trust exposure** — `wealthdb -r exposure coverage`; how old each
  fund's data is, and which fund's holdings stand in for it (`proxy`, a key):
  `wealthdb -r exposure funds`
- **What the household states about a private holding** — a fund:
  `wealthdb -r exposure funds` (`via` reads `declared`, with the record's
  `declaration_date` and the dimensions it `declared`); a property, a
  loan or an SPV: `wealthdb -r exposure securities | grep -i <name>` (quality
  `declared`); the private holdings with no record:
  `wealthdb -r exposure coverage | grep undeclared`
- **Where holdings legally sit** (for an estate question) —
  `wealthdb -r exposure situs`; one fund's situs:
  `wealthdb -r exposure funds -C fund,name,domicile,situs,situs_basis`; what
  each situs rests on, and the holdings whose situs is unknown:
  `wealthdb -r exposure coverage -f csv | grep '^situs,'`
- **Records still to write** — `wealthdb -r exposure coverage -f ndjson`: a
  skeleton per undeclared holding, and a situs-only one per holding of
  unknown situs, for the user to fill into the declarations ledger
- **What the holdings yield, by currency** — `wealthdb -r yield global`
  (`income_yield_pct`: the next twelve months' expected income over value;
  the `total` row weighs the currencies); per account:
  `wealthdb -r yield accounts`
- **Cash that earns too little** — `wealthdb -r yield cash` (worst-paid
  first; `gap_pct` is the rate less the currency's benchmark; money-market
  funds and term deposits sit in the same table)
- **Is the extra yield paid for in rate risk** — `wealthdb -r yield funds`
  (`implied_yield` beside `duration` and `dv01`, per fund); one bond or company:
  `wealthdb -r yield securities -C +currency,duration | grep -i <name>`
- **Dividend safety and earnings** —
  `wealthdb -r yield securities -C +payout,dividend_growth,earnings_growth,coverage | grep -i <company>`
  (`coverage`: operating income over interest expense, in times)
- **Credit quality of a bond** —
  `wealthdb -r yield securities -C +rating | grep -i <issuer>` (`rating`
  is a bucket, AAA to B, or `unrated`; blank where nothing rates it;
  `quality` says what the figures rest on)
- **Yield and duration over time** —
  `wealthdb -r yield global --period quarterly 2026`
- **What a rate rise does** — `wealthdb -r yield curve` (`dv01` per
  currency and tenor: the value lost for a rise of 0.01%, mortgages
  netted); to the income: `wealthdb -r yield global -C +income_100bp`
  (the next twelve months' income change for a rise of 1%)
- **How far to trust the yields** — `wealthdb -r yield coverage`; the
  ledger records still to write: `wealthdb -r yield coverage -f ndjson`
- **Return of the portfolio / each account** —
  `wealthdb -r returns global 2025 --period total` / `wealthdb -r returns accounts 2025 --period total`
  (the account column is `entity`; one account: `| grep -i 'brokerage'`)
- **Best account by return** —
  `wealthdb -r returns accounts 2025 --period total -f json | jq 'map(select(.twr_pct != "n/a")) | sort_by(.twr_pct|tonumber) | reverse | .[0:3]'`
- **Realized gains of a year** — `wealthdb -r gains summary 2025 --period total` (`realized`)
- **Total unrealized gain now** — `wealthdb -r gains summary --period total`
  (`unrealized_end`); never sum positions
- **Lots sold in a year** — `wealthdb -r gains realized 2025`
- **Holdings that gained most** —
  `wealthdb -r gains positions 2025 -x USD -f json | jq 'map(select(.gain_USD and .symbol)) | sort_by(.gain_USD|tonumber) | reverse | .[0:5]'`
  (one row per account and holding: a symbol in two accounts has two rows)
- **A holding's cost basis and unrealized gain** —
  `wealthdb -r holdings positions -C +cost_basis,unrealized_gain,basis_stamp | grep -i btc`
  (`basis_stamp`: `stated/…` the institution's, `derived/…` computed from its
  figures, `rebuilt/<method>/…` rebuilt from the trades); lots:
  `wealthdb -r gains lots | grep -i btc`
- **Where gains are missing or rebuilt** — `wealthdb -r gains coverage 2025`
  (`verdict` `no_basis`, `no_realized` or `partial`: missing, not zero);
  against the statements: `wealthdb -r gains check 2025`. `--missing-basis
  zero` on `holdings positions` or any `gains` view but `coverage` and
  `check` counts an unknown cost as zero.
- **Spent in a window / by category** — `wealthdb -r spending summary 2025 --period total` /
  `wealthdb -r spending categories 2025 --period total` (add `--level detailed`
  for groceries, restaurants, flights …, then `| grep -i grocer`)
- **Biggest purchases in a month** —
  `wealthdb -r spending transactions 2026-03 -x USD -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber) | .[0:5]'`
- **Income / by type** — `wealthdb -r income summary 2025 --period total` /
  `wealthdb -r income types 2025 --period total | grep -i dividend`
- **This year's income, received and still expected** —
  `wealthdb -r income projection 2026` (one row per month, then `total`
  and `monthly_average`; `status` reads `received`, `expected` or
  `mixed`; `basis` says what the expected part rests on)
- **This year's income beside last year's** —
  `wealthdb -r income projection 2025` beside
  `wealthdb -r income projection 2026` (the same types; 2025 is all
  received)
- **What the holdings will pay next** — `wealthdb -r income projection`
  (the months after this one, twelve unless the config says otherwise)
- **Per holding** — `wealthdb -r income positions 2026` (every holding,
  the ones that pay nothing too; `next_12m` is the next twelve months)
- **Expected payments and maturities by date** —
  `wealthdb -r income calendar` (kind `principal` is a maturity, outside
  the income totals)
- **Per account** — `wealthdb -r income accounts`
- **What the expected income assumes** — on any forward view,
  `--rates forward` (floating rates along the forward curve; default
  today's), `--maturing roll` (a maturing holding bought again; default
  `cash`, `idle` for nothing), `--shift -50` (floating rates 50 basis
  points lower)
- **Net cash flow / by class** — `wealthdb -r cashflow summary 2025 --period total` /
  `wealthdb -r cashflow flows 2025 --period total --level class`
- **What will next year's cash flow look like** —
  `wealthdb -r cashflow projection --period quarterly` (the projection's
  income against the trailing year's outflows, mortgages to their
  maturity; `vehicles_income` is the projected income on accounts
  outside the pool, out of `net`; `quality` says which part rests on
  what)
- **Mortgage, retirement or education payments** —
  `wealthdb -r cashflow flows 2025 --period total --level class | grep -i -E 'mortgage|retirement|education'`
- **Largest transactions** —
  `wealthdb -r transactions 2025 -x USD -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber|fabs) | reverse | .[0:10]'`
- **Is the data current / which dates exist** — `wealthdb -r status` / `wealthdb -r snapshots <source>`

## Pick the command

| The question | Command | Views |
|---|---|---|
| Net worth, balances, holdings (as of a day) | `holdings` | `global`, `sources`, `portfolios`, `accounts`, `positions` |
| What is **really owned** once funds are looked through (as of a day); where holdings legally sit | `exposure` | `classes`, `sectors`, `sizes`, `countries`, `currencies`, `credit`, `maturities`, `coupons`, `cycles`, `situs`, `securities`, `issuers`, `funds`, `coverage` |
| What the holdings **yield**; interest rates, duration, rate risk; cash against its benchmark (as of a day) | `yield` | `global`, `sources`, `portfolios`, `accounts`, `positions`, `securities`, `funds`, `cash`, `curve`, `coverage` |
| Trades, dividends, fees, cash in and out | `transactions` | — |
| How something **performed** (return %) | `returns` | `global`, `sources`, `portfolios`, `accounts` |
| **Gained or lost**; cost basis; tax lots | `gains` | `summary`, `sources`, `portfolios`, `accounts`, `positions`, `realized`, `lots`, `coverage`, `check` |
| **Spent**, on what | `spending` | `summary`, `categories`, `transactions` |
| **Received**, from whom; what the holdings are **expected to pay** | `income` | `summary`, `types`, `transactions`; forward: `projection`, `calendar`, `sources`, `portfolios`, `accounts`, `positions` |
| Where the household's cash came from and went; mortgage, retirement, education payments; the plan ahead | `cashflow` | `summary`, `flows`, `sankey`, `transactions`, `coverage`, `projection` |

Holdings totals reconcile: `global` ≈ sum of `sources`, `portfolios`,
`accounts`, and `positions --with-cash`.

## Dates

`holdings`, `exposure` and `yield`: `-d YYYY-MM-DD` is the day (default
today); each account gives its own latest snapshot on or before it, until
that snapshot ends (`wealthdb -r help holdings`). `exposure`
and `yield` take `--period monthly|quarterly|annual` with a window instead,
as below.

Every other command takes the window as positional arguments, never `-d`:
`2026` (a year), `2026-06` (a month), `2026-06-15` (a day),
`2026-01-01 2026-06-30` (a range), `2026-01-01 -` (to today),
`- 2026-06-30` (from the start), `- today` (all time). Defaults:
`transactions` the past 30 days; `returns` since the first snapshot; the
rest the trailing twelve months. Resolve "last year" and "this year" from
today's date and pass them (`2025`, `2026-01-01 today`).

`income`'s forward views (`projection`, `calendar`, `sources`,
`portfolios`, `accounts`, `positions`) take a window that may lie in the
future, and a lone FROM is its own span: `2027` is all of 2027. Their
default is the months after this one, not the trailing twelve. They
refuse an open bound, so the `2026-01-01 -`, `- 2026-06-30` and
`- today` idioms are not for them: give FROM, FROM TO, or neither.

`cashflow projection` takes the same forward window, in whole months
from next month on. Its default is the projection's horizon. It refuses
`--level`, `--investing` and `--period total`.

## Flags and columns

`-f table|json|csv` (`ndjson` on `exposure coverage` and `yield coverage`)
· `-x CCY` output currency · `-C COLS` columns (names, `all`, or
`+add,-drop`) · `-p` redact · `--period` on `returns`, `spending`,
`income`, `cashflow`, `gains`; on `exposure` a series only
(`monthly|quarterly|annual`, not on `securities`, `issuers` or `funds`),
and on `yield` for `global`, `sources`, `portfolios`, `accounts` and
`curve` · `--level` on `spending`, `income`, `cashflow` · `--converted`
on `yield`'s `global`, `sources`, `portfolios` and `accounts` ·
`--rates`, `--maturing`, `--shift` on `income`'s forward views and on
`cashflow projection`. The forward views refuse `--level`. Of them only `projection` takes
`--period`, as `monthly|quarterly|annual` (not `total`: its `total` row
sums the window).

`-C` takes column **names**, not headers: ask for `value`, `share`,
`net_flow`, `twr`; they print as `value_USD`, `share_pct`, `net_flow_CHF`,
`twr_pct`. `wealthdb -r help <command>` lists each view's column names and
defaults (for `holdings`, `wealthdb -r help <view>`, e.g. `help positions`);
`-C all` prints every column.

`-p` prints amounts as `*****.**` (and drops them from JSON) and narrative
text as `***`. A name a person typed (a nickname, a manual holding or
account) prints as a pseudonym such as `manual-1`, numbered afresh each
run. Categories, classes and shares stay legible.

Account attributes, for `grep` or `-C`: `silver_source` (the institution:
`schwab`, `ubs`, …), `account_kind`, `tax_wrapper` (`roth_ira`, `529`,
`pillar_3a`, `trust_*`, …), `management_style`.

`category`, `type`, `income_type`, `class` and `group` are display labels.
The taxonomy values (`cash_withdrawal`) are in `category_id`
(`spend_detailed` on lines), `type_id` (`income_type_id` on lines) and
`class_id` / `group_id` (dotted keys like `financing.mortgage`). Filter on
the value, quote the label.

`counterparty`, `description`, `merchant_signature` and `payer_signature`
are raw statement text that can name a private person: request them only
when needed. `merchant`, `payer` and cashflow's `name` are shown by default
and can name one too. Never echo any of them wholesale.

## Returns

TWR (default) is the strategy's return, cash flow timing removed: "how did
the investments do?". MWR (`--method mwr`, XIRR) is the return on the money
as it went in and out: "what did I make?". `--period` defaults to
quarterly plus a since-inception row. Returns are after fees and taxes
paid.

**Read `quality`:** an `n/a` always has its reason there.
`nonpositive_base` (an opening value zero, negative or unpriced, e.g. a
loan: no return), `mwr_no_flows` (no flows: no MWR), `nav_only…`
(value-only source: rough), `since_data_inception` (since the first
snapshot, not the account's opening), `stale_snapshot` (old valuation).
Account returns are exact; portfolios, sources and global are
best-effort. Returns never add across views: query the grain you want.
Cash accounts and deposit banks have no return rows of their own; their
money still counts in `global`. A card is in no return at all.

## Exposure

`exposure` looks through every fund to the stocks, bonds and cash it holds
and adds what is held directly. Every view sums to net worth: `share_pct` is
a share of net worth (a loan is negative). `class_share_pct` is a share of
what the row's dimension describes: of the row's asset class in `sectors`
and `sizes`; of every row but `(none)` in `credit` and `maturities` (the
debt and the cash), `coupons` (the debt) and `cycles` (the public
equity), so those rows sum to 100.

- **Read `quality` first.** `proxy` (another fund's composition stands in),
  `declared` (the household's own statement: a target split from a fund's
  documents, or a property's country), `aged` (a composition over 35
  days older than the day), `forward` (one dated after it), `partial`,
  `unmapped`, `undeclared`, `unmatched` (part of the value is not fully
  classified), `by_name` (a sector or country found by matching a name),
  `opaque` (a private fund nothing looks through), `no_composition` (in
  `funds`: nothing describes the fund on the day), `unknown` (in
  `credit`: debt nothing rates, on the `unrated` row).
- In an aggregate row each flag reads `flag=NN%`: the share of the row's
  gross value that carries the flag (`proxy=12%`, `aged=<1%`).
- A declared split is marginal: a fund declared 40% Europe and 80% private
  equity shows both, but how its regions and classes combine is assumed,
  not known.
- `securities` shows a declared holding as one row, with its largest
  part's class, sector, country and currency. The split itself is in
  `classes`, `sectors`, `countries` and `currencies`; `funds` says which
  dimensions a declared fund states.
- In `countries`, a part declared by region alone reads country
  `unknown` under its region, and a declared rest of the world reads
  region `other`. Both are declared (quality `declared`), not gaps.
- `unknown` is real money nothing describes, not zero. `(none)` means the
  dimension does not apply: a metal's or a coin's country, the size of
  anything but public equity, anything but debt or cash in `credit` and
  `maturities`, anything but debt in `coupons`, anything but public
  equity in `cycles`, and in `issuers` the leaves no issuer fits.
- Sizes are `large` (from 10bn USD market cap), `mid` (from 2bn), `small`;
  `unknown` where nothing sizes a stock.
- `--country risk|domicile` (on `countries`, `securities`): where the
  business is (default) or where the company is registered.
- **`situs` is not exposure.** It gives the one country whose law each
  holding is an interest under, and looks through no fund: an Irish ETF
  of US stocks is Irish there, and US in `countries`. Use it for an
  estate question, never for allocation. Its `quality` gives what each
  situs rests on (`isin=80%`, `declared`, `custody`, ...), and `funds_pct`
  the share of the row held in funds. `unknown` is a coin, cash at a
  source whose `exposure.custody` names no country, or any holding
  nothing else places; a declaration can place it. A declared situs
  changes no other view.

## Yield

`yield` gives what each holding earns, and how its value moves with
interest rates, as of a day.

- **A rate belongs to a currency.** An aggregate row is one entity in one
  currency; the `total` row weighs the currencies by value (`mixed`).
  `--converted` gives one row per entity instead.
- `income_yield_pct` is the headline: the next twelve months' expected
  income at today's rates, over value. `implied_yield_pct` is what prices
  imply; `realized_yield_pct` what the past twelve months paid over the
  mean daily value. A position shows its security's, pooled over every
  account and source, over its mean value on the days any of them held
  it. A position held part of the year reads what that part paid, below
  its yearly rate, and carries `partial`; an entity row reads a yearly
  rate. A holding whose amount rounds to no cent has no row.
- `duration` is in years; `dv01` is the value lost for a rise of one basis
  point. A mortgage nets: its `dv01` is negative, and the rates leave it out.
- A stock has no implied yield or duration: read `earnings_yield` and
  `dividend_yield`.
- **Read `quality` first.** `implied` (from a price; not shown when it
  covers the whole row), `stated` (a source states the rate), `indicated`
  (a dividend's last payment at its frequency; interim plus final for a
  payer twice a year), `realized`, `declared` (the ledger), `aged` and
  `stale_price` (a fund's lines are old), `floating`, `default`, `thin`
  (near zero most of the year: no realized yield), `partial` (a figure
  covers part of the year: a position's realized yield covers fewer days
  than the year's, or a payer twice a year shows one half's dividend),
  `units` (a dividend per share whose units a split or a ratio change may
  have left unclear), `unknown` (nothing states it: blank, not zero),
  `mixed`.
- In an aggregate row the tags are separated by spaces, each as
  `tag=NN%` or `tag<1%` (`stated=61% aged<1%`), unlike exposure's
  comma-separated flags. A bare tag covers the whole row.

## Spending, income, cash flow

- `spend` and `refunds` are positive; `net_spend` is what a budget counts.
  Own-account moves (card payments, transfers, mortgage payments) are never
  spending. `--level detailed` for a named category (groceries).
  `(uncategorized)` is the backlog: say so when it is large.
- `income` defaults to `--level detailed`. Tax withheld is on the spending
  side; `income summary -C +withheld` shows it beside the income. A
  private fund's `distribution` is a `capital_return`, not income.
- `cashflow`: positive is cash arriving. `operating`, `investing`,
  `financing` and `vehicles` sum to `net_cash_flow`; `operating_in` and
  `operating_out` are positive magnitudes. Retirement, education and health
  plans are `vehicles`: one of them is a row of `flows --level class`;
  `financing` holds the mortgage. These totals are not `income` or
  `spending`'s. `sankey --period`, `sankey --level section` and
  `coverage -x` are refused. `coverage` says per account how far the
  statement can be trusted (`status`: `measured`, `obscured`, `opening`,
  `ended`, `unmeasurable`).

## Diagnostics and gotchas

- `wealthdb -r status` — per source, the date its data was last observed and
  whether newer data waits to load. `wealthdb -r snapshots <source>` — the
  dates gold has, to check a date exists before calling a balance zero.
- A source holds nothing before its first snapshot: an early zero is
  missing history, not a real zero. A blank value means no exchange rate
  to your `-x` currency.
- An agent without a shell uses the MCP server instead (`wealthdb mcp`,
  mcp/README.md).
