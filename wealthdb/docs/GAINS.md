# Gains

What was gained or lost on what the household holds, realized and
unrealized, over a window. This is the read side of the cost basis gold
carries: the stamped book values on positions, the open lots and the
realized lots. [DESIGN.md §7.4](DESIGN.md) defines that data and its
per-source mapping; this file defines the figures the readers compute
from it.

Related design: DESIGN.md §4.16 (the `gains` command), §7.1 (accrued
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

For a period bucket, at any grain:

    realized          = Σ gain of the primary lots sold in the bucket
    unrealized_change = unrealized at the bucket's end
                        − unrealized at its start
    gain              = realized + unrealized_change

`gain` is the bucket's price gain on what is held. A sale moves gain
from unrealized to realized and leaves the total unchanged. A purchase
adds none. Income, fees and taxes are not in it; they are in `returns`,
whose `gain` column is the change in value net of external flows (§6).

## 3. Time

- **Boundaries.** A bucket's start is the holdings as of the second
  before it opens; its end is the holdings as of its last second. Each
  source contributes its latest snapshot at or before the instant, the
  rule of `wealthdb holdings`, so a boundary reconciles with `holdings`
  at that date.
- **A realized lot's date** is its disposal date (the trade date, which
  is what a 1099-B files under), else its settlement date, else the last
  day of its tax year. The last case is counted as `undated_lots`.
- **The window** opens no earlier than the first data gold holds, so an
  open start does not produce empty buckets.

## 4. Currency

Every amount converts at the rate of its own date. An unrealized gain
converts both legs at its snapshot's rate, so it carries no FX of its
own. A realized gain converts at the lot's date. A figure whose rate is
missing is left out of the sum and counted as `fx_missing`.

## 5. Grains and views

The four aggregate views are the holdings grains: `summary` (the whole
portfolio), `sources`, `portfolios` (a source's accounts outside any
portfolio form one row) and `accounts`. They are the same sums grouped
differently, so they reconcile bucket by bucket.

`positions` is one row per account and instrument over the window. A
position line keys on its instrument; a realized lot keys on its
instrument, else its instrument hint, else its description. A lot meets
its position where the two name the same instrument. A lot whose
instrument the account never held in any snapshot gets its own row and
is counted as `unmatched_lots`: the document and the holdings do not
share an identity. A position bought and sold inside the window is not
unmatched; it simply has no boundary value.

`realized` lists the lots, `lots` the open lots as of the window's end,
and `coverage` the accounts. An open lot's value is the one its source
states, else its position's value pro rata by quantity.

## 6. Quality

Every aggregate and positions row carries a `quality` column. Each flag
names one way the identity of §2 can fail:

| flag | meaning |
|---|---|
| `sells_without_documents=N` | sells with no primary lot of their tax year on their account or another account of the same portfolio: realized is understated, and gain with it |
| `lots_without_gain=N` | primary lots with neither a stated gain nor a cost basis |
| `undated_lots=N` | lots placed at the last day of their tax year |
| `unmatched_lots=N` | lots whose instrument the account never held (positions view) |
| `in_kind_moves=N` | securities moved in or out by a transfer or journal: a holding arrives or leaves with its whole unrealized gain, so the change counts gain from before the bucket; at a grain holding both ends of the move the two cancel |
| `corporate_actions=N` | mergers, splits and spin-offs: one holding becomes another with no realized lot |
| `paid_in_basis` | a private-market holding is in the row: its unrealized gain is its value less capital paid in, gross; cash paid back is a capital return in `cashflow`, never realized here |
| `onboarded_in_window=<source>` | the source's first snapshot falls inside the bucket, so its start value is zero by absence |
| `fx_missing=N` | figures left out for want of a rate |

A sale counts as documented on its portfolio as well as its account,
because a bank can book the sale on the portfolio's cash account and
print the lots against its custody account.

A cost basis describes every holding except cash held as a position, a
mortgage, and an FX forward, whose value is itself the gain. Those carry
no unrealized gain, even where the source states a book value (paying a
mortgage down is not a gain), and count toward no coverage figure:
`basis_coverage_pct` is the share of the rest, by value, that carries a
cost basis.

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
