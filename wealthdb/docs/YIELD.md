# Yield

What the holdings earn, and how their value moves with interest rates,
as of a day. The yield report gives each holding its yields, its
duration and its rate sensitivity, and sums them by currency at every
grain `holdings` has. This file defines every figure the report shows
and the rules behind them.

Related design:

- DESIGN.md §4.18: the `yield` command;
- DESIGN.md §7.6: the reference, config and derived tables;
- DESIGN.md §10.15: the code;
- [EXPOSURE.md §9](EXPOSURE.md#yield-records): the ledger's yield
  records;
- [INCOME.md §12](INCOME.md#12-the-forward-views): the income
  projection;
- migrations 0134 to 0136; 0138 for the dashboard's tables; 0141 for
  what a fund's filing states of its lines.

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
- **The twelve months to the day** start the day after the same day a
  year earlier, and end with the day.
- **A holding** is a position or a cash balance with an amount on the
  day. One whose amount rounds to no cent of its currency is no holding
  and has no row. Like a position sold, its income counts for its
  security, and the income views list it as a holding no longer held.

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
- Under `-p` every name a person typed prints as a pseudonym, such as
  `manual-1`, or as its account's redacted id (DESIGN.md §4.6). That
  covers a nickname, a typed account or portfolio name, and a manual
  holding's name, key and instrument.
- Under `-p` any other `security` or `fund` key that is a holding's own
  (`holding:<source>:<id>`, no public key) is hidden whole, as
  `exposure securities` hides it. So is `coverage`'s `instrument`. Any
  other `position_key` stays legible, as `holdings positions` keeps it.
  Rates, durations, shares, labels and public security keys stay.

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
| `realized_yield` | aggregates, `positions`, `cash` | the income of the twelve months to the day over the mean daily value; a holding of an instrument shows the instrument's (§4) |
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
| `direct_value`, `via_funds_value`, `funds` | `securities` | as `exposure securities` gives them |
| `share` | `securities`, `coverage` | the share of net worth, as `exposure securities` gives it; in `coverage`, of the gross value (§10) |
| `lines_yield`, `lines_duration`, `lines_maturity`, `lines_as_of`, `lines_covered` | `funds` | what the fund's lines imply, the day of its composition, and the share of the fund they cover |
| `floating`, `in_default`, `paid_in_kind` | `funds` | the shares of the fund in floating lines, in lines in default or arrears, and in lines paid in kind |
| `duration`, `maturity` | `funds` | the figures the report reads: a ledger record's duration, else the page's or filing's figure, else the lines' |
| `yield_to_maturity`, `distribution_yield`, `sec_yield`, `seven_day_yield`, `trailing_yield` | `funds` | what the fund's page or filing states; the yield to worst where one is stated |
| `as_of`, `source`, `risk_as_of` | `funds` | the day the figures describe; where they come from (`lines`, `page`, `filing`, `ledger`); the day of the filed DV01 |
| `rate`, `tiers`, `tier_up_to`, `valid_from`, `term_end` | `cash` | the rate the balance earns, tiers blended; the number of tiers; the first tier's limit, in the balance's own currency; the day the rate holds from; a term deposit's end. An overdraft lists its debit tiers |
| `tenor`, `dv100`, `funds`, `bonds`, `cash`, `liabilities` | `curve` | the tenor (§6); DV01 times 100, and DV01 by what holds it (§6) |
| `section`, `reason`, `holdings`, `payments`, `instrument` | `coverage` | §10 |

The keys are the ones `holdings` and `exposure` name: `silver_source`,
`portfolio`, `portfolio_id`, `account`, `account_id`, `account_kind`,
`tax_wrapper`, `account_nickname`, `position_key`, `symbol`, `name`,
`asset_class`, `vehicle`, `currency`, `security`, `ticker`, `sector` and
`fund`. `-C all` prints every column. `wealthdb help yield` lists each
view's columns and defaults, and the quality tags. The defaults:

| View | Default columns |
| --- | --- |
| aggregates | the grain's keys (`silver_source`, `portfolio`, or `account` and `tax_wrapper`), `currency`, `value`, `income_yield`, `implied_yield`, `realized_yield`, `duration`, `dv01`, `quality` |
| `positions` | `silver_source`, `account`, `symbol`, `kind`, `currency`, then the aggregates' |
| `securities` | `security`, `name`, `asset_class`, `value`, `share`, `coupon`, `maturity_date`, `implied_yield`, `earnings_yield`, `dividend_yield`, `quality` |
| `funds` | `fund`, `name`, `kind`, `currency`, `value`, `implied_yield`, `duration`, `dv01`, `distribution_yield`, `sec_yield`, `as_of`, `source`, `quality` |
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
| `bond_fund` | a fund mostly of debt, or one that files its rate risk (below) | a ledger record's yield, else what its lines imply, else what its page states (below) | a ledger record's, else its page's, else its lines' |
| `equity_fund` | a fund mostly of equity (below) | its earnings and dividend yields (below) | none |
| `multi_asset` | a fund of asset class multi asset | a ledger record's yield, else its page's; its dividend yield as an equity fund's (below) | a bond fund's, its lines' over the whole fund |
| `stock` | a stock of asset class public equity | its earnings and dividend yields (below) | none |
| `liability` | a mortgage, or a loan owed | its rate, from its terms | to the end of its fixed period (below) |
| `other` | anything else: a property, a private fund, metal, a coin | its realized yield; its expected income repeats what it was paid last year (`realized`); paid nothing, it reads `unknown` | none |

Every kind has a `realized_yield` and a `benchmark` (§4). A stock and an
equity fund have no `gap`. A multi-asset fund's `gap` is its implied
yield's alone. A liability's value and DV01 are negative,
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
before the day states it, among the rates that hold from on or before
the day. A rate a source states once holds from its
`valid_from` on. A ledger `rate` record holds from its day on and reads
`declared`. It replaces the credit rates a source states for its account
and currency.

A source states a rate on a cash balance's payload, or on an account's,
under these keys:

| Key | States | Read as |
| --- | --- | --- |
| `interest_rate` | the rate | percent a year: `0.75` or `0.75%` |
| `interest_tiers` | a tiered rate, in place of `interest_rate` | a list of `{"rate": 0.5, "up_to": 10000}`; the last tier's `up_to` is null |
| `interest_valid_from` | the day the rate holds from | `2026-01-01` |
| `interest_side` | who pays it | `credit` (the balance earns it, the default) or `debit` (it pays it) |
| `interest_conditions` | several rates, such as a credit and a debit side | a list of objects, each with the keys above |

A rate on a cash balance is read at that balance's snapshot. A source
puts a rate on the balance its document or capture is dated by, or on
the newest one before it. The adapter docs say which. A rate on an
account is stated once, for the account's base currency. It is read
only on a kind of account that holds cash: `cash`, `brokerage`,
`custody` or `safekeeping`. Each condition in `interest_conditions`
states its own keys, one per side and day. A
value that does not read states nothing, and neither does the condition
it sits in. A balance that states UBS PSN's tiers reads those, and the
keys beside them state nothing. A liability's rate is its terms'
(below), never these keys. UBS PSN's balance records, Raiffeisen's
account details and VIAC's strategy rate are read in their own shapes.

An account that states no rate has none: its cash reads unknown, never
zero.

A card account's balance is no deposit that earns. Its bill is paid in
full, so it takes no rate, no tag and no expected interest. No rate is
missing on it, and a ledger `rate` record for it states nothing.

A balance whose rate no source states reads `realized` where its
account received interest in the twelve months to the day. Its expected
income repeats that interest
([INCOME.md §12](INCOME.md#12-the-forward-views)). With no interest
received it reads `unknown`. `coverage` lists it under `no rate` either
way (§10).

**Bonds.** Each field of a bond's terms comes from the first of these
that states it: the ledger's `terms` record in force on the day, the
source's own record, the broker's description, OpenFIGI's ticker. The
frequency and the day count fall back to the market's convention (§8).
A floating bond takes only the day count from it, also where only a
ledger record calls it floating. A bond whose market has no convention
pays once a year, on ACT/ACT. A coupon with no stated kind is fixed, or
a zero where it is zero.

The rating is the ledger's `rating` record's. Else it is the worst grade
the source's own record states, else the lower of the grades the
description states. An agency's word that it rates nothing (`NR`, `WR`)
counts only where neither states a grade, and makes the bond unrated.
The grades that give the rating say whether the bond is in default (`D`,
`SD`, `RD`), so a ledger `rating` record clears a default. A bond graded
in default carries `default`, and has no implied yield and no duration.
Its projection reads `unknown`: no coupon and no principal (INCOME.md
§12).

The source's own record is a payload, under the keys below. The coupon
is in percent. Every other value is as the source prints it. A key the
instrument's payload states wins. The other keys come from the newest
position whose payload states any of them. So a bond keeps the terms a
statement printed after a live feed describes it without them.

| Key | States | Read as |
| --- | --- | --- |
| `interest_rate` | the coupon | percent a year: `4.25` or `4.25%` |
| `maturity_date` | the maturity | `2031-06-15`, `06/15/2031`, `06/15/31` or `15.06.2031` |
| `coupon_kind` | the coupon's kind | the coupon words a description uses: `FIXED` or `STEP` fixed; `FLOATING`, `VARIABLE` or `FRN` floating; `TIPS` inflation-linked; `ZERO` or `STRIP` zero, with a coupon of 0 |
| `coupon_frequency` | the payments a year | `SEMIANNUALLY` or `SEMI-ANNUAL` 2, `QUARTERLY` 4, `MONTHLY` 12, `ANNUALLY` or `ANNUAL` 1; `AT MATURITY` states none, since a zero is no bill |
| `call_date` | the first call | a date, as `maturity_date` |
| `rating_moodys`, `rating_sp`, `rating_fitch` | each agency's grade | its bucket: `Aa2` and `AA-` are AA, `NR` and `WR` unrated |

A value the table does not read states nothing. A call date the record
states clears a make-whole call the description names, as a ledger
record's does.

fidelity-web's and svb's statements fill them from the lines below a
bond's row ([fidelity-web](../../collectors/fidelity-web/DESIGN.md)
§4.5, [svb](../../collectors/svb/DESIGN.md#modelling-decisions)). Two
other sources add to them:

- UBS: the `Rating` column of the positions export is one more grade,
  read off the newest position that carries it. Text that is no grade
  (`No rating av.`) gives no rating.
- manual: a note's record gives `interest_rate` and `maturity_date`.

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
- `current_yield` is the coupon over the price.
- A floating note lasts to its next reset: the one its terms state,
  else its next coupon date. Terms that state no frequency reset it
  quarterly, in every currency.

**A fund's kind.** After two rules of its class, a fund's kind follows
what it holds, not the class a source gives the wrapper. The first of
these that holds gives it:

1. `money_market`: a fund of asset class cash, or one with a seven-day
   yield;
2. `multi_asset`: a fund of asset class multi asset;
3. `bond_fund`: a fund that files its rate sensitivity, or whose lines
   are more than half debt;
4. `equity_fund`: a fund whose lines are more than half equity;
5. its asset class: fixed income reads `bond_fund`, public equity and
   real estate `equity_fund`, any other `other`.

A fund of class metal, crypto or other, where a commodity fund is,
skips rules 3 and 4. So does a fund whose debt all matures within a
year beside derivative lines: the bills a futures or swap fund holds
as collateral.

The lines are the composition exposure reads on the day (EXPOSURE.md
§4), by weight, as shares of the whole fund: what a composition leaves
out counts as neither. A line is debt where its asset type maps to
fixed income. It is debt too where its type maps to nothing and the
line states a coupon or a yield, floats, or is in default or paid in
kind. It is equity where its type maps to public equity or real
estate, and a derivative where it maps to other or foreign exchange.

**Funds.** A fund's figures are the newest on or before the day,
within the exposure's `max_gap_days` (EXPOSURE.md §4). A ledger `terms`
record's `yield` and `duration` win over the page, the filing and the
lines ([EXPOSURE.md §9](EXPOSURE.md#yield-records)). They read
`declared`, as of the record's day. `source` reads `ledger` where the
yield is the record's.

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
- It carries `floating` where its income resets with the rates. That
  is so where its lines float for half of it or more, or the lines of
  its filing at the day do. It is so too where its name says it floats.
  The holding's name counts, and so does the name its filing gives it.
  These words say so:
  - FLOATING, FLOATER, FLTG, FRN, FRNS or FLOAT RATE;
  - a broker's short forms FLT RT and FLOAT RT;
  - VARIABLE RATE or VAR RATE;
  - a fund of loans or of CLOs: SENIOR LOAN, BANK LOAN, LEVERAGED LOAN
    or CLO.

  A bare FLOAT does not count: an index fund's name may say FLOAT
  ADJUSTED. Nor does FIXED-TO-FLOATING, FIXED TO FLOAT or VARIABLE RATE
  PREFERRED: those coupons are fixed until they float.
- It carries `default` where any line is in default or arrears.

A multi-asset fund reads these:

- A record's or a page's yield and duration are the fund's, as they
  state them. Its lines' figures are its debt's.
- Its duration and DV01 are a bond fund's. Its lines' duration counts
  over the whole fund: a line without one counts as zero. Its DV01
  then sits on the curve at its debt's own duration.
- `implied_yield` is the record's yield, else the yield to worst its
  page or filing states, else the yield to maturity. Its lines give
  none: their yield is its debt's alone.
- Its dividend yield and its expected income are an equity fund's
  (below).
- It carries `floating` and `default` as a bond fund does.

A line states its own yield and duration, or its price implies them
from its coupon, maturity and face, on ACT/ACT. It pays at its
currency's convention, else once a year. A line's yield to worst
wins over its yield to maturity. A security held only through funds
reads the line of the most value held: its weight times the value of
the fund holding it.

A line that states no coupon kind, no default flag or no arrears flag
takes what its fund's filing states of the same security, as
`exposure coupons` reads the kind (EXPOSURE.md §7). A value the line
states stays.

- A floating line counts in no yield of the fund's lines.
- A line in default or arrears, or paid in kind, has no yield, stated
  or implied. It counts in no yield of the fund's lines, and it shows
  none in `yield securities`. There a line in default reads `default`.

**Stocks and equity funds.** A stock's `earnings_yield` is its trailing
twelve months' diluted earnings per share over its price (§4). An
equity fund's is the inverse of the P/E its page states.
`dividend_yield` is the indicated dividend over the price (§4). An
equity fund or a multi-asset fund reads the trailing yield, else the
distribution yield, its page states, in two cases:

- it has no indicated dividend;
- it was held under eleven of the twelve months, and its indicated
  dividend rests on one calendar month (§4).

Where its page states neither, the one-month dividend stands. Earnings
or dividends a filing reports in another currency than the price give
no yield.

`securities` adds a stock's dividend safety, from its trailing figures:

- `pe`: the price over the earnings;
- `payout`: the dividends per share over the earnings;
- `dividend_growth` and `earnings_growth`: each against a year earlier;
- `coverage`: the operating income over the interest expense.

## 4. Benchmark, income and dividends

**The benchmark.** A currency's curve is the series the config names
per tenor (§8). Each tenor takes its newest observation of the 31 days
to the day. A tenor with none drops out. The benchmark is the curve's
3M tenor where it has one, else its shortest tenor. On the Treasury's
basis it is that tenor's rate as published. On any other basis it is
the curve's zero rate at that tenor, compounded twice a year (§8).
`gap` is a holding's implied yield, else its stated rate, less the
benchmark. A currency with no curve on the day has no benchmark and no
gap.

A bill's `gap` is its bond-equivalent yield less the benchmark, the
convention a bill curve is quoted in. Its `implied_yield` stays the
discount yield. To compare yields across kinds, read
`implied_yield_annual`.

**Income.** `income_12m` is a holding's expected income in the twelve
months after the day's month. The income projection computes it under
its default assumptions
([INCOME.md §12](INCOME.md#12-the-forward-views)):

- today's rates hold for the whole window;
- principal that matures lands in the cash of its own account, and
  earns its account's rate in that currency.

`income_yield` is `income_12m` over value. INCOME.md §12 defines when
each payment counts, the maturing rules and the roll. The holding keeps
its own tags: a fully described bond does not turn `unknown` where its
account states no rate for the cash its principal lands in.

`income_12m` leaves out income no holding earns, such as rent or a
pension an account receives. So the total `income_12m` of `yield
global` falls short of the projection's total for the same twelve
months by that account income.

`income_100bp` is the change in `income_12m` for rates one percent
higher from today. The floating principal earns a floating rate: a
balance, a money-market holding, a floating note's face, a floating
bond fund that earns a floating rate (INCOME.md §12), and principal that
matures into cash. A floating rate never earns below zero (INCOME.md
§12). So `income_100bp` is the floating principal times one percent
where its rate stands at or above zero, less where it stands below
zero, and nothing where it stands a point or more below. A fixed coupon
adds nothing until it matures.

**Realized yield.** `realized_yield` is the income received in the
twelve months to the day over the mean daily value.

- The income is each `income` line that names the holding, at its own
  day's rate. A deposit adds the interest its account received with no
  holding named.
- A dividend counts at its gross where its source states one, else at
  its net (INCOME.md §5).
- A line of an account that held the instrument once and sold it stays
  with that sold position, as `income` lists it. A coupon does too. A
  line of an account that never held it belongs to a holding of it in
  its portfolio: a bank books a dividend to the cash account beside the
  custody account. Where no account of the portfolio holds it but one
  held it once, the line stays with its own sold position. Else it
  belongs to a holding of it at its source. Where the source holds none,
  and one account of the source alone held it, the line is that
  account's sold position: a bank may book a dividend in another
  portfolio.
- A line meets its holding under any id its source gives the
  instrument. A source may key one security by its CUSIP and by its
  ticker. The config may give one holding two keys (`holding_aliases`,
  DESIGN.md §5.1). So a line under one id belongs to the holding under
  the other. An alias for one account covers that account's lines, and
  the lines its portfolio's other accounts book for it. A sold position
  is named by the ticker, or by the id the config's alias gives. The
  holding keeps its own key.
- A broker may book a bond's coupon as the account's interest, naming
  no holding. Its description then names the bond by its coupon and
  maturity (`US TREASU NT 9.875%09/99UST NOTE DUE 09/09/99`). Such a
  line is the coupon of the one bond of its account that it names.
- Where the account holds no such bond, or two, a line that names a
  bond issue (a Treasury, a note, a bond) is the account's coupons
  (`income:coupons:<CCY>`): no deposit's interest, and no holding's. So
  is a coupon whose line names a bond that neither its account, its
  portfolio, nor one account of its source alone held. Any other
  line stays interest, such as a deposit's that states its rate and
  its end.
- A day's amount is the one `holdings` reads at the day's end: its
  account's active snapshot then (DESIGN.md §10.7).
- A holding of an instrument, such as a stock, a fund or a bond, yields
  its instrument's realized yield. The instrument is the security,
  at every source; one with no security key is the instrument at its
  source. The income is that of every account that held it, at any
  source, an account that sold it included. So a security moved from one
  source to another keeps what the first paid and held. The amounts of a
  position that is no holding convert as a holding of its instrument in
  its currency does, else at the day's rate. It is over the
  instrument's mean amount on the days any account held it, summed over
  every account. A day no account held it does not count. A holding
  held half the year so reads half its yearly rate, and carries
  `partial`. Every holding of the instrument shows that figure.
- A cash balance's mean daily value is its amount on each day of the
  twelve months, at the day's rate, over the days. A day it was not
  held counts as zero.
- A row's realized yield is its holdings' own income over the sum of
  their mean daily values. Each mean counts every day, a day not held
  as zero, so a row reads a yearly rate. It is no mean of their yields.
  The holdings are the ones its other rates weigh (§5).
- A day is thin where the amount is under a tenth of the mean daily
  amount, in size. A day not held is thin. A holding or a row thin on
  more than half the days has no realized yield. It carries `thin`. So
  a holding of an instrument held under half the year has none.

**The indicated dividend** of a stock or a fund is the first of these:

1. a ledger `dividend` record: its amount per payment at its
   frequency. It reads `declared`.
2. the last month's payment per share, specials aside (below), at the
   frequency of the year's pay months. The payment lies within 400 days
   of the day. It reads `indicated`. A payer twice a year pays its
   interim and its final, which often differ, each in its own half: the
   last month's payment, and the newest payment within 400 days in the
   other half of the year. The other half's months lie 1 to 10 months
   from the last month, before or after it. A month 11 months or more
   away is the last's own payment a year apart, as the frequency counts
   it (below). Each half pays again in its own month, a year after its
   newest pay day.
   - Where that half has no rate but a special, in a month the year
     counts (below), the special stands as its payment in two cases. It
     stands where it is up to six times the last month's, as an uneven
     final is no special. It also stands where the same month a year
     earlier paid within a third of it, as a large final that recurs is
     the payer's schedule.
   - Where that half's newest month still has no rate, the same month
     of the calendar exactly a year earlier stands for it, if that month
     has a rate. A month off does not stand in, unlike a drifted pay day
     in the frequency's count (below).
   - Where the other half has no rate even then, and the year's pay days
     fall in exactly two months of the calendar, the one rate pays once
     a year, in its own month. A pay day a year on counts once here
     (below). It covers part of the year and reads `partial`.
3. the filings' dividends per share over four quarters. It reads
   `indicated`.

The indicated dividend is the instrument's: the security's, at every
source, as for the realized yield. Every holding of it shows the same
one, and the projection pays it on each holding's own quantity. The
payments of every account count, at any source, an account that sold
the instrument included.

- An account's lines of one day are one payment: a fund's distribution
  often comes as several, income and capital gains apart. Two sources'
  accounts of one id are two accounts.
- A payment per share is the payment over the quantity the account was
  entitled to: what it held as its record day began, or as its ex day
  began where the calendar gives the days (below). The record day comes
  from the first of these that gives one. The coverage view counts each
  source (§10):
  1. `stated`: the dividend calendar's record day
     ([adapters/refdata.md](adapters/refdata.md) §2). The payment's
     distribution is the one whose pay day lies nearest the payment's
     day. That pay day lies from ten days before it to three after.
     Where the calendar states no pay day, the distribution is the one
     whose record day, else ex day, lies latest in the record window
     (below). Where the market fixes no lag, that window is the 105 days
     to the payment for a share or a receipt, as a Japanese share pays
     some three months after its record day. For a fund the window is
     the 63 days to the payment. A distribution whose record day or ex
     day lies after the payment is not its. A distribution its source
     estimates loses to one it does not, however near.
  2. `derived`: that distribution states an ex day and no record day.
     The record day is the ex day plus the market's settlement cycle on
     the ex day, less one, in business days (below). Under a cycle of
     two it is the next business day. Under a cycle of one it is the ex
     day. The trade the day before the ex day is the last that earns the
     payment. It settles on the record day. A stated ex day alone counts
     only where a cycle derives its record day, no later than the
     payment's day. So a share of Hong Kong, which has no cycle, reads
     its issuer's learned gap, else the lag.
  3. `learned`: no distribution is the payment's, or it gives no record
     day, and the issuer's own distributions teach a gap (below). The
     record day is the pay day less the gap.
  4. `lag`: no gap is learned either, and the market fixes a lag. The
     record day lies a fixed number of days before the pay day, by the
     market of the security paid. Each market also has a record window:
     the days before the pay day in which its record day falls.

  A market with no lag then leaves the record day unknown (`none`).

  | Market | Lag | Window before pay |
  |---|---|---|
  | a share quoted in dollars | 30 days | 3 to 35 days |
  | a share of the United Kingdom or Hong Kong | 30 days | 14 to 63 days |
  | a share of Switzerland, Denmark or a euro market below | 2 business days | 2 to 4 business days |
  | a share of Sweden | 4 business days | 4 to 6 business days |
  | a receipt of a Japanese share | 90 days | 70 to 105 days |
  | a receipt of a share of a market with a lag in business days | 14 days | 7 to 42 days |
  | a receipt of a share of the United Kingdom | 30 days | 21 to 63 days |
  | any other receipt | 30 days | 7 to 63 days |

- The euro markets are Germany, France, Italy, Spain, Austria and
  Belgium. In these markets, in Switzerland and in Denmark, the ex-day,
  the record day and the pay day fall on consecutive business days, and
  a trade on the ex-day earns nothing. So the lag reaches back to the
  ex-day. A Swedish share pays three business days after its record day,
  which follows its ex-day by one, so its lag reaches back to the ex-day
  too. The window reaches two business days further: a payment booked
  late, or a closing day the calendar below does not know, then falls
  inside it.
- A business day is a weekday on which the market's settlement system
  is open: a record day is a settlement day. A Monday's lag of 2
  business days reaches back to the Thursday. The closing days follow
  from the year, Easter by the Gregorian computus:

  | Market | Settlement system | Closed on |
  |---|---|---|
  | the euro area | TARGET2 | 1 January, Good Friday, Easter Monday, 1 May, 25 and 26 December |
  | Switzerland | SIX SIS | TARGET2's days; 2 January, Ascension, Whit Monday, 1 August, 24 and 31 December |
  | Sweden | Euroclear Sweden | TARGET2's days; 6 January, Ascension, 6 June, Midsummer Eve (the Friday from 19 to 25 June), 24 and 31 December |
  | Denmark | Euronext Securities Copenhagen | 1 January, Maundy Thursday, Good Friday, Easter Monday, Ascension and the day after, Whit Monday, 5 June, 24, 25, 26 and 31 December |

  The euro area is its 21 members. So 2 business days before Monday 5
  January 2026 is Wednesday 31 December 2025 in the euro area, and
  Monday 29 December 2025 in Switzerland.
- A share's market is that of the currency it is quoted in, whatever its
  issuer's country:
  - dollars: the United States;
  - pounds or pence: the United Kingdom;
  - Hong Kong dollars: Hong Kong;
  - francs, Danish kroner, Swedish kronor: Switzerland, Denmark, Sweden;
  - euros: the issuer's market, where that is one of the euro markets
    above.

  Any other share has no lag. Neither has a fund, or any position that
  is neither a share nor a receipt: without a record day from the
  calendar or a learned gap, its payment reads the quantity of its pay
  day (below).
- A US common stock reads the lag unless its filings tag the dates: no
  free source states most US companies' record days. `sec-edgar` reads
  them from a 10-Q's or 10-K's tags where a company states them.
- An issuer's gap is learned from the distributions the calendar states
  of it:
  - Each distribution counts the business days from its record day to
    its pay day. A distribution that states only an ex day counts from
    the record day its market's cycle derives. Where the market has no
    cycle, a distribution counts from its ex day, the day its quantity
    reads by, where it states one.
  - The gap is the median of these counts, the lower of the middle two.
    It needs two distributions. One paid more than 90 days after its
    record day, or before it, counts for nothing, and so does an
    estimate. A distribution stated again, as for another share class,
    counts once.
  - A company's issuer is its EDGAR filer, so its share classes of one
    market learn together. A receipt quoted in another market than its
    share learns apart, as its depositary sets its own days. Any other
    security is its own issuer: a fund by its share class.
  - A gap counts the business days of the distribution's market
    (below), else of its ISIN's country.
  - A fund with no gap of its own takes the gap of its pool. The funds
    of the United States pool together, the European funds together,
    and the funds of any other market apart. A pool needs two funds
    with a gap. A share takes no pool's gap: a fund pays days after its
    record day, a company weeks.
  - The gaps are rebuilt at every load (`dividend_gaps`).
- Where the calendar states or derives the record day, the quantity is
  the one held as the ex day began. Holdings count by trade date, and a
  trade on the ex day earns nothing. The ex day is the one the
  distribution states. Else the ex day lies the cycle less one, in
  business days, before the record day, by the market's cycle on the
  record day. Where the market has no cycle then, the record day stands
  for it. Under a cycle of one day, the two days are one.
- That day is the payment's record window alone, so a trade near it
  leaves the quantity known. A fund's payment, which no lag dates, rates
  on that day once the calendar gives it.
- Where a learned gap gives the record day, the quantity is the one held
  as its ex day began, as for a stated record day. The record window
  runs from two business days past the gap's longest to its shortest,
  as a lag's does. Each end reads by its ex day, as above.
- A market's settlement cycle is the business days a trade settles
  after. It changes on a day, from that day on:

  | Market | Settlement cycle |
  |---|---|
  | the United States | 3 days; 2 from 2017-09-05; 1 from 2024-05-28 |
  | Canada | 3 days; 2 from 2017-09-05; 1 from 2024-05-27 |
  | the euro area, Switzerland, the United Kingdom, Sweden, Denmark, Norway | 2 days from 2014-10-06; 1 from 2027-10-11 |
  | Japan | 3 days; 2 from 2019-07-16 |

  A market of no cycle derives neither day from the other, nor does a
  day before a market's first cycle. Hong Kong has none: a share's
  record day there follows the closure of its register. The config
  replaces a market's cycles (§8).
- A distribution's market sets its settlement cycle and its business
  days. A share's is its market as for the lag (above). A share in euros
  reads its issuer's market wherever that is of the euro area, not only
  in the six euro markets above. Yen, Canadian dollars and Norwegian
  kroner name Japan, Canada and Norway. A receipt's market is that of
  the currency it is quoted in, not its share's. So a dollar receipt of
  a Japanese share settles in the United States. A fund's market is its
  country, else its currency's.
- A distribution's business days are those of the calendar of
  settlement days above. A market the calendar does not list reads
  TARGET2's. So a day derived in Japan or the United States steps over
  TARGET2's closing days, not its own, and may miss by a day.
- The currency is the one the instrument is listed in, where gold states
  one. Else it is the one the position's newest snapshot values it in,
  newest across every id the instrument folds under. Two positions seen
  at that instant that give different lags give none. A statement may
  value a position in its portfolio's currency: the position then reads
  that currency's market.
- A receipt's market is that of the share it stands for: its situs
  (EXPOSURE.md §13). Any position the situs rule reads as a receipt is
  one, whatever its basis. A receipt whose situs is declared reads the
  declared country as its share's. A receipt whose share is unknown has
  no lag. A receipt gold does not know as one reads as a share of its
  currency's market.
- A payment rated on its record day's quantity counts in no rate where
  the account's most over its least across the record window is above
  1.1, or where some of the window is unknown. A trade near the true
  record day leaves the quantity paid on unknown. The days the account
  held none count nothing: the account was paid, so none of them was its
  record day. The other accounts paid that day, or the last month with a
  rate, give the rate, and the day still counts in the frequency.
- Where no record day is known, the quantity is the one held as the pay
  day began. So it is where the account held none on the day its
  quantity reads, or no snapshot says what it held, or its first
  snapshot falls inside the record window. An account paid that held
  none on the record day the fixed lag gives held some on the true one.
- That quantity comes from the account's newest snapshot by then. The
  account's trades since that snapshot move it. The day's own trades
  earn none of the payment.
- A trade booked on a cash account that holds none of its instrument as
  the trade happens moves another account's holding. That is the one
  other account of its portfolio that holds it then. A bank books a
  trade on the cash account beside the custody account. Where several
  accounts hold it then, or none, the trade moves no holding, since its
  account is not known. A payment that reads all the portfolio's
  accounts then reads their snapshots without that trade. A trade of any
  other kind of account moves only its own holding.
- A trade stamped at a snapshot's instant counts in it, as the lot
  engine reads a snapshot. Some statements leave out their last day's
  trades: where the snapshot shows the previous one's quantity without
  the trade, the trade counts after it.
- A snapshot of only some accounts, such as a statement's, says nothing
  of the others. From the snapshot that ends an account's last
  observation (DESIGN.md §10.7), the account holds nothing.
- The quantity counts the instrument under every id its source gives
  it, as a line meets its holding: a symbol and a CUSIP, say, or a
  holding alias.
- A payment booked to a cash account that never held the instrument
  reads the quantity its portfolio's accounts held: a bank books a
  dividend to the cash account beside the custody account. Where no
  account of the portfolio held it, and one account of the source alone
  did, it reads that account's: a bank may book it in another
  portfolio.
- A payment whose quantity is unknown counts in no rate, such as one
  whose pay day comes before its account's first snapshot. So does one
  that reads its pay day's quantity where the account held none then:
  one that sold before its pay day. Today's quantity never stands in.
- A payment that reads its pay day's quantity counts in no rate where
  that is under two thirds, or over one and a half times, the quantity
  that guards it. The holding may have changed between its record day
  and its pay day. Where no record day is known, the guard is what the
  account held 100 days before. Where one is, it is the first quantity
  the account shows held after the day its quantity reads: the record
  day, or the ex day. A buy just before that day may show only then: a
  snapshot that holds none of an instrument moves by no trade, as it
  may hold it by another id.
- The accounts paid on one day pay one rate: the median of theirs.
- A payment per share above twice the median of the year's payments per
  share is a special: a special dividend, or a return of capital. With
  fewer than three payments in the year, the median is that of the 24
  months to the day. With fewer than four there, a special is a payment
  above four times their median, the smaller one's with two. So a final
  two or three times the interim is no special among a few payments.
- A payment above that bar is no special where it holds the level of the
  newest pay days. That is where it lies in the run of pay days that
  ends at the newest: two or more, each with a rate, all within a third
  of one another, one of them in another month of the year than its
  own. A payer that raised its dividend steeply pays its new rate on
  every pay day since. Two specials of one size between regular pay days
  stay specials, also where a regular one between them has no rate.
- A special counts in no rate, except as a half's payment of a payer
  twice a year (above), but it counts as income received.
- A month's payment per share is the sum of its days' that are not a
  special: two lines of one month on different days are one payment.
  Each account's days sum, and the month pays the median of the
  accounts'.
- The frequency is the number of pay months in the twelve months to
  the day, scaled to a year by the months held in them. A holding held
  five months that paid in each of them pays monthly. The months held
  are the days any account held the instrument, as for the realized
  yield.
- Each month of the calendar counts once. A pay month and one 11 or 12
  months after it are one payment, the same one a year on: a pay day
  drifts by days each year, into its own month or a neighbouring one.
- The count goes no faster than the two closest of those months allow,
  round the year: October and July are three months apart. Nor does it
  go faster than the two closest pay months in time allow: a monthly
  fund held in two stretches a year apart keeps its gap of one month.
  Two payments six months apart are twice a year's, whatever the
  months held.
- The count reads as the nearest of 1, 2, 4 or 12. Halfway between two,
  it reads the higher, where the two closest pay months are that close.
  So a quarterly payer that missed one payment of the year reads 4, not
  2. A payer twice a year whose pay day drifts across a month end still
  reads 2.
- Two months of the calendar, more than a month apart, may be the two
  halves of a payer twice a year. They are where both of these hold:
  - their payments per share differ by more than a third of the
    smaller;
  - the holding covered a quarter before the older month, or a quarter
    after the newer, and nothing was paid in it.

  The quarter is the month three months off and its neighbour beyond
  it, as a pay day may drift into a neighbouring month. The holding
  must cover that quarter, from as far before it as a record window may
  reach:
  - at least 35 days, the window of a share of the United States;
  - 63 days for a lag of 30 days, which may be a share of the United
    Kingdom's;
  - for a payment the calendar dates, back to the day its quantity
    reads.

  So a payment due in that quarter would show. The two months then read
  2, however short the holding. Else they read as above. A quarterly
  payer that raised its dividend, or pays more in one season, pays in
  that quarter. Two months next to each other are never halves: they
  may be a monthly payer's.
- One calendar month alone shows no rhythm: it reads once a year. Held
  eleven of the twelve months or more, it is a yearly payer's: a
  quarterly payer pays three times in them, and a transfer's gap of a
  few days does not count. Held less, it may be any payer's: a fund held
  a month would read a twelfth of what it earns. So where a fund held
  under eleven months shows one calendar month, the yield its page
  states wins, where it states one:
  - a bond fund's distribution or SEC yield, else its trailing yield;
  - an equity or multi-asset fund's trailing yield, else its
    distribution yield.

  A stock keeps its reading.
- A month whose only payments are specials counts where the same month
  a year earlier had a payment: the special fell on the payer's own
  schedule, as a trimmed holding's payment does. Else it does not
  count, such as a yearly payer's one-off return of capital.

**Units.** A payment per share counts in the units the instrument has
on the day. A split, a reverse split or a change of a depositary
receipt's ratio changes them. Between two snapshots, a change of
quantity that the account's trades do not explain is a change of units:

- where a `corporate_action` transaction of the instrument names it.
  The legs count once each, as the lot engine dedupes them (LOTS.md
  §3.5). They must state the change: their sum is the units delivered
  or taken back, or one leg states the new total. A single leg that
  takes units back is a reverse split here.
- where the price per unit moved by the inverse of the change, nearer
  to it than to staying put.

Any other such change is a trade no transaction records. So is a
change under three for two either way, such as a scrip dividend. A
change of units is a whole number n of new units for each old one, or
one new unit for n old ones. It reads from the quantities, the trades
between the snapshots before the change or after it. The new quantity
is exact, or short of a fraction paid in cash. Every payment per share
before the change divides by its factor.

The holding carries `units`, and the payments stay as they are, where:

- no whole number explains a change, or more than one does, as on a
  small position after a reverse split;
- the legs state another change than the quantities show;
- two accounts show a change at one snapshot differently;
- some span of snapshots since the payment had no account holding the
  instrument with its quantity stated at both ends, so a change in it
  would not show.

A payment or a record in another currency than the holding's converts
at the day's rate. One that no rate converts counts for nothing.

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

- Value, `income_12m` and `income_100bp` sum every holding of the
  row's currency. A liability nets.
- DV01 belongs to the currency of the rates that move it. A fund whose
  filing or lines state its DV01 per currency puts each currency's part
  on that currency's row; its value stays on its own currency's row.
  The parts are in the shares of its DV01 per tenor (§6). The total row
  and `--converted` sum every holding's DV01 whole.
- Every rate is weighted by value over the holdings that state it. A
  bond with no implied yield is left out of `implied_yield`, not
  counted as zero. `realized_yield` is the exception: the holdings'
  income over their summed mean daily values (§4).
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

- `funds`: bond funds, multi-asset funds and money-market funds;
- `bonds`: bonds, bills and floating notes;
- `cash`: term deposits; a deposit counts zero;
- `liabilities`: mortgages and loans owed, negative.

A bond's DV01 lands where its cash flows fall. A fund with a filed
DV01 per tenor keeps the filing's split, in the filing's currencies.
Anything else sits at its duration: a term deposit at its end, a
floating note at its reset. A time between two tenors splits linearly
between them. A time before 3M counts at 3M, one past 30Y at 30Y. The
rows of every currency sum to the holdings' DV01, within rounding.

**The sign.** A DV01 is positive where the holding loses value as rates
rise, as a bond does. Fund filings follow no one sign convention, so
the report reads each filing as a long fund's: one whose DV01 sums
below zero is flipped whole. Its rows keep their signs relative to each
other. A fund whose ledger record or page states a negative duration,
an inverse or rate-hedged fund, reads its filing in that sign instead.
An inverse fund that states no duration reads as long. Silver keeps
what was filed; gold's `fund_risk_signed` reads it signed.

## 7. Quality

Each row says what its figures rest on, as tags separated by spaces. A
tag carries the share of the row's gross value it covers, as `tag=NN%`:
`stated=61%`. A share that rounds to less than one percent reads
`tag<1%`: `aged<1%`. A bare tag covers the whole row. One that covers
all but a sliver reads `tag=100%`. `implied` that covers the whole row
does not print: a computed yield is the norm. A `positions` or `cash`
row lists its holding's tags without shares.

| Tag | Meaning |
| --- | --- |
| `implied` | the yield is computed from the price |
| `stated` | a source states the rate |
| `indicated` | a dividend is the last payment at its frequency, or the filings' |
| `realized` | the figure comes from the transactions, where nothing else is known |
| `declared` | a ledger record states it |
| `aged` | a fund's composition is older than exposure allows (EXPOSURE.md §8) |
| `stale_price` | a fund's lines are priced on their filing's day, not the day |
| `floating` | a floating note, which implies no yield, or a fund whose income resets with the rates: its lines or its filing's float for half of it or more, or its name says it floats (§3) |
| `default` | a bond is graded in default, or a fund holds lines in default or arrears; they imply nothing |
| `thin` | the amount was under a tenth of its mean on more than half the year's days: no realized yield (§4) |
| `partial` | a figure covers part of the year: a position's realized yield covers the days it was held, fewer than the year's; or a payer twice a year shows one half's dividend, as the other half's rate is lost (§4) |
| `units` | a dividend per share may be in the units of its day: no whole factor explains how they changed since, or no account held it through a span since (§4) |
| `unknown` | no source and no record gives a figure |
| `mixed` | a total across currencies |

## 8. Config

The `yield` block of `wealthdb.cfg` holds what no source states
(DESIGN.md §5.1):

```json
"yield": {
  "curves": {
    "USD": { "3M": "fred:DGS3MO", "1Y": "fred:DGS1", "10Y": "fred:DGS10" },
    "EUR": { "3M": "curves:YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_3M", "1Y": "curves:YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_1Y", "10Y": "curves:YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_10Y" },
    "CHF": { "1Y": "curves:SNB1A.SNB.NSS.KZS.EID.J01M0", "2Y": "curves:SNB1A.SNB.NSS.KZS.EID.J02M0", "10Y": "curves:SNB1A.SNB.NSS.KZS.EID.J10M0" }
  },
  "withholding": { "US": { "default": 30, "taxable_personal": 15 }, "CH": { "default": 35 } },
  "rating_funds": { "isin:XX0000000004": "BBB", "isin:XX0000000005": "AAA-A" },
  "conventions": { "EUR": { "frequency": 1, "day_count": "ACT/ACT" } },
  "horizon_months": 12,
  "cycles": { "utilities": "sensitive" },
  "settlement_cycles": { "US": [{ "days": 3 }, { "from": "2017-09-05", "days": 2 }, { "from": "2024-05-28", "days": 1 }] }
}
```

- `curves` maps a currency to its public curve: each tenor (`3M`,
  `1Y`, `30Y`) to a rate series, as `<silver_source_id>:<series_id>`.
  The source must be configured. `fred` serves the US dollar's curve,
  and `curves` the euro's and the franc's. The curve gives the
  benchmark (§4) and the forward rates of the income projection.

  Each series' payload states its basis. Spot rates read as zero rates
  at their stated compounding. Par yields bootstrap with their stated
  coupons. A series that states no basis, or a Treasury one (`fred`'s
  `constant_maturity` and `discount`), reads on the Treasury's basis:
  semiannual par yields.
  [collectors/curves/README.md](../../collectors/curves/README.md#basis)
  gives the basis of each `curves` series.

  A curve whose tenors state two bases gives no curve, and so no
  benchmark. So does a basis the engine cannot read, and a curve on
  another basis than the Treasury's that cannot be built. A curve on
  the Treasury's basis that does not bootstrap keeps its benchmark, but
  gives no forward rates.
- `withholding` maps a situs country to the tax it withholds at source,
  in percent, per tax wrapper. The situs is the holding's legal situs
  (EXPOSURE.md §13), the wrapper its account's. A country's `default`
  stands for every wrapper it names no rate for. With the block above:

  | holding | account's wrapper | withheld from a dividend of 100 |
  |---|---|---|
  | a share of a US company | `taxable_personal` | 15 |
  | a share of a US company | `traditional_ira` | 30, the US `default` |

  A holding whose situs is Switzerland has 35 withheld in any account.
  A country the block does not name withholds nothing in the model, and
  so does a country with no `default` for a wrapper it does not name.
  Without the block, the forward views' `withheld` is blank, and the
  Income dashboard shows the tax the sources booked.
- `rating_funds` maps a fund sliced by credit rating to the bucket its
  lines carry (`BBB`), or a range of buckets, best first (`AAA-A`). It
  is keyed like `exposure.size_funds`, and feeds the credit view of
  `exposure` (EXPOSURE.md §7), which reads a range at its worse end.
  The yield report's `rating` is a holding's own: its terms' or a
  ledger record's.
- `conventions` maps a market to its coupon frequency and day count. A
  market is a currency, or a currency and a bond sector or `bill`
  (`USD:government`, `USD:bill`). An entry replaces the built-in one of
  its market. It fills a bond's frequency and day count where its terms
  state none. A floating bond takes only its day count.
- `horizon_months` is the income projection's default window: the
  months after the current one, 1 to 36, default 12
  ([INCOME.md §12](INCOME.md#12-the-forward-views)).
- `cycles` overrides the built-in cycle of one of the eleven sectors:
  `cyclical`, `defensive` or `sensitive`. It feeds the cycle view of
  `exposure` (EXPOSURE.md §7).
- `settlement_cycles` maps a market, an ISO 3166 country code, to its
  settlement cycles, oldest first. Each is a number of business days,
  1 or more, from its day on (`from`, `YYYY-MM-DD`). Only the first may
  leave its day out: it then holds from the first day on. The days
  ascend. A market the block names replaces its built-in cycles, and an
  empty list leaves it none. The cycle derives a dividend's record day
  from its ex day, and its ex day from its record day (§4).

In the built-in conventions, a `CHF` or `EUR` bond pays once a year on
30/360. A `USD` bond pays twice on 30/360, a `USD:government` bond
twice on ACT/ACT. A `USD:bill` pays no coupon and counts ACT/360.

In the built-in cycles, materials, consumer_cyclical, financials and
real_estate are cyclical. Consumer_staples, health_care and utilities
are defensive. Communication, energy, industrials and technology are
sensitive.

The built-in settlement cycles are those of §4's table.

The block is checked when the config loads. DESIGN.md §7.6 says how a
load stores it.

## 9. The ledger

The declarations ledger states the yield figures no source states. It
holds an account's `rate`, a holding's `rating` and a stock's
`dividend`. It also holds the `terms` of a bond, a note, a mortgage, or
a fund that files nowhere. [EXPOSURE.md §9](EXPOSURE.md#yield-records)
defines the four records.
A field a record states wins over the same field from any source, and
the figure reads `declared`.

The coverage view starts the ledger.
`wealthdb yield coverage -f ndjson` prints a skeleton record for each
holding a figure is missing on (§10):

```sh
wealthdb yield coverage -f ndjson > yield-skeletons.ndjson
```

Where the config names no ledger, `exposure.declarations` sets its
path (EXPOSURE.md §9).

## 10. Coverage

`wealthdb yield coverage` says what the figures rest on, in four
sections:

- `rests_on`: one line per tag (§7), with the value, the share of the
  gross value and the count of the holdings that carry it;
- `missing`: one line per reason below, with the same figures;
- `record_day`: where the dividends' record days come from (below);
- `holdings`: each holding a figure is missing on, by `silver_source`
  and `instrument`, the largest first within each reason.

The `missing` reasons, and the ledger record that fills each:

| Reason | Holdings | The record that fills it |
| --- | --- | --- |
| `no terms` | a bond, bill, floating note, note or liability that reads `unknown`, unless it is in default | `terms` |
| `no rate` | cash with a positive balance and no rate, on any account but a card | `rate`, naming the account and the currency |
| `no receiving rate` | a bond, bill, floating note, note or term deposit whose principal matures within the projection's 36 months, under the cash rule, into cash whose account states no rate | `rate`, naming the account and the currency |
| `no figures` | a bond fund or money-market fund that reads `unknown` | `terms` |
| `no dividend` | a stock, equity fund or multi-asset fund that reads `unknown` | `dividend` |
| `no rating` | a bond, bill or floating note without a rating | `rating` |

`wealthdb yield coverage -f ndjson` prints a skeleton of that record
for each holding the `holdings` section lists:

```json
{"silver_source_id":"broker-1","instrument":"bond-1","rating":{},"note":"QORVANT CORP NOTE 4.00000% 06/30/2031"}
{"silver_source_id":"bank-2","account":"current-1","rate":{"currency":"CHF"},"note":"Current"}
```

Its note is the holding's name, or the account's. A record filled in
from the skeleton goes into the ledger, and the next load takes it. A
skeleton loads only once it is filled in: the load refuses an empty
object, and a rate without its `pct`. A holding its source names by no
instrument takes no skeleton, but a rate names its account. An account
takes one rate per currency, so the first line that lacks it carries its
skeleton.

`no receiving rate` reads the income projection under its default
assumptions (§4): the holding keeps its own readings, but what its
principal earns after the maturity is unknown. The holding's line names
it, and its skeleton names the account its principal lands in.

**Record days.** `record_day` counts the dividends and distributions
that the holdings' readings rate (§4): those of the 24 months to the
day. Its `payments` column counts each payment once. `holdings`,
`value` and `share` are those of the holdings whose readings rate them.
One line per reason, where a payment has it:

| Reason | Payments |
| --- | --- |
| `stated` | the dividend calendar states the record day |
| `derived` | the record day derives from the calendar's ex day by the market's settlement cycle |
| `learned` | its issuer's learned gap gives the record day, or for a fund its pool's (§4): no distribution of the calendar is the payment's, or it gives no record day |
| `lag` | the market's lag gives the record day: no gap is learned either |
| `none` | no record day is known: the pay day's quantity rates |
| `dropped` | the record window drops the reading: the account's most over its least across it is above 1.1, or some of it is unknown |

So `stated`, `derived` and `learned` rise, and `lag`, `none` and
`dropped` fall, as the calendar covers more of the holdings. A dropped
payment also counts on the line of its record day's source.

## 11. The projection, the dashboard and MCP

The income projection is the forward half of `wealthdb income`: the
expected income month by month, per holding and per payment.
`income_12m` is one reading of it.
[INCOME.md §12](INCOME.md#12-the-forward-views) defines it, and the
withholding it shows.

The Metabase Yield dashboard (`wealthdb web`) charts these figures as
of a day of the last twelve months, beside the income today's holdings
are expected to pay. [web/DESIGN.md](../../web/DESIGN.md) §6 lists its
cards, its pickers and its privacy twin, and §8 the tables it reads.

The MCP server has a `yield` tool with the same views, an `as_of` day,
`converted`, and a `period` series between `from` and `to`. Its rows
equal the command's, cell for cell. A series' header names its window:
its first and last period ends.

## 12. What yield is not

- Not a dividend model. A stock has no yield to maturity and no
  duration.
- Not a forecast. The indicated dividend stands in for analyst
  estimates, and the public curves for futures-implied rates.
- Not a credit rating. A bucket comes from the ledger, the agency
  grades a source's record or a broker's description prints, or the
  config. It never comes from an agency's own data.
- Not a total return. Price changes are in `gains` and `returns`.
- Not a fixed income report. The aggregate views and `positions` cover
  every holding; `kind` picks out the bonds.
