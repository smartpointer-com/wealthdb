# Gains

What was gained or lost on what the household holds, realized and
unrealized, over a window. This is the read side of the cost basis gold
carries: the stamped cost basis of each position, the open lots and the
realized lots. [DESIGN.md §7.4](DESIGN.md) defines that data and its
per-source mapping; this file defines the figures the readers compute
from it.

Related design: DESIGN.md §4.15 (the `gains` command), §7.1 (accrued
income), §7.4 (cost basis), §10.13 (the report macros), docs/LOTS.md
(the lot engine), and migrations 0116 to 0120.

Every figure here is a sum or a difference of figures a source states,
or that the lot engine rebuilt from the trades where a source states
none (docs/LOTS.md). Where neither knows a figure, it is blank and the
coverage says so, unless the reader asks to count a missing cost basis
as 0 (§8).

---

## 1. Words

- **Cost basis** is what a statement calls it: the purchase price plus
  transaction costs, adjusted for later events (wash sales, returns of
  capital, splits) where the source adjusts. Gold stores it as
  `book_value`; the readers call it `cost_basis`.
- **The basis stamp** says which notion of cost basis a figure is, as
  one token `origin/method/fees`, for example `stated/lots/included`. A
  brokerage lot's tax basis, a fund's capital paid in and an exercised
  option's value at exercise are all cost bases, and they are not the
  same thing. A basis the lot engine rebuilt names its method:
  `rebuilt/fifo/included`. The third part says whether purchase fees
  are in. The readers never add a fee a source leaves out.
- **Clean value** is `market_value − accrued_interest`. Gold's market
  value includes accrued interest and declared dividends (DESIGN.md
  §7.1). That part is income, not gain, so every unrealized gain is
  measured on the clean value.
- **Where a cost basis applies.** It describes every holding except
  cash held as a position, a mortgage, and an FX forward, whose value is
  itself the gain. Those carry no unrealized gain, even where the source
  states a cost basis for them: paying a mortgage down is not a gain.
- **Column names.** A money column in the holding's own currency has a
  twin in the output currency named `<name>_outccy`. The twin prints as
  `<name>_<CCY>`, for example `cost_basis_CHF`. `value` is
  `market_value`'s twin, as on `holdings`. A figure only the output
  currency carries, such as a bucket's `realized`, keeps its own name
  and prints as `realized_<CCY>`.

## 2. Definitions

For a position with a cost basis:

    unrealized = clean value − cost basis          (position currency)

For a realized lot:

    gain = the gain the document states
           else proceeds − cost basis + wash sale disallowed
           else unknown (counted, never summed)

`gain_origin` says which: `stated`, `derived` or `unknown`, or
`assumed_zero` where a missing cost basis counts as 0 (§8). Only the
primary lots count (DESIGN.md §7.4): a sale stated by a 1099-B, its
correction and a year-end summary counts once. A sale no document
states has the lot engine's lots as its primary set (docs/LOTS.md
§5.3).

For a period bucket, per account and instrument, then summed to any
grain:

    realized          = Σ gain of the primary lots sold in the bucket
    unrealized_change = unrealized at the bucket's end
                        − unrealized at its start
    gain              = realized + unrealized_change

`gain` is the bucket's price gain on what is held. A sale moves gain
from unrealized to realized and leaves the total unchanged. A purchase
adds none. Income, fees and taxes are not in it; they are in `returns`
(§7).

The change is unknown, and left out, when one side holds the instrument
without a full cost basis. If the other side has one, the row says
`basis_changed`. Either the source began or stopped stating a basis
inside the bucket, or a rebuilt basis gained or lost a lot of unknown
cost. The whole gain since purchase is not this bucket's.

### Sold and held

A sale realizes the whole gain since its purchase, and the same amount
leaves the unrealized change. So in a bucket with a sale, `realized`
and `unrealized_change` move apart. The Gains dashboard splits the gain
by what prices did instead:

    released    = Σ over the lots sold in the bucket and held at its start:
                  quantity × start price − cost basis
    sold_gain   = realized − released
    held_change = unrealized_change + released
    gain        = sold_gain + held_change

- `sold_gain` is what the lots sold gained in the bucket: from its
  start, or from their purchase in it, to the sale.
- `held_change` is the change in unrealized gain with the sold lots left
  out.
- A lot was held at the start when it was bought before the bucket or
  states no purchase date.
- The start price is the clean value per unit of the instrument across
  the source's accounts at the bucket's start, converted at the start's
  rate. A document that names a security by its CUSIP takes the price
  of its ticker.
- Lots that sell more units than the source held at the start release
  in proportion.
- Nothing is released where the start is unknown: the source did not
  hold the instrument, or a change is left out (above).

The release sits on the lots' row. That can be another row than the
position's: a lot keyed by its CUSIP, or a wallet of a pooled
portfolio. Summed over the rows of a grain, the two splits add up to
the same gain.

The split depends on the bucket. Twelve monthly splits do not add up to
the year's split, but their gains add up to the year's gain.
`report_gains` carries both splits. The `gains` command prints realized
and unrealized change, which statements reconcile to.

## 3. Time

- **Boundaries.** A bucket's start is the holdings as of the second
  before it opens; its end is the holdings as of its last second. Each
  source contributes its latest snapshot at or before the instant, the
  rule of `wealthdb holdings`, so a boundary reconciles with `holdings`
  at that date. An account that has lines at one boundary but is
  missing from its source's snapshot at the other is flagged
  `accounts_unobserved`.
- **A realized lot's date** is its disposal date (the trade date, which
  is what a 1099-B files under), else its settlement date, else the last
  day of its tax year. The last case is counted as `undated_lots`, and
  the realized view marks the lot `undated`.
- **The window** opens no earlier than the first data gold holds, so an
  open start does not produce empty buckets.

## 4. Currency

Every amount converts at the rate of its own date.

- An unrealized gain converts its value and its cost basis at the same
  rate, its snapshot's.
- So `unrealized_change` in the output currency includes the change in
  the rate on a gain carried through the bucket. In the holding's own
  currency the gain may not have moved at all.
- A realized gain converts at the lot's date.
- A figure whose rate is missing is left out of the sum and counted as
  `fx_missing`.

## 5. Grains and views

The four aggregate views are the holdings grains: `summary` (the whole
portfolio), `sources`, `portfolios` (a source's accounts outside any
portfolio form one row) and `accounts`. All four, and `positions`, sum
the same rows: one per bucket, account and instrument. So they
reconcile bucket by bucket.

`positions` shows those rows for the whole window. Each input to a row
keys on an instrument:

- a position line on its instrument, else its position key;
- a realized lot on its instrument, else its instrument hint, else its
  description;
- a sell, transfer or corporate action on its instrument.

They meet where they name the same instrument. A row the lots or events
alone make shows their key as `position_key`. A lot whose instrument
the account never held in any snapshot is counted as `unmatched_lots`:
the document and the holdings do not share an identity. A position
bought and sold inside the window is not unmatched when any snapshot
holds it; it simply has no boundary value.

`realized` lists the lots, with what each realized in `disposal`: a
sale, or a fee, spend, tender, cash in lieu or expiry. `lots` lists the
open lots as of the window's end: a position's stated lots, else, on a
position the lot engine filled, the engine's. An open lot's value is
the one its source states, else its position's value pro rata by
quantity. `coverage` lists the accounts, and `check` the lot engine
against the stated lots (docs/LOTS.md §9).

The Metabase Gains dashboard reads the same rows, month by month. Its
figures over a window sum the months in it, so they equal the monthly
buckets summed. A figure at the window's end reads its last month.

## 6. Quality

Every aggregate and positions row carries a `quality` column. Each flag
names one way the identity of §2 can fail.

| flag | meaning |
|---|---|
| `sells_without_documents=N` | N sells have no primary lot of their tax year, on their account or on another account of the same portfolio, and none the lot engine rebuilt. Realized misses them, and gain with it. |
| `sells_rebuilt=N` | N sells have the lot engine's realized lots as their primary set: no document states them. |
| `lots_without_gain=N` | N primary lots state no gain, and not both proceeds and a cost basis. |
| `undated_lots=N` | N lots count at the last day of their tax year. |
| `unmatched_lots=N` | N lots name an instrument the account never held (positions view). |
| `in_kind_moves=N` | N securities moved in or out by a transfer or journal. A holding arrives or leaves with its whole unrealized gain, so the change includes gain from before the bucket. At a grain that holds both ends of the move, the two cancel. |
| `corporate_actions=N` | N mergers, splits or spin-offs turned one holding into another with no realized lot. |
| `basis_changed=N` | N holdings began or stopped carrying a cost basis between the boundaries. Their change is left out (§2). |
| `accounts_unobserved=N` | N accounts have lines at one boundary only, while their source has a snapshot at the other. The snapshot left them out, or they closed without a closing row; their change counts the whole unrealized gain. |
| `paid_in_basis` | A private-market holding is in the row. Its unrealized gain is its value less the capital paid in, gross. Cash paid back is a capital return in `cashflow`, never realized here. |
| `onboarded_in_window=<source>` | The source's first snapshot falls inside the bucket, so its start value is zero by absence. |
| `seed_lots=N` | The lot engine opened N lots of unknown cost: a snapshot held more than the trades explain, or a sale took more. |
| `implied_disposals=N` | N times a snapshot held less than the trades explain; the lots left with no proceeds. |
| `snapshot_blips=N` | N times a snapshot was off for less than 45 days: quantity left and came back, or showed before its trade. The lots stand as the trades left them. |
| `spliced` | A rebuilt basis holds a seed a source's stated lots resolved later. |
| `pooled` | A rebuilt basis is its portfolio's pool, shared by quantity among its wallets. |
| `dated_by_settlement` | Rebuilt realized lots date their sales by settlement, so a term near the one-year line can be off. |
| `wash_sales_not_applied=N` | N rebuilt losses fall within 30 days of a purchase of the same holding; no wash sale rule was applied. |
| `fee_unvalued=N` | N fees in a third currency had no rate and are not in the cost or the proceeds. |
| `basis_assumed_zero=N` | Under a missing cost basis read as 0, N positions and lots took 0 for a cost no one states. |
| `fx_missing=N` | N figures are left out for want of a rate. |

A sale counts as documented on its portfolio as well as its account,
because a bank can book the sale on the portfolio's cash account and
print the lots against its custody account.

`basis_coverage` (it prints as `basis_coverage_pct`) is the share, by
value, of the holdings a cost basis applies to (§1) that carry one. The
aggregate views leave a holding with no rate into the output currency
out of the share and count it in `fx_missing`. The `coverage` view
leaves the share blank instead, so its verdict cannot read `ok`.

The `coverage` view gives one verdict per account: `ok` (positions at
least 99% covered by value and every sell documented or rebuilt),
`no_basis` (it holds positions, none with a cost basis), `no_realized`
(sells neither a document nor the lot engine covers, positions covered)
or `partial`. It also counts the sells and positions the engine rebuilt,
the seeds it opened, and names the source's mode and the methods.

## 7. What gains is not

- **Not returns.** `returns` measures performance after income, fees,
  taxes and FX on every leg, over accounts and their cash. Its `gain`
  column is `end − start − net flow`. The two meet only where nothing
  but prices moved:

      returns gain = gains (price gain on positions)
                   + income − fees − taxes
                   + FX on every leg
                   + what positions do not see (cash interest, cards, loans)

- **Not the cash flow statement.** `cashflow` shows the cash a sale
  brought in under investing; `gains` shows what the sale gained. The
  `proceeds` column ties the two by hand.
- **Not a tax report.** The figures are the documents', summed, and the
  lot engine's where no document states a sale. A tax return applies
  rules (netting, carryovers, jurisdictions, wash sales) that no reader
  here applies.

## 8. A missing cost basis

A cost basis no source states and the lot engine could not rebuild is
missing. A reading says what it means:

- `ignore` (the default): a figure that needs it is blank, and the
  quality column counts it.
- `zero`: it counts as 0. An unrealized gain is the clean value less
  the cost the lots know; a realized gain is the proceeds less it. The
  coverage reads whole, and `basis_assumed_zero=N` says how much of it
  is assumption. This is the conservative reading for tax.

`quantity_without_basis` on `holdings positions` is the quantity held
with no known cost.

`--missing-basis ignore|zero` on `wealthdb gains` and `wealthdb
holdings positions` picks it, as do `missing_basis` on the MCP `gains`
and `holdings` tools and the Missing cost basis picker on the Gains
dashboard. The default is the config's `lots.missing_basis`, else
`ignore`.
