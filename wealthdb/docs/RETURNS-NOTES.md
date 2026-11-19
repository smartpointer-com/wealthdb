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

`wealthdb returns private` says what each private fund or SPV took,
what it gave back and what it is marked at. It reads gold alone.

**Which holdings it lists.** A private holding is an instrument in one
account that a `contribution` line names: a capital call, or an SPV
subscription. The rule reads the cash legs, not a source or a taxonomy,
so it holds for every private-market source. Two kinds stay out:

- A private company's shares bought directly are a `buy`. Their cost
  and their sale are in `wealthdb gains`.
- Founder stock and options have no cash leg at all.

A holding with no money in, out or marked is no investment, and the
view leaves it out: a call refunded whole, or a call of no amount. The
test reads the legs and the lines in their own currency, so every
output currency lists the same holdings.

One row per holding, then the total:

| column | reads |
|---|---|
| `paid_in` | the `contribution` lines, net of refunds |
| `distributed` | the `distribution` and `sell` lines, and the exits in kind: a `transfer_out` of the holding, at the value it states |
| `nav` | the holding's lines at the latest snapshot. It is 0 once the holding has left the holdings, and blank where it never had a value |
| `marked_at` | the oldest mark of those lines, `coalesce(valued_at, snapshot_at)`; for a holding that has left, the mark of its last line |
| `dpi` | `distributed / paid_in` |
| `tvpi` | `(distributed + nav) / paid_in` |
| `irr_pct` | the money-weighted return since the first call, a yearly rate |

An SPV that pays out cash books a `sell`, as AngelList does, so
`distributed` counts a sale as a distribution. Without it an SPV that
exited for cash would read a TVPI of 0. An SPV bought as a `buy`
(EquityZen) is a direct holding: it is in `wealthdb gains`.

**One definition.** The macro `report_private` computes every figure but
the IRR, from `private_legs`. The IRR is the engine's own solver
(`solveIRR`, the money-weighted code of every grain): the legs as flows,
nothing held before the first call, and the NAV received on `end_date`,
the later of the NAV's snapshot and the last leg. So
`irr_pct` carries the engine's `mwr_*` tags where it has no answer.

**Historic FX.** Each leg converts at its own day's rate, and the NAV at
its snapshot's. The multiples therefore move with the output currency.

**The total.** Each holding's money rounds once, to the cent, and the
total adds the cents, so it is the sum of the rows as they print. `dpi`
and `tvpi` pool the holdings: they are the ratios of the sums, not an
average of the rows' ratios, and both mean what a fund's own DPI and
TVPI mean. `irr_pct` pools the legs of every holding listed. The
total's `marked_at` is the oldest mark, and a holding with no value
leaves the total's `nav`, `tvpi` and `irr_pct` blank. The total is over
every holding: a filter keeps or drops it as a row, never recomputes
it.

**No mark, no NAV.** Every row has a mark but one: a holding that never
had a value. Its `nav`, `tvpi` and `irr_pct` are blank, never zero, and
it says `no_mark`. A holding that had lines and has left reads a NAV of
0, a real one: gold holds it no more. Its multiples and IRR are final,
its `marked_at` is its last line's mark, and it says `exited`.

Quality tags:

- `no_mark`: the holding never had a value, so `nav`, `tvpi` and
  `irr_pct` are blank.
- `exited`: the holding has left the holdings, so `nav` is 0 and
  `marked_at` is the mark of its last line.
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
- `unpriced=N`: N legs or lines have no rate to the output currency. The
  sums leave them out, and a leg with none leaves `irr_pct` blank. A
  zero needs no rate.

On the total each tag counts the holdings it holds on.

Limits:

- Marks are old by nature, so `marked_at` always shows.
- Commitments and unfunded capital are not in gold.
- The view carries no volatility or other risk figure. Marks are too old
  and too sparse for one.

`web refresh` writes the view's rows in every reporting currency to
`report_private_markets`, and the Private Markets dashboard reads them
through `web_private`, the IRR with them. Guards: `TestPrivateMarkets`,
`TestPrivateMarketsHistoricFX`, `TestPrivateMarketsTableIsTheCLIsRows`,
`TestPrivateView`.

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
    holding. It pairs by holding and quantity, never by value: one coin
    out, one coin in. The two legs move one holding in opposite
    directions, at most 7 days apart (`lots.PairWindow`), in the same
    quantity (`sameQuantity`). What the same quantity is depends on the
    holding's asset class and vehicle, each the row's own where it states
    one, else its instrument's:
    - **A coin** (asset class `crypto`, vehicle `physical`): within 5% of
      the larger (`lots.PairTolerance`), so a network fee taken in kind
      still pairs, and its value reads as a loss.
    - **Everything else:** a security (a share, an option contract, a unit
      of a fund, a bond) never arrives short, so only the exact quantity
      pairs, up to float noise (1e-9 of the larger). 100 shares out and 99
      in do not pair. This includes a share of a fund or trust that holds
      coins (class `crypto`, vehicle `etf` or `fund`), and a class or
      vehicle gold does not state.

    The value netter runs first, over every leg, but pairs two legs only
    where both are cash or both moved one holding in the same quantity.
    The legs that moved a holding and are left then pair by holding and
    quantity (`pairInKind`):
    - **Pairing.** Each leg, in day order, takes the open leg of the
      closest quantity, the latest on a tie. One leg pairs with one leg:
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
      where the legs moved the same quantity the first valuation holds
      (`sameQuantity`). The first value then stands for them at the
      debut's prices, by the holding's share of that value
      (`movedLegs.heldAt`). So the price move up to the debut is
      return, and a shortfall, a fee in kind, is a loss. After a zero
      first value the debut is the late one ("Staggered-inception
      subsumption"), and its valuation is the one read. Each holding's
      part of the first value is read only where the first valuation
      holds positions in one currency and no cash. Elsewhere, and where
      the quantities are not the same, a leg funds its debut by its
      value. A first value below that value then books the difference as
      capital that left: nothing says the same holding arrived.
  - **Cash.** A leg that names no instrument pairs by value:
    ε = max(1.00 outCcy, 0.5% of the larger leg); window ±3 calendar
    days; FX-normalized via `value_outccy`. It is deterministic:
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
  legs moved one holding in quantities that are not the same (a security
  at all, a coin beyond 5%), the pair binds the quantity both legs hold.
  The larger leg's rest, at its share of the leg's value, stays a transfer
  nothing pairs: a flow, counted in `unmatched_transfers`. A coin short
  within 5% pairs whole: the shortfall is the network's fee.
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
  over the full candidate set (pre-debut legs included) **before**
  subsumption, so genuine internal pairs annihilate and only a
  constituent's own surviving pre-debut/closure capital is subsumed. A
  surviving journal-OUT sits on an *alive* account whose value series
  reflects the drop, so it is real, not an orphaned phantom.
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
- **A zero first value is no valuation.** It holds none of the money
  moved in before it. Every leg that moved money in on or before the
  zero, paired or not, funds the account's first nonzero valuation
  instead, the late debut (`lateDebut`). A deposit from outside and a
  transfer in from an untracked account fund it, as a move from a
  tracked account does. The late debut books what its value shows beyond
  the account's own legs since the zero, less what the paired legs
  brought, as a debut books its first value: the rest is capital either
  way. The chain carries a pair's money until then, whatever the gap,
  and a coin moved in funds it by quantity, at that day's prices. The
  account's own later legs keep their own reading. The household reads
  as it would without the zero, in every window: one that opens before
  the legs, between them and the zero, at the zero, or between the zero
  and the late debut. Per-entity-once books a late debut in the step of
  its day. Three cases keep the zero:
  - Money the account paid out before the zero funds the zero itself,
    since a zero is what paying out everything leaves.
  - Where no nonzero value follows, every valuation says none of the
    money is there. The money moved in funds the zero: a pair's money
    leaves as capital on it, and money from outside never counts.
  - Where no money moved in before the zero, the account reads from the
    zero on.

  Guards: `TestZeroIsNoValuation`, `TestZeroDebutKeepsItsReading`,
  `TestZeroDebutFundsItsFirstNonzeroValue`,
  `TestZeroDebutInsideTheWindowsOpening`, `TestZeroDebutInASinceYearWindow`,
  `TestLateDebutBooksWhatItsValueDoesNotHold`, `TestZeroDebutThatStaysZeroPaysOut`.
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
