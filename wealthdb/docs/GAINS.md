# Gains

What was gained or lost on what the household holds, realized and
unrealized, over a window. This is the read side of the cost basis gold
carries: the stamped cost basis of each position, the open lots and the
realized lots. [DESIGN.md §7.4](DESIGN.md) defines that data and its
per-source mapping; this file defines the figures the readers compute
from it.

Related design: DESIGN.md §4.15 (the `gains` command), §7.1 (accrued
income), §7.4 (cost basis), §10.13 (the report macros), §13.4 (the lot
engine), and migration 0116.

Every figure here is a sum or a difference of figures a source states.
Gold computes no cost basis of its own; where a source states none, the
figure is blank and the coverage says so.

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
  same thing. The third part says whether purchase fees are in. The
  readers never add a fee a source leaves out.
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

`gain_origin` says which: `stated`, `derived` or `unknown`. Only the
primary lots count (DESIGN.md §7.4): a sale stated by a 1099-B, its
correction and a year-end summary counts once.

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
`basis_changed`: the source began or stopped stating a basis inside the
bucket, and the whole gain since purchase is not this bucket's.

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

`realized` lists the lots, `lots` the open lots as of the window's end,
and `coverage` the accounts. An open lot's value is the one its source
states, else its position's value pro rata by quantity.

## 6. Quality

Every aggregate and positions row carries a `quality` column. Each flag
names one way the identity of §2 can fail.

| flag | meaning |
|---|---|
| `sells_without_documents=N` | N sells have no primary lot of their tax year, on their account or on another account of the same portfolio. Realized misses them, and gain with it. |
| `lots_without_gain=N` | N primary lots state no gain, and not both proceeds and a cost basis. |
| `undated_lots=N` | N lots count at the last day of their tax year. |
| `unmatched_lots=N` | N lots name an instrument the account never held (positions view). |
| `in_kind_moves=N` | N securities moved in or out by a transfer or journal. A holding arrives or leaves with its whole unrealized gain, so the change includes gain from before the bucket. At a grain that holds both ends of the move, the two cancel. |
| `corporate_actions=N` | N mergers, splits or spin-offs turned one holding into another with no realized lot. |
| `basis_changed=N` | N holdings began or stopped carrying a cost basis between the boundaries. Their change is left out (§2). |
| `accounts_unobserved=N` | N accounts have lines at one boundary only, while their source has a snapshot at the other. The snapshot left them out, or they closed without a closing row; their change counts the whole unrealized gain. |
| `paid_in_basis` | A private-market holding is in the row. Its unrealized gain is its value less the capital paid in, gross. Cash paid back is a capital return in `cashflow`, never realized here. |
| `onboarded_in_window=<source>` | The source's first snapshot falls inside the bucket, so its start value is zero by absence. |
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
least 99% covered by value and every sell documented), `no_basis` (it
holds positions, none with a cost basis), `no_realized` (sells no
document covers, positions covered) or `partial`.

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
- **Not a tax report.** The figures are the documents', summed. A tax
  return applies rules (netting, carryovers, jurisdictions) that no
  reader here applies.
- **Not a lot engine.** Sources that state no basis (§7.4's list) stay
  blank until the lot engine rebuilds one (DESIGN.md §13.4).
