# Yield

What the holdings earn, and how their value moves with interest rates,
as of a day. The yield report gives each holding its yields, its
duration and its rate sensitivity, and sums them by currency at every
grain `holdings` has. This file defines every figure the report shows
and the rules behind them.

Related design: DESIGN.md §4.18 (the `yield` command), §7.6 (the
reference, config and derived tables), §10.15 (the code);
[EXPOSURE.md §9](EXPOSURE.md#yield-records) (the ledger's yield
records); [INCOME.md §12](INCOME.md) (the income projection); migrations
0134 to 0136.

Every figure belongs to one holding first. A grain sums or weighs the
figures of its holdings, so the grains reconcile by construction. A
holding is valued exactly as `holdings global` values it.

---

## 1. Words

- **A rate** is in percent a year: `4.25` is 4.25%. It is in its
  currency's terms.
- **Stated** is a rate a source states: a coupon, a deposit rate, a
  fund's yield. **Implied** is the yield the engine computes from a
  price, a coupon and a maturity.
- **The benchmark** is a currency's risk-free rate (§4).
- **Duration** is the modified duration in years. **DV01** is the value
  lost, in the output currency, for a rise in rates of one basis point.
- **Gross value** is the sum of the holdings' values with a liability
  counted whole. Quality and coverage shares are shares of it.

## 2. The views

`wealthdb yield <view> [-d DATE] [--converted] [-f FORMAT] [-C COLS] [-x CCY] [-p]`

`wealthdb yield <view> --period monthly|quarterly|annual [FROM [TO]]`

| View | One row per | Answers |
| --- | --- | --- |
| `global` | currency, and a total | what the holdings yield, how long they last, how they move with rates |
| `sources` | source and currency | the same per source |
| `portfolios` | portfolio and currency | the same per portfolio |
| `accounts` | account and currency | the same per account; a mortgage is a negative row |
| `positions` | holding | one holding's figures and its own terms, with the keys of `holdings positions` |
| `securities` | security, held directly and through funds | a bond's terms and implied yield; a stock's earnings yield, P/E, dividend yield, payout and growth; the keys of `exposure securities` |
| `funds` | fund held directly | what the fund's lines, page or filing say, as of which day |
| `cash` | cash balance, term deposit, money-market fund | the rate and its tiers, the realized yield, the benchmark and the gap |
| `curve` | currency and tenor | DV01 along the curve, by what holds it (§6) |
| `coverage` | reason | what the figures rest on, and what is missing (§10) |

The first four are the aggregate views. They share one set of columns.
Their amounts reconcile: the value, DV01 and income of `global` equal
the sum of each of the other three. `cash` lists the worst-paid first:
the rows without a gap, then the largest shortfall below the
benchmark. The debit balances follow the credit ones. `funds` lists the
largest fund first.

- `-d` is the day, today by default.
- `--converted` gives the aggregate views one row per entity (§5).
- `--period` runs an aggregate view or `curve` at the last second of
  every period of a window, a `period` column first. The window
  defaults to the twelve months to today and ends no later than today.
  It refuses `-d`.
- `-f ndjson` is `coverage`'s alone: the ledger records still to write
  (§10). It takes no `-C`, `-p` or `--period`.
- `-p` stars the amounts: value, DV01, the income amounts and
  `tier_up_to`. It stars the price in `positions` and `securities`
  alike, and every account id.
- Under `-p` a `security` or `fund` key that is a holding's own
  (`holding:<source>:<id>`, no public key) is hidden whole, as
  `exposure securities` hides it. `coverage`'s `instrument` is hidden
  whole too. `position_key` stays legible, as `holdings positions`
  keeps it. Rates, durations, shares, labels and public security keys
  stay.

### Columns

An amount prints in the output currency: `value_CHF`, `dv01_CHF`,
`income_12m_CHF`. A rate or a share prints with a `_pct` suffix:
`income_yield_pct`, `share_pct`. `-C` takes the name without the
suffix. Years, prices, `pe` and `coverage` print as they are.

| Column | Views | Meaning |
| --- | --- | --- |
| `value` | all but `curve` | the holdings' value; a liability's is negative |
| `income_yield` | aggregates, `positions` | `income_12m` over value: the headline for an income reader (§4) |
| `stated`, `current_yield` | aggregates, `positions` | the coupon, dividend or deposit rate as stated; the stated rate over the price |
| `implied_yield` | aggregates, `positions`, `securities`, `funds` | the yield the price implies, to worst where a call is known, in its market's own compounding |
| `implied_yield_annual` | aggregates, `positions` | the same yield as an effective annual rate, so markets compare |
| `realized_yield` | aggregates, `positions`, `cash` | the income of the twelve months to the day over the average value (§4) |
| `earnings_yield`, `dividend_yield` | aggregates, `positions`, `securities` | trailing earnings over price; the indicated dividend over price |
| `benchmark`, `gap` | aggregates, `positions`, `cash` | the currency's benchmark; the implied yield, else the stated rate, less it (§4) |
| `duration` | aggregates, `positions`, `securities` | modified duration in years |
| `convexity` | aggregates, `positions` | the curvature of the price in the yield, in years squared |
| `dv01` | aggregates, `positions`, `funds`, `curve` | the value lost for a rise of one basis point |
| `income_12m`, `income_100bp` | aggregates, `positions`, `cash` | the projected income of the twelve months after this one, and its change for rates one percent higher (§4) |
| `maturity` | aggregates, `positions` | the years to maturity: a fund's weighted average |
| `quality` | all but `curve` and `coverage` | what the figures rest on (§7) |
| `kind` | `positions`, `funds`, `cash` | the holding's kind (§3) |
| `coupon`, `maturity_date`, `coupon_kind`, `rating` | `positions`, `securities` | the terms: a coupon kind is `fixed`, `floating`, `inflation_linked` or `zero`; a rating is a credit bucket |
| `price` | `positions`, `securities` | clean, per 100 of face, for every bond, in default or not; blank where the quantity is no face value (§3); per unit otherwise |
| `spread` | `positions` | a floating holding's rate over its benchmark, which the projection keeps along the rate path; blank for any other |
| `pe`, `payout`, `dividend_growth`, `earnings_growth`, `coverage` | `securities` | a stock's dividend safety (§3) |
| `direct_value`, `via_funds_value`, `funds`, `share` | `securities` | as `exposure securities` gives them |
| `lines_yield`, `lines_duration`, `lines_maturity`, `lines_as_of`, `lines_covered` | `funds` | what the fund's lines imply, the day of its composition, and the share of the fund they cover |
| `floating`, `in_default`, `paid_in_kind` | `funds` | the shares of the fund in floating lines, in lines in default or arrears, and in lines paid in kind |
| `duration`, `maturity` | `funds` | the figures the report reads, as its `implied_yield` is (§3): a ledger record's duration, else the page's or filing's figure, else the lines' |
| `yield_to_maturity`, `distribution_yield`, `sec_yield`, `seven_day_yield`, `trailing_yield` | `funds` | what the fund's page or filing states; the yield to worst where one is stated |
| `as_of`, `source`, `risk_as_of` | `funds` | the day the figures describe; where they come from (`lines`, `page`, `filing`, `ledger`); the day of the filed DV01 |
| `rate`, `tiers`, `tier_up_to`, `valid_from`, `term_end` | `cash` | the rate the balance earns, tiers blended; the number of tiers; the first tier's limit; the day the rate holds from; a term deposit's end. An overdraft lists its debit tiers |
| `dv100`, `funds`, `bonds`, `cash`, `liabilities` | `curve` | DV01 times 100, and DV01 by what holds it (§6) |
| `section`, `reason`, `holdings`, `instrument` | `coverage` | §10 |

The keys are the ones `holdings` and `exposure` name: `silver_source`,
`portfolio`, `portfolio_id`, `account`, `account_id`, `account_kind`,
`tax_wrapper`, `account_nickname`, `position_key`, `symbol`, `name`,
`asset_class`, `vehicle`, `currency`, `security`, `ticker`, `sector` and
`fund`. `-C all` prints every column. The defaults:

| View | Default columns |
| --- | --- |
| aggregates | the grain's keys (`silver_source`, `portfolio`, or `account` and `tax_wrapper`), `currency`, `value`, `income_yield`, `implied_yield`, `realized_yield`, `duration`, `dv01`, `quality` |
| `positions` | `silver_source`, `account`, `symbol`, `kind`, `currency`, then the aggregates' |
| `securities` | `security`, `name`, `asset_class`, `value`, `share`, `coupon`, `maturity_date`, `implied_yield`, `earnings_yield`, `dividend_yield`, `quality` |
| `funds` | `fund`, `name`, `kind`, `currency`, `value`, `implied_yield`, `duration`, `distribution_yield`, `sec_yield`, `as_of`, `source`, `quality` |
| `cash` | `silver_source`, `account`, `kind`, `currency`, `value`, `rate`, `valid_from`, `term_end`, `realized_yield`, `benchmark`, `gap`, `quality` |
| `curve`, `coverage` | every column; `coverage` leaves out `account` |

An invented household, in Swiss francs:

```
$ wealthdb yield global -x CHF
 currency |  value_CHF | income_yield_pct | implied_yield_pct | realized_yield_pct | duration | dv01_CHF | quality
----------+------------+------------------+-------------------+--------------------+----------+----------+----------------------------------------------------------------
 USD      | 1250000.00 |             3.42 |              4.08 |               3.11 |     4.20 |   525.00 | implied=70% stated=30%
 CHF      |  800000.00 |             1.12 |              1.02 |               0.91 |     2.50 |   200.00 | implied=35% stated=61% unknown=4%
 EUR      |  300000.00 |             2.61 |              2.93 |               2.40 |     5.00 |   150.00 | aged=20% stale_price=20%
 total    | 2350000.00 |             2.53 |              2.89 |               2.27 |     3.72 |   875.00 | mixed implied=62% stated=37% aged=3% stale_price=3% unknown=1%
(4 rows)

$ wealthdb yield cash -x CHF
 silver_source | account   | kind         | currency | value_CHF | rate_pct | valid_from | term_end   | realized_yield_pct | benchmark_pct | gap_pct | quality
---------------+-----------+--------------+----------+-----------+----------+------------+------------+--------------------+---------------+---------+----------
 bank-2        | Current   | deposit      | CHF      |  32000.00 |          |            |            |               0.00 |          0.45 |         | unknown
 broker-1      | Brokerage | deposit      | USD      |  40000.00 |     0.50 |            |            |               0.48 |          4.00 |   -3.50 | stated
 broker-1      | Brokerage | money_market | USD      |  90000.00 |     3.95 |            |            |               3.90 |          4.00 |   -0.05 | stated
 bank-1        | Term      | term_deposit | CHF      | 100000.00 |     0.85 | 2026-04-01 | 2027-03-31 |               0.80 |          0.45 |    0.40 | declared
(4 rows)
```

## 3. What each kind reads

A fund is a holding of vehicle `etf` or `fund`. A figure no source
gives stays blank, and the holding carries `unknown`.

| Kind | What it is | Its yield | Its duration |
| --- | --- | --- | --- |
| `deposit` | a cash balance or a demand deposit | its account's rate (below) | zero |
| `term_deposit` | a cash balance whose rate states a term end after the day | its rate to its end | the years to its end, over one plus the interest it earns to then |
| `money_market` | a fund of asset class cash, or one with a seven-day yield | a ledger record's yield, else the seven-day yield its filing states | a ledger record's duration, else its weighted average maturity |
| `bond` | a bond with a fixed, zero or inflation-linked coupon | the yield its price implies (below) | its own, at that yield |
| `bill` | a bond whose terms state no coupon payments (frequency 0) | its discount yield on ACT/360 | its one payment's |
| `floating_note` | a bond whose coupon kind is floating | none implied; it carries `floating` | to its next reset |
| `note` | a loan held, or a convertible note | its coupon | none |
| `bond_fund` | a fund of asset class fixed income or multi asset | a ledger record's yield, else what its lines imply, else what its page states (below) | a ledger record's, else its page's, else its lines' |
| `equity_fund` | a fund of asset class public equity or real estate | its earnings and dividend yields (below) | none |
| `stock` | a stock of asset class public equity | its earnings and dividend yields (below) | none |
| `liability` | a mortgage, or a loan owed | its rate, from its terms | to the end of its fixed period (below) |
| `other` | anything else: a property, a private fund, metal, a coin | its realized yield; its expected income repeats what it was paid last year (`realized`); paid nothing, it reads `unknown` | none |

Every kind has a `realized_yield` and a `benchmark` (§4). A stock and an
equity fund have no `gap`. A liability's value and DV01 are negative,
so it nets against the assets' rate risk. It has no `income_12m`, and
it earns nothing in the projection.

**Liabilities.** A liability's rate risk runs to the end of its fixed
period (`fixed_until`). Terms that state no fixed period fix the coupon
to maturity, unless it floats.

**Deposits.** A balance at or above zero earns its account's credit
rate in its currency. One below zero pays the debit rate. A tiered rate
is blended over the balance: each tier's rate on the part of the
balance up to its limit. The rates are `account_rates` (DESIGN.md
§7.6). A rate a snapshot states holds as the newest snapshot on or
before the day states it. A rate a source states once holds from its
`valid_from` on. A ledger `rate` record holds from its day on and reads
`declared`. It replaces whatever a source states for its account and
currency.

A balance whose rate no source states repeats the interest its account
received in the twelve months to the day, a year later. It reads
`realized`, not `unknown`. With no interest received it reads
`unknown`. `coverage` lists it under `no rate` either way (§10).

**Bonds.** Each field of a bond's terms comes from the first of these
that states it: the ledger's `terms` record in force on the day, the
source's own record, the broker's description, OpenFIGI's ticker. The
frequency and the day count fall back to the market's convention (§8).
A coupon with no stated kind is fixed, or a zero where it is zero. The
rating is the ledger's `rating` record's, else the lower of the
agencies' grades the description states. A bond graded in default
carries `default`, and has no implied yield and no duration. Its
projection reads `unknown`: no coupon and no principal (INCOME.md §12).

Every bond's price is clean, per 100 of face, in default or not. It is
the holding's amount less the interest accrued, where the source states
it, over the face. Where no accrual is stated, the amount reads as clean
and the engine computes the accrual. A price outside 1 to 1000 says the
quantity is no face value. The price is then blank, and the bond reads
`unknown`.

- `implied_yield` is the yield its price implies, the accrual added, by
  the street convention, compounded at its frequency. A bond above par
  with a known call that is not make-whole yields to the call, where
  that yield is the lower.
- `implied_yield_annual` is that yield as an effective annual rate. A
  bill's is the rate its price earns to maturity, compounded yearly.
- `current_yield` is the coupon over the price. A floating note's next
  reset is the one its terms state, else its next coupon date. Terms
  that state no frequency make it reset quarterly.

**Funds.** A fund's figures are the newest on or before the day,
within the exposure's `max_gap_days` (EXPOSURE.md §4). A ledger `terms`
record's `yield` and `duration` win over the page, the filing and the
lines ([EXPOSURE.md §9](EXPOSURE.md#yield-records)). They read
`declared`, as of the record's day. `source` reads `ledger` where the
yield is the record's.

A money-market fund yields the record's yield, else the seven-day yield
its filing states. It lasts the record's duration, else its weighted
average maturity.

A bond fund reads these:

- `implied_yield` is the record's yield, else what its lines imply,
  where lines with a yield cover half the fund or more. A yield from
  the lines carries `implied` and `stale_price`: the lines are priced on
  their composition's day. It carries `aged` too where the composition
  reads aged in exposure (EXPOSURE.md §8).
- Else it is the yield to worst its page or filing states, else the
  yield to maturity (`stated`).
- `stated` and `current_yield` are its distribution yield, else its SEC
  yield. `dividend_yield` is its trailing yield.
- Its DV01 is the fund's filed DV01 per currency and tenor, where its
  filing states one and no record states a duration. It is scaled to
  the holding: the filed DV01 over the fund's net assets, times the
  holding's value. Else it is its value times its duration.
- It carries `floating` where floating lines are half the fund or more,
  and `default` where any line is in default or arrears.

A line states its own yield and duration, or its price implies them
from its coupon, maturity and face, on ACT/ACT. A line's yield to worst
wins over its yield to maturity. A security held only through funds
reads the line of the most value held: its weight times the value of
the fund holding it.

- A floating line counts in no yield of the fund's lines.
- A line in default or arrears, or paid in kind, has no yield, stated
  or implied. It counts in no yield of the fund's lines, and it shows
  none in `yield securities`. There a line in default reads `default`.

**Stocks and equity funds.** A stock's `earnings_yield` is its trailing
twelve months' diluted earnings per share over its price (§4). An
equity fund's is the inverse of the P/E its page states.
`dividend_yield` is the indicated dividend over the price (§4). An
equity fund without one reads the trailing yield, else the distribution
yield, its page states. Earnings or dividends a filing reports in
another currency than the price give no yield.

`securities` adds a stock's dividend safety, from its trailing figures:

- `pe`: the price over the earnings;
- `payout`: the dividends per share over the earnings;
- `dividend_growth` and `earnings_growth`: each against a year earlier;
- `coverage`: the operating income over the interest expense.

## 4. Benchmark, income and dividends

**The benchmark.** A currency's curve is the series the config names
per tenor (§8). Each tenor takes its newest observation of the 31 days
to the day. A tenor with none drops out. The benchmark is the curve's
3M tenor where it has one, else its shortest tenor. `gap` is a
holding's implied yield, else its stated rate, less the benchmark. A
currency with no curve on the day has no benchmark and no gap.

A bill's `gap` is its bond-equivalent yield less the benchmark, the
convention a bill curve is quoted in. Its `implied_yield` stays the
discount yield. To compare yields across kinds, read
`implied_yield_annual`.

**Income.** `income_12m` is a holding's expected income in the twelve
months after the day's month. The income projection computes it under
its default assumptions ([INCOME.md §12](INCOME.md)):

- today's rates hold for the whole window;
- principal that matures lands in the cash of its own account, and
  earns that cash's rate.

A coupon counts on its date, in full. In the projection, a payment
later in the current month counts whole in that month. A dividend
counts at its frequency. A deposit, a money-market fund and a floating
note accrue their rate on their balance or face, day by day.
`income_yield` is `income_12m` over value. INCOME.md §12 defines the
maturing rules and the roll.

Cash that states no rate leaves a maturing principal's income
`unknown`. The holding keeps its own tags: a fully described bond does
not turn `unknown` for it.

`income_12m` leaves out income no holding earns, such as rent or a
pension an account receives. So the total `income_12m` of `yield
global` falls short of the projection's total for the same twelve
months by that account income.

`income_100bp` is the change in `income_12m` for rates one percent
higher from today. It is the average floating principal of those
twelve months times one percent. The floating principal earns a
floating rate: a balance, a money-market holding, a floating note's
face, a floating bond fund, and principal that matures into cash. So it
is positive for cash and floating holdings. A fixed coupon adds nothing
until it matures.

**Realized yield.** `realized_yield` is the income received in the
twelve months to the day over the average value.

- The income is each `income` line that names the holding, at its own
  day's rate. A deposit adds the interest its account received with no
  holding named.
- The average value is the holding's amount at each of the twelve
  month ends and at the day, at the day's rate. A month the holding was
  not held counts as zero.

**The indicated dividend** of a stock or a fund is the first of these:

1. a ledger `dividend` record: its amount per payment at its
   frequency. It reads `declared`.
2. the last payment per share, at the frequency the year's payments
   show (1, 2, 4 or 12). The last payment lies within 400 days of the
   day. The lines paid on one day are one payment: a fund's
   distribution often comes as several. It reads `indicated`.
3. the filings' dividends per share over four quarters. It reads
   `indicated`.

With none of these, the holding pays no dividend the records show.
Held a year or more, it yields zero and reads `realized`. Bought within
the year, it reads `unknown`. A holding with no acquisition day counts
as held a year.

**Trailing twelve months** of a reported figure are its four newest
quarters, the newest at most 135 days old. A quarter two year-to-date
figures imply is their difference. The fourth quarter is the fiscal
year less its nine months. Without four quarters, the newest fiscal
year serves. The same rule a year earlier gives the growth figures.

A reported figure is read by its period's end. A figure that a later
filing restated reads restated, on any day.

## 5. Aggregation and the currency rule

A rate belongs to a currency. So an aggregate row is one entity in one
currency.

- Value, DV01, `income_12m` and `income_100bp` sum every holding. A
  liability nets.
- Every rate is weighted by value over the holdings that state it. A
  bond with no implied yield is left out of `implied_yield`, not
  counted as zero.
- Liabilities and every line of no positive value, such as an
  overdraft, are left out of the rates, unless the row holds nothing
  else. They are left out of the value `income_yield` divides by too. A
  row's rates say what its assets earn; its DV01 nets the debt.
- The total row weighs every currency by value. It carries `mixed`
  where it holds more than one. In `global` its `currency` reads
  `total`. In the other views its `silver_source` reads `total` and its
  `currency` `(all)`.
- `--converted` gives one row per entity instead, every currency
  together, and no total row. Its `currency` reads `(all)`. A row of
  more than one currency carries `mixed`.

## 6. Rate sensitivity along the curve

`curve` gives each currency's DV01 at five tenors: 3M, 1Y, 5Y, 10Y and
30Y. Its `dv100` is DV01 times 100, the value lost for a rise of one
percent. Each row splits its DV01 by what holds it:

- `funds`: bond funds and money-market funds;
- `bonds`: bonds, bills and floating notes;
- `cash`: term deposits; a deposit counts zero;
- `liabilities`: mortgages and loans owed, negative.

A bond's DV01 lands where its cash flows fall. A fund with a filed
DV01 per tenor keeps the filing's split, in the filing's currencies.
Anything else sits at its duration: a term deposit at its end, a
floating note at its reset. A time between two tenors splits linearly
between them. A time before 3M counts at 3M, one past 30Y at 30Y. The
rows of every currency sum to the holdings' DV01, within rounding.

## 7. Quality

Each row says what its figures rest on. A tag carries the share of the
row's gross value it covers: `stated=61%`, `aged<1%`. A tag that covers
the whole row prints alone. `implied` that covers the whole row does
not print: a computed yield is the norm. A `positions` or `cash` row
lists its holding's tags without shares.

| Tag | Meaning |
| --- | --- |
| `implied` | the yield is computed from the price |
| `stated` | a source states the rate |
| `indicated` | a dividend is the last payment at its frequency, or the filings' |
| `realized` | the figure comes from the transactions, where nothing else is known |
| `declared` | a ledger record states it |
| `aged` | a fund's composition is older than exposure allows (EXPOSURE.md §8) |
| `stale_price` | a fund's lines are priced on their filing's day, not the day |
| `floating` | a floating holding, with no implied yield |
| `default` | a bond is graded in default, or a fund holds lines in default or arrears; they imply nothing |
| `unknown` | no source and no record gives a figure |
| `mixed` | a total across currencies |

## 8. Config

The `yield` block of `wealthdb.cfg` holds what no source states
(DESIGN.md §5.1):

```json
"yield": {
  "curves": { "USD": { "3M": "fred:DGS3MO", "1Y": "fred:DGS1", "10Y": "fred:DGS10" } },
  "withholding": { "US": { "default": 30, "taxable_personal": 15 }, "CH": { "default": 35 } },
  "rating_funds": { "isin:XX0000000004": "BBB", "isin:XX0000000005": "AAA-A" },
  "conventions": { "EUR": { "frequency": 1, "day_count": "ACT/ACT" } },
  "horizon_months": 12,
  "cycles": { "utilities": "sensitive" }
}
```

- `curves` maps a currency to its public curve: each tenor (`3M`,
  `1Y`, `30Y`) to a rate series, as `<silver_source_id>:<series_id>`.
  The source must be configured. The curve gives the benchmark (§4) and
  the forward rates of the income projection.
- `withholding` maps a situs country to the tax it withholds at source,
  in percent, per tax wrapper. The situs is the holding's legal situs
  (EXPOSURE.md §13), the wrapper its account's. A country's `default`
  stands for every wrapper it names no rate for. Where the block names
  no rate, the income projection's `withheld` is empty, never zero.
- `rating_funds` maps a fund sliced by credit rating to the bucket its
  lines carry (`BBB`), or a range of buckets, best first (`AAA-A`). It
  is keyed like `exposure.size_funds`, and feeds the credit view of
  `exposure`, which reads a range at its worse end. The yield report's
  `rating` is a holding's own: its terms' or a ledger record's.
- `conventions` maps a market to its coupon frequency and day count. A
  market is a currency, or a currency and a bond sector or `bill`
  (`USD:government`, `USD:bill`). An entry replaces the built-in one of
  its market. It fills a bond's frequency and day count where its terms
  state none.
- `horizon_months` is the income projection's default window: the
  months after the current one, 1 to 36, default 12 (INCOME.md §12).
  The CLI and the MCP server read it from the config.
- `cycles` overrides the built-in cycle of one of the eleven sectors:
  `cyclical`, `defensive` or `sensitive`. It feeds the cycle view of
  `exposure`.

In the built-in conventions, a `CHF` or `EUR` bond pays once a year on
30/360. A `USD` bond pays twice on 30/360, a `USD:government` bond
twice on ACT/ACT. A `USD:bill` pays no coupon and counts ACT/360.

In the built-in cycles, materials, consumer_cyclical, financials and
real_estate are cyclical. Consumer_staples, health_care and utilities
are defensive. Communication, energy, industrials and technology are
sensitive.

The block is checked when the config loads. DESIGN.md §7.6 says how a
load stores it.

## 9. The ledger

The declarations ledger states the yield figures no source states: an
account's `rate`, a holding's `rating`, a stock's `dividend`, and the
`terms` of a bond, a note, a mortgage or a fund that files nowhere.
[EXPOSURE.md §9](EXPOSURE.md#yield-records) defines the four records.
A field a record states wins over the same field from any source, and
the figure reads `declared`.

## 10. Coverage

`wealthdb yield coverage` says what the figures rest on, in three
sections:

- `rests_on`: one line per tag (§7), with the value, the share of the
  gross value and the count of the holdings that carry it;
- `missing`: one line per reason below, with the same figures;
- `holdings`: each holding a figure is missing on, by `silver_source`
  and `instrument`, the largest first within each reason.

| Reason | Holdings | The record that fills it |
| --- | --- | --- |
| `no terms` | a bond, bill, floating note, note or liability that reads `unknown` | `terms` |
| `no rate` | cash with a positive balance and no rate | `rate`, naming the account |
| `no figures` | a bond fund or money-market fund that reads `unknown` | `terms` |
| `no dividend` | a stock or equity fund that reads `unknown` | `dividend` |
| `no rating` | a bond, bill or floating note without a rating | `rating` |

`wealthdb yield coverage -f ndjson` prints a skeleton of that record
for each holding the section lists:

```json
{"silver_source_id":"broker-1","instrument":"bond-1","rating":{},"note":"QORVANT CORP NOTE 4.00000% 06/30/2031"}
{"silver_source_id":"bank-2","account":"current-1","rate":{},"note":"Current"}
```

Its note is the holding's name, or the account's. A record filled in
from the skeleton goes into the ledger, and the next load takes it. An
empty object is refused at load, so a skeleton loads only once it is
filled in. A holding its source names by no instrument takes no
skeleton, but cash names its account.

## 11. The projection, the dashboard and MCP

The income projection is the forward half of `wealthdb income`: the
expected income month by month, per holding and per payment.
`income_12m` is one reading of it. [INCOME.md §12](INCOME.md) defines
it, and the withholding it shows.

The Metabase Yield dashboard (`wealthdb web`) charts the same figures
as of a day of the last twelve months:

- the income, implied and realized yields, and the projected income;
- yield by currency, cash against its benchmark, yield against
  duration, and dividend yield against payout;
- DV01 along the curve, and the income's change for rates one percent
  higher;
- the yields and the duration over time, and what the yields rest on.

Its currency, source, tax wrapper and asset class pickers narrow every
card. The cards of expected income read today's projection under four
more pickers: the rate path, the maturing rule, a shift in basis
points, and a horizon of 12, 24 or 36 months (12 by default, whatever
`horizon_months` says). Its privacy twin shows the rates as they are,
DV01 as a share of net worth per basis point, and expected income as a
yield ([web/DESIGN.md](../../web/DESIGN.md) §6 and §8).

The MCP server has a `yield` tool with the same views, an `as_of` day,
`converted`, and a `period` series between `from` and `to`. Its rows
equal the command's, cell for cell.

## 12. What yield is not

- Not a dividend model. A stock has no yield to maturity and no
  duration.
- Not a forecast. The indicated dividend stands in for analyst
  estimates, and the public curves for futures-implied rates.
- Not a credit rating. A bucket comes from the ledger, a broker's
  description or the config, never from an agency's own data.
- Not a total return. Price changes are in `gains` and `returns`.
- Not a fixed income report. Every view covers every holding; `kind`
  picks out the bonds.
