# `wealthdb returns` — method and rationale

The design decisions behind `wealthdb returns` (TWR / MWR): the
conventions, the non-obvious choices, and what is out of scope.
[DESIGN.md §10.9](DESIGN.md) is the summary; this file is the "why".

The engine is **source-agnostic**. `internal/returns/` holds the pure
math: period Modified-Dietz, TWR chaining, XIRR, and onboarding and
closure subsumption. It names no source and has no per-source branch.
Each source's knowledge enters through its `ReturnsPolicy` (see
"Pluggable per-source policy" below).

## Grain and scope

- **Account-grain is exact; coarse grains** (portfolios / sources /
  global) **are best-effort** — heuristic transfer netting plus synthetic
  onboarding for staggered inception. Returns are **not additive across
  grains**: `global == Σ accounts + hidden plumbing` is a *value* identity
  (guarded by a reconciliation test; cash-account, hidden-mode and
  returns_hide rows are in global but emit no accounts-grain row), never a
  return identity.
- **Mortgage / net-negative entities** are excluded from return rollups;
  per-entity twr/mwr = n/a + `nonpositive_base`, reported on a separate
  liability line.
- **Credit cards are returns-invisible** — see below.
- **The NAV-only source** (manual) reports value-growth TWR tagged
  `nav_only` + `nav_only_capital_call_risk`; MWR is `mwr_no_flows` on the
  source alone. Its only counted flows are the hand-asserted ledger legs
  (`transfer_in` / `transfer_out` from the equity-transfer ledger) for value
  that arrived from another tracked vehicle rather than a bank.
  A blended aggregate MWR is still computed and tagged
  `mwr_incomplete_flows`: disclose, do not refuse. carta and equityzen
  count the real boundary flows of their double-entry ledgers (see
  "Pluggable per-source policy"). A window of theirs with no observed
  flow keeps `nav_only_capital_call_risk` through the `CapitalCallRisk`
  knob.

## Credit cards are returns-invisible

An account of kind `card` (migration 0037) is a revolving-credit
liability. Its outstanding balance is carried as negative cash. It contributes
**nothing to any return series at any grain**: no value, no flow, no row.
A card is not an investment; its balance swings are spending, and running
them through TWR/MWR would report shopping as performance.

**The seam is `appendSeries`** (`internal/gold/returns.go`), the one place
both the single- and multi-currency loaders create an account record. A
card goes to the holdings side of the dataset there, which the returns
computation never reads, so it never enters the accounts map, and every
downstream stage follows from that absence:

- `attachOneFlow`'s nil-account gate drops every card flow — purchases,
  refunds, card payments, interest charges, rewards;
- a stored transfer pair binds only where both legs attached, so a card
  leg can never pair with a real transfer and net it away;
- snapshot-day loading, grouping and the materializer need no change of
  their own: they only ever walk the map, and `loadSnapshotDays` /
  `bindStoredLinks` already skip accounts they do not find.

**Deliberately not a `ReturnsPolicy` knob.** Policies are keyed by adapter
kind and default to a strict no-op, so a card arriving from a source with
no registered policy would leak straight into the aggregates. The rule is
engine-level and keys on the account kind itself.

**Contract of record:** with cards loaded, the returns global **does not
equal `report_global`**. The gap is the card balances. Same precedent
as mortgages and other liabilities, which the rollups already
drop; unlike a mortgage, a card is not even reported on the liability
line, because it produces no row at all. The holdings surfaces
(`report_global`, `wealthdb holdings`) still carry cards: net worth
includes the debt, returns do not measure it.

**The checking-side leg of a card payment stays a real external
withdrawal.** The money left the returns-visible system — it went to a
liability the returns engine cannot see — so it is capital out, not an
internal transfer. Nothing on the card side exists to net it against, and
that is the correct answer rather than an accident of the exclusion.

Guards: `TestReturnsCardEmitsNoRowsAndLeavesAggregatesIdentical` (zero
rows at every grain, aggregates byte-identical to a no-card baseline),
`TestMaterializeReturnsCardInvisible` (the same through the
multi-currency loader), `TestReturnsCardExcludedFromValueSpine` (the
`report_global` gap and the surviving payment leg).

## The gain column

Every row carries `gain = end value − start value − net flow`, the money
the percentages describe: capital added is no gain, so a deposit leaves
it unchanged. It is computed from the row's three figures and inherits
their quality flags. It is not the `wealthdb gains` figure, which is the
price gain on positions measured against a cost basis;
[GAINS.md](GAINS.md) §7 relates the two.

## The net worth bridge

`wealthdb returns bridge` says why net worth moved. `returns` leaves
mortgages and cards out of its values, so its start and end are not net
worth, and a mortgage repayment reads as money leaving. Its flows are
still the right base: a bridge built from the cash flow statement's
classes would miss money that reaches a plan without passing through
household cash.

Each bucket holds, to the cent:

    end_net_worth = start_net_worth + net_saved + onboarded + gain

| column | reads |
|---|---|
| `start_net_worth`, `end_net_worth` | what `holdings global` reads at the bucket's edges |
| `onboarded` | the synthetic onboarding and closure flows of the global run, less any tracking move: value that entered or left tracking, neither saved nor gained |
| `net_flow` | the global run's real flows, less any tracking move: its net flow less `onboarded` |
| `debt_repaid` | the fall in what is owed on mortgages and cards. It is negative when debt rose |
| `net_saved` | `net_flow + debt_repaid` |
| `gain` | what the identity leaves: `returns`' gain plus the FX move on debt |
| `bridge_gap` | net worth less `returns`' value less the mortgages and cards, at the worst edge of the bucket |

**One run, then months.** The bridge makes one since-inception run of the
global grain, with the default netting and inception, and cuts its flows
into months. A window selects months and a coarser bucket sums them, so a
month reads the same in every window. A flow pair that straddles a window
stays netted, where a run over that window alone would keep one leg.

**Cents first.** Every figure is rounded once, on a month, and a bucket
sums whole cents. `gain` is the residual of those cents, so the identity
holds exactly, a month's start is the month before's end, and months add
up to quarters and years. Beside `returns`' own gain it can differ by a
few cents of rounding.

**Debt in its own currency.** Each mortgage or card line is measured in
the currency it is held in (`report_debt_history`). Its
fall over a month is converted at that month's end rate
(`report_debt_fx`). An FX move on a debt held in another currency must
not read as saving, so it stays in `gain`.

How the common cases read:

- A card purchase raises what is owed, so `debt_repaid` falls: it reads as
  money spent. The bill's payment is a flow out and `debt_repaid` rises:
  the two cancel.
- A mortgage instalment leaves as a flow. Its principal comes back through
  `debt_repaid`. Its interest stays spent.
- A new loan: its cash arrives as a flow and `debt_repaid` is negative. The
  two cancel.
- An account that joins, or that zeroes on the spine's last day, moves
  `onboarded`. An account that empties with an exit leg to outside the
  household moves `net_flow` on the zeroing day: the closure books the
  leg's own money. An account that drops out with value books no flow in
  `returns`, so the fall stays in `gain` under `dropped_while_nonzero`.

**A move into or out of tracking is one movement.** Money can move from a
tracked account to one that tracking first sees that month. The netter
finds no partner for it, because it pairs only legs it can see. A bank can
also book such a move as a plain withdrawal. Read apart, it would be a
fall in `net_saved` beside a rise in `onboarded`. The mirror holds for an
account that closes into a tracked one.

So the month's synthetic flows net first, a closure against a debut, since
that is value going from one account to another. Then, where the month's
net real flow and net synthetic flow have opposite signs, the smaller size
leaves both columns. Their sum stays the run's net flow, so `gain` does not
move. The month is tagged `tracking_netted`.

The measure is the month's net real flow, not its unmatched transfer legs,
because a withdrawal is no transfer leg. Its cost: any net outflow beside a
debut reads this way, so a month's spending can absorb a small debut. A
debut with no net outflow beside it stays in `onboarded`, as when tracking
first starts. A flow in another month than the debut stays in `net_flow`.

The bridge's own quality tags join `returns`' tags for the bucket:

- `debt_first_seen`: a debt appeared with no inflow of its size in the
  month. `net_saved` falls once by what was already owed.
- `debt_ended`: a debt left the history while still owed. `debt_repaid`
  rises with no payment beside it.
- `debt_unpriced`: a debt has no rate to the output currency. Its fall is
  left to `gain`.
- `bridge_gap`: `bridge_gap` is not zero, so `gain` holds that difference.
  Returns leaving an asset out, as `returns_exclude` does, opens one. So
  does a day with no rate for a holding.
- `tracking_netted`: a month's net flow and onboarding had opposite signs,
  as above.

Limits:

- The whole household only. Returns do not add across grains.
- Whole months only, because the rows are monthly. FROM is a month's first
  day and TO a month's last, or today or later.
- A sale whose cash lands after month-end reads as a loss, then a gain.
- `gain` is a residual, so it is only as good as the flows `returns`
  identifies.

`web refresh` writes the monthly rows to `report_bridge` in every
reporting currency, from the datasets the returns tables use.
Guards: `TestBridgeReadsTheCommonCases`,
`TestBridgeIdentityAndSumsInEveryCurrency`,
`TestBridgeGainIsReturnsGainPlusFXOnDebt`,
`TestMaterializeBridgeIsTheCLIsRows`.

## Private markets

`wealthdb returns private` says what each private holding took, what it
gave back and what it is marked at. It reads gold alone. `-d DATE` reads
it as of a day: the legs up to the end of that day, and the NAV from the
last snapshot on or before it.

**Which holdings it lists.** A private holding is an instrument in one
account whose asset class is `private_equity` or `private_debt`, in any
vehicle but a listed fund (`etf`). The rule holds for every source and
every way the holding was funded:

- a fund called with `contribution` lines, or bought with `buy` lines;
- an SPV, a private company's stock, a convertible note or a loan;
- a holding that only a source's positions state, with no cash leg.

An instrument a `contribution` line names is a private holding whatever
its class, so a fund of another class that calls capital is listed too.

The class is the instrument's row's. Where a source writes no instrument
row, it is the holding's latest line's, else its latest leg's.

**One holding, one row.** A holding is one instrument in one portfolio,
or in one account outside any portfolio. The instrument is known by its
ISIN where gold states one, else by its id. A bank that keeps a fund on a
custody account and pays its calls from a cash account of the same
portfolio therefore has one row for it. The row names the account that
holds the latest line, else the account of the first leg. The same fund
in two portfolios is two holdings.

A move of the holding between two accounts of its portfolio is no exit.
A `transfer_out` counts for nothing where a `transfer_in` of the same
instrument into another account of the portfolio answers it on its day,
of the same quantity where both rows state one.

A holding with no money in, out or marked is no investment, and the
view leaves it out: a call refunded whole, a call of no amount, or a
line with no cost and no value. The test reads the legs and the lines in
their own currency, so every output currency lists the same holdings.

One row per holding, then the total:

| column | reads |
|---|---|
| `asset_class`, `vehicle` | the instrument's class and vehicle |
| `status` | `held` where nothing came back; `partially_exited` where something came back and the NAV is not 0; `exited` where the NAV is 0 after something came back, or once the holding has left the holdings |
| `first_call` | the day of the first call; with none, of the first leg, else of the first line |
| `paid_in` | the `contribution`, `buy` and `fee` lines on the holding, net of refunds |
| `commitment` | what the holder committed, as the holding's capital account states it (below); blank where none states one |
| `unfunded` | what the commitment still holds open: the remainder the account states, else the commitment less what it says was called (else paid in), never below 0. Where it says neither, the commitment less `paid_in`, read as 0 below 1% of the commitment, and blank with no `paid_in`. 0 once the holding has exited |
| `distributed` | the `distribution` and `sell` lines, a `private_debt` holding's `interest`, `dividend` and `coupon` lines (the cash a loan or a note returns), and the exits in kind: a `transfer_out` of the holding, or one whose `holding` names it, at the value it states |
| `nav` | the holding's lines at the latest snapshot. It is 0 once the holding has left the holdings, and blank where it never had a value |
| `marked_at` | the oldest mark of those lines, `coalesce(valued_at, snapshot_at)`; for a holding that has left, the mark of its last line |
| `dpi` | `distributed / paid_in` |
| `tvpi` | `(distributed + nav) / paid_in` |
| `irr_pct` | the money-weighted return since the first call, a yearly rate |
| `stated_tvpi`, `stated_dpi` | the capital account's own multiples; where it states none, the ratios of its stated figures over what it says was paid in, else called |
| `stated_as_of` | the day of the capital account |

**A holding with no call.** A source that states positions only has no
cash leg for what was paid. Such a holding reads its stated cost: each
line it holds at the day gives one call, its book value on its
acquisition date. Its lines in each account count, whenever each
account's snapshot is. A holding that has left reads the lines of its
last snapshot. The IRR reads those calls as any other, and the row says
`paid_in_from_cost`. Where a line states no cost or no acquisition date,
the holding has no `paid_in`, `dpi`, `tvpi` or `irr_pct`, and it says
`no_cost`. The rule reads each day for itself: a day before a holding's
first call reads its stated cost.

An SPV that pays out cash books a `sell`, as AngelList does, so
`distributed` counts a sale as a distribution. Without it an SPV that
exited for cash would read a TVPI of 0.

**Exits in kind.** An SPV that pays out in listed shares is a pair of
equity-transfer ledger rows (DESIGN.md §13.9). Both name the shares, not
the SPV. The out-row's `holding` column names the SPV, by the id
`-C +holding_id` prints. The view then counts the exit as the SPV's
distribution:

- at the value the row states, on the row's day;
- in `dpi`, `tvpi` and the IRR's flows.

The exit ends the SPV's figures. A later gain on the shares belongs to
the broker's position.

**Capital accounts.** A capital account is what a source states for a
holding on one day (gold's `capital_accounts`):

- Carta: a fund's partner metrics, with its commitment, called capital,
  capital contributed, distributions and NAV;
- AngelList: a deal's row of the positions table, with its commitment,
  contributed capital, realized and unrealized value and TVPI, at the
  data date of the latest dump that listed it. The commitment is the
  row's stated commitment, else its investment amount. A fund's calls
  draw on it over time. An SPV pays it at once;
- UBS: a fund's quarterly report, with its net commitment, capital
  called and valuation.

The view reads each holding's latest account on or before the day it
reads. The account names its holding by its instrument, and by its
account where the source states one. Each account of a source keeps its
own latest account of an instrument. A UBS report names the fund by its
ISIN alone, so it takes the one holding of that ISIN in the source; one
held in two portfolios takes none. A day before a holding's first
account reads no commitment: AngelList's dump keeps no history, so its
accounts read from the latest dump's day on.

An account can state a commitment but no remainder, call or payment,
as an AngelList node can. It gives no unfunded of its own, so the view
takes the holding's own `paid_in`: the commitment less it. Fees and
rounding part a stated amount from a computed one, so a remainder below
1% of the commitment reads 0. So an SPV paid in whole reads 0, not a
sliver. With no `paid_in` the unfunded is blank.

An exited holding reads `unfunded` 0: a holding sold or wound up whole
owes no further calls. Its `commitment` still shows.

The stated figures stand beside the computed ones. A source counts its
own fees, calls and marks, so they can differ. Neither replaces the
other. A stock or a note has no commitment, and that is no gap.

**One definition.** The macro `report_private` computes every figure but
the IRR, from `private_legs`. The IRR is the engine's own solver
(`solveIRR`, the money-weighted code of every grain): the legs as flows,
nothing held before the first call, and the NAV received on `end_date`,
the later of the NAV's snapshot and the last leg. So
`irr_pct` carries the engine's `mwr_*` tags where it has no answer.

**Historic FX.** Each leg converts at its own day's rate, the NAV at
its snapshot's, and the commitment and unfunded at the capital
account's. The computed multiples therefore move with the output
currency. The stated ones read in the account's own currency.

**The total.** Each holding's money rounds once, to the cent, and the
total adds the cents, so it is the sum of the rows as they print. `dpi`
and `tvpi` pool the holdings: they are the ratios of the sums, not an
average of the rows' ratios, and both mean what a fund's own DPI and
TVPI mean. The total's `marked_at` is the oldest mark. The total never
goes blank because one holding lacks a figure:

- its money adds what the holdings state, and `no_mark=N` and
  `no_cost=N` count the holdings it leaves out;
- `dpi` pools the holdings with a `paid_in`, and `tvpi` those with a
  `paid_in` and a `nav`;
- `irr_pct` pools the legs and NAVs of the holdings that have their own
  IRR's inputs: a `paid_in`, a `nav` and a rate for every leg.

The total adds the `commitment` and `unfunded` of the holdings that
state one. Its `stated_tvpi`, `stated_dpi` and `stated_as_of` are blank.

The total is over every holding: a filter keeps or drops it as a row,
never recomputes it.

The total also counts every exit in kind out of a listed holding's
portfolio, or its account outside one, that no holding claims: an
equity-transfer ledger out-row with no `holding`, of an instrument that
no `buy` or `transfer_in` there names. Shares the portfolio bought or
took in are a direct holding, and cash is no exit in kind, so neither
counts. A source's own `transfer_out` that no holding claims is no
private distribution either: a bank delivers listed shares out of a
portfolio that also holds a fund. Such an exit is in the total's
`distributed`, `dpi`, `tvpi` and `irr_pct`, and the total says
`unattributed_exit=N`. Then the total is more than the sum of the rows.

**No mark, no NAV.** Every row has a mark but one: a holding that never
had a value. Its `nav`, `tvpi` and `irr_pct` are blank, never zero, and
it says `no_mark`. A zero that was never anything else is no value
either: a source may carry a fund at 0 until it values it. So a holding
whose lines never stated a value but 0 up to the day, and from which
nothing came back, has no mark. One marked above 0 before and at 0 now
is written off and keeps its 0.

A holding that had a value and has left reads a NAV of 0, a real one:
gold holds it no more. Its multiples and IRR are final, and its
`marked_at` is its last line's mark. A holding that has left is
`exited`, valued or not, as is a holding still listed at a NAV of 0
after something came back. A holding written off with nothing back is
still `held`. A holding that has left with no money in or out is no
investment and is not listed, whatever its lines were marked at.

The IRR stands wherever the solver finds one. A holding or a total that
has paid back more than it took and still holds a value has one, and
`mwr_nonunique` says where its flows change sign more than once.

Quality tags:

- `no_mark`: the holding never had a value other than 0, and nothing
  came back, so `nav`, `tvpi` and `irr_pct` are blank.
- `no_cost`: the holding has no call and states no cost, so
  `paid_in`, `dpi`, `tvpi` and `irr_pct` are blank.
- `paid_in_from_cost`: `paid_in` is the holding's stated cost on its
  acquisition date, not a cash leg.
- `exited`: the status is `exited`, so `nav` is 0. Once the holding has
  left the holdings, `marked_at` is the mark of its last line.
- `at_cost`: the source carries the NAV at cost, before any valuation
  of its own, so `tvpi` reads 1 and `irr_pct` 0 by construction. AngelList
  states such a line as `valuation_basis: cost`, Carta's fund carry as
  `called_capital`. A mark plus what moved in since is a mark.
- `call_after_mark`: a call came after the mark. The NAV leaves it out
  until the next mark, so `tvpi` reads low.
- `distribution_after_mark`: a distribution or an exit came after the
  mark. A NAV not yet marked down for it reads `tvpi` high.
- `exit_unvalued=N`: N exits in kind state no value. `distributed` misses
  them.
- `unattributed_exit=N`, on the total only: N exits in kind name no
  holding. The total counts them, and no row does. To give one to its
  holding, write the holding's id in the ledger row's `holding` column.
- `unmatched_commitment=N`, on the total only: N capital accounts state
  a commitment of a private holding that no row takes. The holding is
  not listed at the day, as before its first call, or the account would
  name two holdings. The total leaves them out.
- `unpriced=N`: N legs, lines or commitments have no rate to the output
  currency. The sums leave them out, and a leg with none leaves
  `irr_pct` blank. A zero needs no rate.

On the total each tag counts the holdings it holds on. Its
`exit_unvalued` and `unpriced` also count the unattributed exits that
state no value or have no rate.

Limits:

- Marks are old by nature, so `marked_at` always shows.
- A commitment reads only where a source states one. AngelList states
  one for each deal.
- AngelList does not state a valuation basis for every deal. A deal it
  values at cost without saying so reads `tvpi` 1 with no `at_cost`, and
  its `stated_tvpi` reads 1 too.
- No source states an IRR per holding. AngelList's multiples for the
  whole account are not shown.
- The view carries no volatility or other risk figure. Marks are too old
  and too sparse for one.

`web refresh` writes the view's rows to `report_private_markets`: in
every reporting currency, at every quarter end since the total's
`first_call` and at the end of the refresh's day. Each row's figures are
`-d`'s at its day. The rows name a holding, its account, class and
vehicle as at the refresh, as the holdings group then. The Private Markets dashboard reads
them through `web_private`, the IRR with them. The macros take a list of
instants, so one pass per currency computes every quarter end. The
returns engine solves each IRR once per distinct set of legs, NAV and
days, so an exited holding is solved once for all its later quarters.
The macro `report_private_flows_multi` adds the legs the total counts
per holding and quarter, in every reporting currency, and
`serving_private_flows` holds it for the dashboard's cash flows. Guards:
`TestPrivateMarkets`, `TestPrivateMarketsListing`,
`TestPrivateMarketsStatus`, `TestPrivateMarketsAsOf`,
`TestPrivateMarketsHistoricFX`, `TestPrivateMarketsTableIsTheCLIsRows`,
`TestPrivateMaterialStartsAtTheFirstLine`,
`TestPrivateMarketsPortfolioHolding`, `TestPrivateMarketsPortfolioMoves`,
`TestPrivateMarketsNeverValued`, `TestPrivateMarketsDebtIncome`,
`TestPrivateIRRPastPaybackPooled`, `TestPrivateFlowsAreTheTotalsLegs`,
`TestPrivateMarketsCapitalAccounts`, `TestPrivateView`,
`TestPrivateViewAsOf`.

## The math (`internal/returns`)

- **All returns math is float64.** Money is exact (DECIMAL) up to the
  gold→returns boundary and floated only here; XIRR is inherently
  iterative and Modified-Dietz an approximation. No precision claim beyond
  ~1e-7 on rates.
- **No figure follows the order of its inputs.** Every sum over flows is
  exact and rounded once (`returns.Sum`), and MWR reads a day's flows as
  one cash flow. So a row reads the same whatever order a day's flows
  arrive in, its `mwr_nonunique` tag included. Guards:
  `TestSumIsExactInAnyOrder`,
  `TestReturnsDoNotFollowTheOrderOfADaysFlows`.
- **Snapshot-aligned headline.** The since-inception cumulative TWR chains
  Modified-Dietz over the entity's **actual valuation (snapshot) days**
  (`canonicalChainBounds`), not a fixed daily/monthly calendar grid
  (`--period` still controls the per-bucket display rows). *Why:* a fixed
  calendar grid detonates the geometric chain for sparse-snapshot,
  flow-bearing sources. When a run of deposits lands in a gap with no
  snapshot, the deposit's calendar bucket sees a flow with no value move
  (Dietz ≈ −F/base, a sub-(−100%) sub-period) while the value jump shows
  up at the next snapshot a bucket later — chaining the poisoned factor
  drives the since-inception TWR orders of magnitude below −100%. Breaking
  at real valuation days keeps each flow in the same sub-period as the
  value change it causes (textbook TWR at valuation dates). A carried tail
  past the last snapshot is excluded; a window with no interior valuation
  falls back to a single `[winFrom, winTo]` bucket. For dense
  (daily-snapshot) sources this is daily bucketing. Guard:
  `TestRunReturnsSparseSnapshotNoChainCollapse`.
- **Between valuations the chain rolls an account forward.** A coarse
  grain's valuation days are the union of its accounts', so a boundary can
  fall between a flow into a sparsely valued account and the valuation that
  shows it. A carried value would not show the flow yet: one sub-period
  would hold the flow against a flat value and a later one the value change
  against no flow. The chain would then miss most of the flow's effect.
  So at each boundary the chain adds what each account
  moved since it last valued (`unvaluedAt`, `Flow.ValuedOn`): its flows, and
  the legs of any pair netting cancelled, flows keeping their days. The
  window's opening value counts the same way.
- **The valuation has to confirm it.** Money can leave an account by a path
  no flow records, such as a purchase, and a flow can be dated after the
  valuation that shows it. So a gap between two valuations rolls forward
  only if the value change matches what moved, to within half of that, and
  the moves never take the account below zero (`confirms`). Otherwise the
  gap keeps its carried values, the plain reading. A gap that nets to
  nothing says nothing either way, so it keeps them too, as does a gap into
  a closure the synthetic flow books. This is an estimate: an unrelated
  value change of the same size passes the check. On an account's own
  valuation days a roll adds nothing, so a grain whose accounts value
  together chains its plain carried values. The accounts grain never rolls.
  Guard: `TestChainRollsAnAccountForward`.
- **No loss beyond everything while the value stays at zero or above.**
  The chain compounds the entity's assets summed. A value that never
  reads below zero cannot lose more than it held. A sub-period below
  -100% while that value stays at zero or above therefore says the flows
  and values disagree, as when money paid in has no value yet, or that the
  sub-period is too coarse for the approximation. Chained, that one factor
  takes the whole window below -100%. So the window's TWR is n/a, tagged
  `loss_beyond_total`. Where the entity's value reads below zero on some
  day of the sub-period, as when an overdraft or a margin loan outweighs
  the rest, the arithmetic stands. A loan account of the grain is no part
  of the value the chain compounds, so it does not count. The tag changes
  no other figure: MWR, the values, the flows and the per-period rows read
  as before. Guard: `TestLossBeyondTotal`.
- **MWR reads the values the chain reads.** Its opening and closing values
  hold what the window's edges have rolled in, so a window that ends with
  a deposit not yet shown has an MWR beside its TWR, not behind it.
  `start`, `end` and `net_flow` stay the carried values. They equal
  holdings, and `net_flow` is what the bridge reads. `gain` reads them
  too, so a row's gain can differ from what its MWR implies by the money
  still unshown. Such a row is tagged `rolled_forward`. The per-period
  rows read the carried values too. Guard: `TestMWRReadsTheChainsValues`.
- **Onboarding books the debut value.** A staggered constituent's real
  pre-debut flows are dropped first (their fate in the pre-debut region,
  `fateOf`), then synthetic onboarding books the first-snapshot value,
  less the pre-debut legs netting cancelled. Under per-entity-once it
  books the group's step (`groupOnboardStep`), which offsets the same
  legs and is floored at zero. A per-constituent amount reduced by such
  legs can be negative ("Staggered-inception subsumption").
- **XIRR:** Newton-Raphson (analytic derivative) from a 10% guess, with a
  bisection fallback scanning `[−0.9999, 100]` at 0.01 resolution for the
  first sign-change bracket. The rate is floored at −0.9999 (never worse
  than ~−100%). `ErrNoFlows` / `ErrNoSignChange` / `ErrNoConverge` map to
  `mwr_*` quality flags; a no-external-flow window is `mwr_no_flows` and
  never calls XIRR (avoids the `[−V0, +V1]` holding-period collapse
  masquerading as an IRR).
- **Sign convention.** `value_outccy` already carries the canonical sign
  (`canonical.ApplyCanonicalSign`, pinned by test), so Dietz
  `F_i = +value_outccy` and XIRR `cf = −value_outccy`, with no per-kind
  exception.

## Value spine and flows

The inputs come from the existing report macros. Returns adds no input
macro of its own:

- **Value spine:** `report_accounts_history(p_ccy)` is the per-account
  carry-forward series; its ASOF inner join omits pre-inception days, so a
  boundary before an account's first snapshot reads NULL ("not yet alive",
  not 0). Every grain is driven off the per-account spine (aggregated in
  Go), which is what synthetic onboarding needs.
  - The macro resolves the active snapshot **per account**.
    An account a partial run did not cover keeps its own last
    observation, so it stays in the spine for that day. The spine therefore
    has no holes where a source writes different accounts under different
    snapshots. A deposit run, a card run and a statement backfill each
    carry their own `snapshot_at`, so this case is common. An account that
    vanishes for a snapshot or two and then reappears is carried across the
    gap too, so the macro and the Go carry agree there.
  - A **dropped** account's series ends where gold ends its last
    observation, by the re-cover or the carry horizon
    ([DESIGN.md](DESIGN.md) §10.7). It reads **0 after its last emitted
    row**, not its last carried value: post-disappearance absence ≠
    pre-inception NULL. `valueAt` reads 0 past the last row, and an
    account that ended while still holding value raises
    `dropped_while_nonzero`. So `global == Σ accounts` reconciles past
    the ending.
  - A **closure** is an explicit zeroing row (`silver.ClosureMarkerBatch`),
    which the spine carries as a zero immediately — the supported way to end
    an account's value on the day it really ended. A cash account written down
    to 0 is one: the history keeps zero rows, so the series continues at 0
    rather than ending at the last non-zero balance, and the engine reads a
    closure instead of a feed drop.
  - An account whose FIRST observation is a zero starts its series on that
    day rather than at its first non-zero balance, which moves its inception
    day earlier, and an account observed only at zero enters the spine with an
    all-zero series instead of being absent from it. That is the intended
    reading: the account existed, and was empty.
- **Flows:** `report_transactions(from, to, p_ccy)` converts `net_amount`
  to the output currency at `occurred_at`; adapter kind and portfolio come
  from `silver_sources` + the history rows.

## Netting and closure (coarse grains)

- **Netting** runs only over transfer-like kinds (`transfer_in/out`,
  `journal`), never deposit/withdrawal: a deposit+withdrawal of equal size
  can be a genuine pair of external movements, so netting them would cancel
  real external capital. An internal move booked as deposit/withdrawal
  surfaces as two external flows, not silently cancelled. A leg pairs by
  what it moved:
  - **In kind.** A leg whose transaction names an instrument moved a
    holding, unless the instrument is money: class `cash` outside a fund,
    such as a wallet's balance in a fiat currency. It pairs by holding
    and quantity, never by value: one coin out, one coin in. The two legs
    move one holding in opposite directions as one move (`oneMove`). What one move is depends on the
    holding's asset class and vehicle, each the row's own where it states
    one, else its instrument's:
    - **A coin** (asset class `crypto`, vehicle `physical`) pairs by the
      coin rule, the rule the transfer matcher applies (SPENDING.md §3,
      "The matcher"). The receipt lies within 2 days of the withdrawal,
      in either order. It is at most 0.1% more than the coin sent. Or it
      is short by a fee of at most the larger of 0.1% of the coin sent
      and 100 USD's worth at the coin's rate that day, and less than a
      quarter of it. The rate is the latest one at most 7 days old. A
      coin with no such rate may lose 1%. So a network fee taken in kind
      still pairs, and its value reads as a loss.
    - **Everything else:** a security (a share, an option contract, a unit
      of a fund, a bond) never arrives short. Its legs pair at most 7 days
      apart (`lots.PairWindow`), on the exact quantity only, up to float
      noise (1e-9 of the larger). 100 shares out and 99 in do not pair.
      This includes a share of a fund or trust that holds coins (class
      `crypto`, vehicle `etf` or `fund`), and a class or vehicle gold does
      not state.

    The value netter runs first, over every leg, but pairs two legs only
    where both are cash or both moved one holding as one move (a
    security on the exact quantity, a coin by the coin rule, its days
    included).
    The legs that moved a holding and are left then pair by holding and
    quantity (`pairInKind`):
    - **Pairing.** Each leg, in day order, takes the open leg of the
      smallest shortfall (for a security, the closest quantity), the
      latest on a tie. One leg pairs with one leg:
      a move split over two receipts pairs at most one of them. A leg
      that states no quantity pairs only through a stored pair. Whatever
      the price did between the two days is return.
    - **Identity.** Two legs move one holding where any identity of
      their instruments agrees: the same ISIN, the same CUSIP, or the
      same instrument id among the sources of one kind
      (`holdingUnion`). So shares moved between two banks pair by ISIN,
      and a leg whose instrument states an ISIN still pairs with one
      whose instrument states only the CUSIP, or only the id, both
      share.
    - **Funding a debut.** A leg that funds a debut funds it by quantity
      where the first valuation holds what the legs moved, as one move
      would deliver it (`quantitiesAgree`: the coin rule's quantities for
      a coin, the exact quantity for a security). The first value then
      stands for them at the debut's prices, by the holding's share of
      that value (`movedLegs.fundedAt`). So the price move up to the debut
      is return, and a shortfall, a fee in kind, is a loss. Where a zero
      first value is no valuation, the debut is the first nonzero one
      ("Staggered-inception subsumption"), and its valuation is the one
      read. Each holding's
      part of the first value is read only where the first valuation
      holds positions in one currency and no cash. Elsewhere, and where
      the quantities do not agree, a leg funds its debut by its value. A
      first value below that value then books the difference as capital
      that left: nothing says the same holding arrived.
  - **Cash.** A leg that names no instrument, or names money, pairs by
    value: ε = max(1.00 outCcy, 0.5% of the larger leg); window ±3
    calendar days; FX-normalized via `value_outccy`. It is deterministic:
    `netMatch` sorts stably by `(|amount|, day, id)`.
- **Stored transfer pairs** (opt-in, DESIGN.md §5.8) are the one
  sanctioned crossing of that deposit/withdrawal line. A pair the load
  stored binds in every entity that holds both legs in the window, at any
  day gap and any value gap: the matcher's pairs and the equity-transfer
  ledger's `in_kind` pairs alike. Where both legs are live the pair drops.
  Where a leg falls on or before its account's debut, or in a zero tail,
  the pair settles as a netted pair: the leg funds the debut, or the
  closure books it. Such an entity is tagged `cross_source_netted=N`. The
  stored evidence is exactly what the within-entity heuristic lacks. Their
  legs are withheld from the netter's candidates, and `--netting off`
  disables them along with the netter. Off ⇒ byte-identical. Where both
  legs moved one holding in quantities that are not one move's (a
  security at all, a coin beyond the coin rule's quantities), the pair
  binds the quantity both legs hold. The larger leg's rest, at its share
  of the leg's value, stays a transfer nothing pairs: a flow, counted in
  `unmatched_transfers`. A coin within the coin rule pairs whole: the
  shortfall is the network's fee.
- **Money in transit.** Between a pair's two legs the money is in neither
  account, and it is still the household's. For a pair the value netter's
  rule would not pair (a stored pair whose legs are days or a price move
  apart, or a leg on a debut that is a deposit or withdrawal, or legs paired
  by holding and quantity alone), the chain carries the money
  (`movedLegs.rolled`). The carry runs from the day the chain stops
  counting it in the sending account until the receiving account's value
  shows it: the valuation a leg funds (below), else the leg's valuation.
  It runs whether or not either valuation confirms the legs, so every
  funding leg of a debut is carried when the first value appears. Where the
  sending account would stop counting the money only on or after the day
  the receiving one shows it, there is no transit and nothing is carried.
  Such a pair leaves TWR and MWR as if the money never left: cash legs of
  one amount into a debut have no net effect. A pair the value netter's
  rule pairs, and a stored pair whose both legs are live, keep their own
  reading.
- **Explicit closure** is detected only when an account's value series ends
  in a ~0 carry tail (`|v| < valueTol`) — mere staleness never triggers it.
  The per-source `ClosureScope` knob decides how the exit is accounted:
  - `ClosureSubsumeDrains` (default): flows dated inside the flat zero-carry
    tail show no ΔV, so they are subsumed (the zero-tail region). The
    closure flow books them instead, on the zeroing day (below).
    Real drains before the zeroing snapshot stay flows. Safe when the
    ledger may be missing or mis-dated around a closure.
  - `ClosureLedgerExact` (carta, equityzen, manual): zero-tail flows are
    real dated exit legs. Those ledgers put the proceeds on the zeroing day
    itself, so they are KEPT and no closure synthetic is booked. The
    realized-vs-last-mark delta shows as return. A late escrow release after
    the zeroing likewise books as gain. A flow-less zeroing reads as a full
    loss under either scope, tagged via `CapitalCallRisk`.

  The **closure flow** is the mirror of onboarding. It is dated on the
  zeroing day, the first valuation that shows the account empty, and the
  window must hold that day.

  - **Inside a longer zero tail** it books what the tail's legs moved out,
    once netting has run (the exit fate). That is the legs' own money
    on another day, so it is a real flow, not a synthetic one, and the
    bridge reads it as money that left. An exit leg dated days after the
    zeroing therefore leaves on the day the value shows the money gone. A
    source or portfolio whose accounts all empty out ends its chain on its
    return up to the exit, not on a fall to zero. A leg that netting
    cancelled against another member is an inside move, so a grain that
    holds both books nothing for it. A window that ends after the zeroing
    but before the exit leg still books the exit: the account's own tail
    legs past the window's end count too, unnetted, as the roll reads
    flows past a window's end.
  - **Money passed through an emptied account** after its zeroing, from
    a member to outside, was never in the zeroing's value. It leaves on
    the day it came in, and the zeroing day books only the rest.
  - **With no exit leg** it books nothing. The zeroing reads as a loss, as
    a write-off should.
  - **On the spine's final day** no exit leg has arrived yet. The closure
    books the carried value, less what already left since the last
    valuation: a real flow, a cross-matched leg, or a leg netting cancelled
    against money that stays inside. A drain then counts once. Only this
    final-day flow is synthetic, so only it moves the bridge's
    `onboarded`.

  Guards: `TestClosedEntityEndsOnItsReturn`,
  `TestFinalDayClosureBooksWhatIsLeft`, `TestClosureBeforeItsExitLeg`. The
  zeroing valuation also confirms the gap it ends, with the exit among
  that gap's moves. A sub-period in which the entity holds nothing and
  moves nothing adds nothing to the chain, so later zero valuations leave
  a closed entity's return where it ended.

  The zero tail runs to the spine's end, so its legs are subsumed in any
  window that holds them. The exit-day zero snapshot itself comes from the
  adapters (`silver.ClosureMarkerBatch`). When a portfolio fully empties,
  the exit date's batch replays the previous snapshot's positions at zero
  value. An empty batch is invisible to gold's history and as-of queries,
  which would otherwise carry the pre-exit marks forward as phantom value.
  It is the closure mirror of an inception anchor: the track record ends
  at a real, dated zero.

## Post-exit residue

An entity can sell nearly everything and keep a small residue, on which
the sold positions' last income lands weeks later. Against so small a
base, that income reads as a gain of many thousand percent, and one such
sub-period would carry the whole chain.

- **An exit** is a sub-period, or a run of them, after which the value
  left is below 1% of the value before it, outflows having taken at
  least 90% of that value. The value left makes the test robust to fees
  and to rates: the outflows of a sale can come to less than its value.
  The outflow floor keeps a write-off, value lost with no outflow, a
  loss.
- **A run** is consecutive sub-periods that each move money out, or move
  none. It holds the opening value of the first plus the run's inflows.
  A sub-period with a net inflow ends it. So a sale spread over several
  days is one exit. A sub-period is measured alone as well, so a run
  that opened higher cannot hide an exit.
- **The residue** is every later sub-period that opens below 1% of the
  value before the exit. Each adds nothing to the chain. The first
  sub-period that opens at 1% or more, after new money say, ends the
  rule, and the chain runs as before. A window held wholly on the
  residue reads 0%.
- **Every window reads one decision.** The rule runs once, on the
  entity's since-inception chain, and each window skips the sub-periods
  that end on the same days. So a window that opens after an exit, or
  inside one, leaves out the same residue and carries the tag, and the
  Returns chart's cumulative line, which divides since-year windows,
  holds its level after an exit.
- The summary row then carries `post_exit_residue`. The income still
  counts in `gain` and in MWR, which read the window's own values and
  flows.
- **One decision, in USD.** Flows convert at their own day's rate and
  values at the valuation's, so a share near a threshold can fall on
  either side of it in another currency. Which sub-periods are residue
  is decided in USD, the engine's canonical currency, on the accounts'
  USD figures, and every reporting currency leaves out the same ones and
  carries the same tag. Where the USD figures cannot be loaded, the
  reporting currency decides alone.
- The two shares are `residueShare` (1%) and `exitOutflowShare` (90%),
  beside `residueCurrency`, in `internal/gold/returns_chain.go`.

A known edge: a withdrawal of 90% and a market fall that leave less
than 1% read as an exit, within one sub-period or across a run. A small
first value that grows has no exit before it, and a partial withdrawal
that leaves 1% or more is no exit. The rule reads the same at every
grain and in every window. Guards: `TestPostExitResidue`,
`TestPostExitResidueValueLeft`, `TestPostExitResidueSameInEveryCurrency`,
`TestPostExitResidueInEveryWindow`, `TestPostExitResidueCumulativeLine`.

## Staggered-inception subsumption

*(referenced by DESIGN.md §10.9)* At coarse grains a constituent that
joins the aggregate value spine mid-window ("staggered inception") must
have each boundary-crossing dollar counted **exactly once**, in the same
bucket as the value change it causes. An account fed only by month-end
snapshots (a pension or 3a account, say) can be funded mid-month and
first appear weeks or months later, so a naive near-debut funding dedup
never fires and the engine
books *both* the synthetic onboarding and the real funding — driving the
chained TWR below −100%.

**Mechanism (`entityFlows`: each leg's region and fate):**

- A constituent debuting at `d > winFrom` has every own external flow dated
  `≤ d` **dropped** from the aggregate flow series; onboarding books the
  **first-snapshot value** at `d`, less the legs netting cancelled (below).
  An account already alive at
  `winFrom` keeps all in-window flows and gets no onboarding.
- The **closure mirror** (`lastNonzeroDay`): a closing constituent's flows
  dated after its last non-zero carried value — anywhere in the terminal
  zero-carry tail — land where the series shows no ΔV. Under the default
  `ClosureScope` they are subsumed, and the closure flow books them on
  the zeroing day; `ClosureLedgerExact` keeps them as the real exit legs.
- **Netting interaction:** stored pairs and transfer/journal netting run
  over the legs of every account valued by the window's end (pre-debut
  legs included) **before** subsumption, so genuine internal pairs
  annihilate and only a constituent's own surviving pre-debut/closure
  capital is subsumed. A surviving journal-OUT sits on an *alive* account
  whose value series reflects the drop, so it is real, not an orphaned
  phantom.
- **A cancelled pre-debut leg reduces onboarding** by its signed amount,
  where the partner's account is on the spine by then and shows the leg by
  the window's end. The debut value holds that money and the pair's other
  leg is gone, so booking the value in full would read an internal move as
  an outside inflow, and the flat value as a loss. At the global grain this
  covers a move between two sources. Money from an account the entity does
  not hold yet is new to it, so such a pair changes nothing. The reduced
  amount may be negative: an account that passed on more than it kept. What
  happens before a first valuation is capital in either direction, never
  performance. Guard: `TestFundedDebutBooksNoNewMoney`.
- **A debut after the window's end** is no part of the window. The entity
  holds an account from its first valuation, so a window that ends
  before it holds none of the account's value:
  - The account's legs in the window are no flows of the entity. Money
    paid into it is not in the entity yet.
  - A leg on a held account that pairs with one of them is a real flow.
    The money left the value the window measures. Its partner is no flow
    of the window, so the leg counts in `unmatched_transfers`.
  - A stored pair with a leg on the account does not bind, so it counts
    in no `cross_source_netted`.
  - A window that ends on or after the debut reads the same pair as an
    inside move that funds the debut.

  Guards: `TestWindowBeforeDebutKeepsThePartnersLeg`,
  `TestWindowBeforeDebutDropsItsLegs`,
  `TestWindowBeforeDebutUnbindsAStoredPair`.
- **A zero first value is no valuation.** It holds none of the money
  moved in before the account's first nonzero valuation. The test is the
  net of every leg on or before that valuation, paired or not
  (`movesInBy`). Where it moves money in, the engine drops the zero head
  (`readZeroHead`), and the first nonzero valuation is the debut:
  - Every leg on or before it funds it, as for any debut. A deposit
    from outside, a transfer in from an untracked account and a move
    from a tracked account all count.
  - A round trip before the zero moves nothing in.
  - Money moved in after the zero funds the debut too.
  - A net within the value netter's tolerance counts as none, so FX
    between the legs of a round trip does not read as money in.
  - The legs are netted in USD, as the post-exit residue is decided
    ("One decision, in USD"). Every reporting currency then drops the
    same zero, so the account debuts on the same day in each.

  The household reads, flow for flow, as the same household without the
  zero, in every window: one that opens before the legs, between them
  and the zero, at the zero, between the zero and the debut, or on it.
  Two cases keep the zero:
  - Where the legs move more out than in, or nothing at all, the zero is
    the first valuation. Money paid out before it funds the zero, since a
    zero is what paying out everything leaves. A later value reads as the
    account's own.
  - Where no nonzero value follows, every valuation says none of the
    money is there. The money moved in funds the zero: a pair's money
    leaves as capital on it, and money from outside never counts.

  Guards: `TestZeroIsNoValuation`, `TestZeroHeadShapes`,
  `TestMovesInByNetsTheLegs`, `TestReadZeroHeadDropsOnlyAFundedZero`,
  `TestZeroDebutKeepsItsReading`, `TestZeroDebutFundsItsFirstNonzeroValue`,
  `TestZeroDebutInsideTheWindowsOpening`, `TestZeroDebutInASinceYearWindow`,
  `TestZeroHeadDebutBooksWhatItsValueDoesNotHold`,
  `TestZeroDebutThatStaysZeroPaysOut`,
  `TestPerEntityOnceZeroDebutCarriesItsFunding`,
  `TestZeroHeadAgreesAcrossPaths`, `TestZeroHeadDecidesOnceAcrossCurrencies`.
- **The same holds at a closure.** A leg the closure day first shows,
  cancelled against money that stays inside, leaves the synthetic closure
  flow.
- **Per-entity-once offsets the same legs** (`groupOnboardStep`). Its
  step is the debuting accounts' first value, less their cancelled
  pre-debut legs, less a sibling's drop on the debut day. Funding days
  before the debut is an inside move there too, and so is funding from
  another source at a grain that holds both. A cancelled leg that the
  sibling's drop on the debut day shows is taken off once, not twice.
  The step is floored at zero. Guard:
  `TestPerEntityOnceOffsetsCancelledFunding`.

Onboarding still legitimately recognizes **untracked pre-existing
capital** — a late account whose backfill carries no funding transactions
at all: booking its first value as onboarding is correct, not a
double-count. Guard: `TestStaggeredJournalFundedNoPhantom`.

## Pluggable per-source policy

*(referenced by DESIGN.md §10.9 and `internal/returns/returnspolicy.go`)*

Source economics differ: NAV-only tiny bases, drained closures, conduit
cash. A single hardcoded model is wrong for most sources. Per-source
`if` branches in shared engine code would leak one source's behaviour
into another, because at the global grain every constituent merges into
one entity. So the engine reads a declarative
`ReturnsPolicy` per source instead, and the returns calculation names no
source.

- **Registry.** `internal/returns/returnspolicy.go` defines
  `ReturnsPolicy` and a registry keyed by adapter kind (`RegisterPolicy`
  / `ReturnsPolicyFor`). Each source declares its policy in its
  `silver.Source`, and `silver.Register` forwards it to the registry
  from the adapter package's `init()`. Each constituent keeps
  its own source's policy inside a merged entity, so a policy is
  source-scoped even at the global grain. `DefaultReturnsPolicy(flow)`
  sets every knob to the engine's baseline, so a policy that changes no
  knob changes no number.
- **Knobs.** Each has a live engine site, and a knob is added together
  with the code that reads it:

  | knob | default | effect |
  |---|---|---|
  | `Flow` | per source | the completeness regime: which collected rows of the capital kinds count (`canonical.CapitalKind` says which kinds those are) |
  | `OnboardScope` | per constituent | `OnboardPerEntityOnce` books one step-up per entity (`groupOnboardStep`) |
  | `AccountsGrain` | normal | `blanked` shows values with TWR/MWR n/a + `accounts_grain_meaningless`; `hidden` emits no rows, while values and flows still aggregate |
  | `CapitalCallRisk` | off | keeps `nav_only_capital_call_risk` on a window where the source's own accounts observed no external flow |
  | `ClosureScope` | `ClosureSubsumeDrains` | `ClosureLedgerExact` keeps zero-tail flows as real exit legs ("Netting and closure") |
  | `Inception` | full window | `InceptionFirstRealSnapshot` anchors the window at the first real position snapshot |
  | `ConduitKinds` | none | account kinds that feed the value spine but get no per-account onboarding |
  | `ExternalOnly` | off | drops every row the silver tagged `returns_flow: internal` (`attachOneFlow`) |

- **Flow classification.** One table says which kinds move capital
  (deposit, withdrawal, transfer in and out, journal) and which of them
  net (the transfers and journal); it lives beside the kind roles in
  `internal/canonical` (`CapitalKind`). A source declares only its
  completeness, the regime:
  - flow-complete counts every capital kind: banks, pensions, angellist,
    and carta and equityzen (`PrivateMarketLedgerPolicy`), whose holding
    legs (buy, sell, contribution, distribution) are no capital kind;
  - nav-only counts no collected row; manual counts only the
    equity-transfer ledger's legs.

  A ledger leg counts under every regime, and nets against a partner where
  a grain holds both.
- **Deposit banks and card issuers** register the bank policy. Their
  accounts are cash or cards: a cash account is plumbing and hides on any
  source, and a card emits nothing ("Credit cards are returns-invisible").
  The policy states that rather than fall through to
  `unknown_adapter_policy`.
- **UBS.** UBS is a conduit relationship: external cash lands in a cash
  account and funds securities in another account of the same
  relationship. Its policy sets four knobs:
  - `OnboardScope = OnboardPerEntityOnce`: the relationship's inception
    step-up is booked once, net of the cancelled legs that funded it and
    of same-day negative sibling funding drops, floored at 0. An internal
    cash→securities move onboards nothing.
  - `ConduitKinds = [cash]`: cash feeds the value spine but gets no
    per-account onboarding.
  - `ExternalOnly = true`: the silver tags conduit churn
    `returns_flow: internal`, and the engine drops those rows. A tag is
    used rather than a rewritten kind, because spending reads the kind
    too. The silver classifier (`pdfCashIsExternal`,
    `buildSameDayOffsetVeto`) is described in
    [adapters/ubs.md](adapters/ubs.md) §5 and §7.
  - `Inception = InceptionFirstRealSnapshot`: the window starts past the
    sparse cash-only pre-history, so a tiny opening base cannot inflate
    the return.
- **cointracking** registers the default policy. A coin's transfer legs
  count as any source's transfers do. A move between two wallets nets
  where a grain holds both wallets: the netter pairs its legs by coin and
  quantity, and with `returns_transfer_matching` on, the pair the load
  stores nets too. A wallet that debuts funded by such a move onboards only
  what the move did not bring in. A leg nothing pairs is a flow at the
  day's rate.
- **Config overrides.** wealthdb.cfg can adjust a source's registered
  policy (`returns_policy_overrides`, DESIGN.md §5.6), keyed by
  silver_source_id. The override applies at the engine's single
  resolution point (`newAccountData`), so the CLI and the materialized
  `report_returns` agree. `flow_regime` replaces the source's
  completeness. `accounts_grain` sets the per-account display
  mode. It is the escape hatch for data completeness that differs from
  the registered default. One example: a flow-counting source whose
  silver carries no transactions pins back to `nav_only`. The
  `returns_hide` block (DESIGN.md §5.7) hides specific account or
  portfolio ids the same way: no rows, but values and flows still
  aggregate.

**Not done: residual onboarding.** Subtracting a conduit's counted
inflows from per-constituent onboarding cannot work. The conduit's gross
throughput dwarfs debut values, so there is nothing to net against. The
per-entity-once step-up (`groupOnboardStep`) handles conduits instead.

## UBS stamp-duty routing

UBS's unmapped `cash_movement` falls through to deposit or withdrawal by
sign, not to `other`. Stamp duty is routed to `tax` by its distinctive
terms anywhere in the narrative ([adapters/ubs.md](adapters/ubs.md) §6).
The fall-through stays deposit/withdrawal on purpose. `other` would drop
genuine unlabeled wires, which are real external capital, out of the
flow series, and that is the worse error. A stamp-duty variant without
those terms still reads as a deposit or withdrawal.

## Quality flags

The `quality` column is the honesty surface: every n/a carries a reason and
every approximation is tagged. Computed set:

`since_data_inception`, `configured_inception`, `partial_window`,
`staggered_inception`, `accounts_grain_meaningless`, `empty_bucket`,
`carried_forward`, `boundary_same_snapshot`, `stale_snapshot`,
`dropped_while_nonzero`, `dietz_degenerate`, `loss_beyond_total`,
`nonpositive_base`,
`mwr_no_flows`, `mwr_no_sign_change`, `mwr_nonunique`,
`mwr_no_converge`, `mwr_incomplete_flows`, `mwr_negative_net_capital`,
`unmatched_transfers=N`, `cross_source_netted=N`, `rolled_forward`,
`post_exit_residue`, `journal_present`,
`nav_only`, `nav_only_capital_call_risk`, `unknown_adapter_policy`,
`fx_clamped_flow`, `pre_fx_history`, `flows_before_inception`,
`after_tax`.

A benchmark's figures have flags of their own ("Benchmarks" below):
`benchmark_gap`, `benchmark_partial_period`, `benchmark_start_clipped`,
`benchmark_end_clipped`, `benchmark_no_overlap` and `shadow_nonpositive`.

`post_exit_residue` marks a summary row whose chain left out sub-periods on
the residue of an exit ("Post-exit residue").

`loss_beyond_total` marks a summary row whose TWR is n/a because one of its
sub-periods lost more than everything while the entity's value stayed at
zero or above ("The math").

`rolled_forward` marks a summary row whose window starts or ends with money
moved that a valuation has not shown yet. TWR and MWR read it rolled in;
`start`, `end`, `net_flow` and `gain` read the carried values.

`configured_inception` marks a window truncated to a configured
inception override (DESIGN.md §5.4) rather than the data's own start.
`accounts_grain_meaningless` blanks TWR/MWR on accounts-grain rows of a
source set to the blanked accounts-grain mode (its accounts' returns are
noise one by one; the coarser grains stay valid). Plumbing goes further:
every `cash` account, whatever its source, a source set to the hidden
accounts-grain mode, and `returns_hide` ids emit no rows at all, nor does a
portfolio or source made only of them, while still feeding every aggregate.
`mwr_negative_net_capital` reports XIRR as n/a because the
window's net invested capital (opening base plus net external flow) is
zero or below.

`stale_snapshot` marks a bucket (or the summary row) whose end-day
valuation rests on a snapshot older than 3× the entity's median
snapshot gap — the feed-died-mid-bucket case that `empty_bucket` /
`carried_forward` cannot see, since the bucket itself still holds
snapshots. The cadence is inferred per entity, so a daily feed flags
after days and a quarterly manual source only after months, with no
per-source configuration; with fewer than 3 observed gaps no cadence
is inferred and the flag stays off.

`unknown_adapter_policy` is unreachable through the real pipeline.
A load opens a source only through a registered adapter, and each
adapter that emits accounts registers a policy from its `init()`. A
guard test (`TestEveryAccountKindHasPolicy`) checks every registered
kind against the policy registry. The reference sources (curves, fred,
refdata) are exempt: they emit no accounts and never reach the returns
engine. The flag stays as defense in depth, unit-tested at policy
resolution.

`flows_before_inception` marks an entity whose observed external ledger
flows reach back before its resolved inception (data-derived, policy
anchor, or a §5.4 `inception_overrides` floor): those flows sit outside
every window the entity can produce, so the measurement cannot see them.
With a configured override the flag documents exactly what the override
excludes; without one it means the transaction backfill reaches further
back than the value spine supports (the case the §5.4 override is the
remedy for). Purely diagnostic — no flow handling changes — and anchored
on the inception, so an explicit FROM or a windowed-partition clip never
raises it.

No flag marks a corporate action inside a window, or a dormant account
carried forward.

## Benchmarks

A benchmark is a named mix of funds with target weights, rebalanced
monthly. One fund at 100% is the simple case. Funds stand in for
indexes: a fund's total return is public, and an index's own series is
licensed. A fund's fee costs a few basis points a year.

The engine ships a built-in set, on with no config:

| Benchmark | Funds |
| --- | --- |
| S&P 500 | IVV 100 |
| World (ACWI) | ACWI 100 |
| US total bond market | BND 100 |
| Global 60/40 | ACWI 60, BND 40 |
| Global 80/20 | ACWI 80, BND 20 |

The `benchmarks` block of wealthdb.cfg adds one, or redefines a built-in
one by its name (DESIGN.md §5.1). `wealthdb help benchmarks` lists them.

Examples:

```sh
wealthdb returns global --benchmark "Global 60/40" --period annual
wealthdb returns global 2024 --benchmark "S&P 500" --period total -x CHF
```

**Months.** Each public N-PORT filing states the fund's total return for
each of the three months it covers, per share class. refdata keeps the
class the ticker maps to, and gold holds them in `fund_returns`. Where
two filings state a month, the later one counts.

- US registered funds only. A configured mix of UCITS funds has no
  returns.
- History starts in 2019.
- Filings are quarterly and public 60 days after the quarter's end, so
  the newest months lag by two to five months.

**A month's return.** `r = Σ wᵢ rᵢ`. A month that lacks a fund of the mix
has no return: it is a gap, and no month is ever invented. A mix runs
from the latest first month of its funds to the earliest last month.

**Currency.** Funds report in dollars. Read in another currency, a month
is unhedged at month-end FX: `(1 + r) × FX_end / FX_start − 1`. The rate
comes from `fx_daily`, by the chain a holding is valued by. A rate that
falls back to the day-0 clamp carries `pre_fx_history`. A month with no
rate is a gap.

**Growth between two days** (`returns.Index`). Whole months compound. A
day inside a month has earned the share of the month its days so far
make up, compounded per day. The first of a month has earned none of
it, so a window from January 1 takes January whole, and the buckets of
`report_returns` compound their months exactly.

**The days compared.** Every comparison runs over the days both sides
have. A window that starts before the benchmark's first month compares
from there (`benchmark_start_clipped`). A window that ends after its
last month compares through it (`benchmark_end_clipped`). The engine
then computes the household over those same days, so the excess is
like with like. That figure can differ from the Return tiles, which run
to today. A window with a gap has no return (`benchmark_gap`). It does
not skip the month. A window that shares no day with the benchmark's
months has no return either (`benchmark_no_overlap`).

The MCP `returns` and `exposure` tools take the same `benchmark`.

**Buckets.** `benchmark_returns` holds a row per household bucket, at
each granularity:

- A bucket before the benchmark's first month, or after its last, has
  no row.
- A bucket its months start or end inside has no return
  (`benchmark_partial_period`): part of a quarter beside a whole quarter
  is not like with like.
- A bucket through a gap has no return (`benchmark_gap`).

**Excess return** is the plain difference, portfolio less benchmark, in
percentage points.

**The shadow value** starts at the window's start value, as MWR reads it:
with what moved but is not shown yet rolled in. Each flow of
the household enters at the end of its day, and every amount then grows
with the benchmark. It is the same money on the same days in another
investment. The benchmark's MWR is the XIRR of the household's flows
with the shadow's value at the end: a public market equivalent. The
flows are those `entityFlows` gives the global grain. A shadow the flows
take to zero or below is a short position from that day on, which no
household holds. It keeps its value but has no MWR
(`shadow_nonpositive`).

**The money-weighted lines.** `benchmark_mwr` holds, per benchmark,
currency and start-year window, two series of month ends:

- `portfolio`: the household's value, and its MWR from the window's
  start to the day. The MWR reads each month end's value with what is not
  shown yet rolled in, as the tile does. It runs through the household's
  own window, so its last point is the Return (MWR) tile where the window
  starts inside the benchmark's months.
- `benchmark`: the shadow's value and MWR, through the benchmark's last
  month. Its last point is the benchmark's MWR tile.

Each series opens with a row at the window's start, with no return, so
the two sides start level. A window with no opening value has no lines.
A month end past a gap has no value and `benchmark_gap`. One with no
XIRR answer has the engine's `mwr_*` flag.

Each month end's solve starts from the rate of the one before, over the
flows netted by day (`returns.FlowSeries`). The last one is solved from
the engine's usual guess over the flows themselves, as the summary row
is, so a line ends on its tile's number. A month end's `mwr_nonunique`
reads the flows netted by day, as the summary row's does.

`web refresh` writes both tables with `report_returns`, in one
transaction, from the same datasets. `returns global --benchmark`
computes the same figures live.

## Out of scope

There is no gross-of-fees mode. Returns are net of fees and after tax.
