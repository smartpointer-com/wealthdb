# Cashflow

The household's cash flow statement, and the Sankey that draws it:
where cash came from and where it went over a window, across the
household's accounts, with its own internal movements removed and
buying-and-selling shown as one net movement rather than two gross
ones.

This document is a **delta against [SPENDING.md](SPENDING.md) and
[INCOME.md](INCOME.md)**. Cashflow is the third reading of one engine:
one enrichment pass, one precedence lattice, one internal-transfer
matcher, one signature normaliser, one taxonomy. Where a mechanism is
shared, this file says so and points at the section that explains it.

What it adds is smaller than either predecessor and worth stating up
front:

- **Almost no collector work.** The resolution reads neither bronze nor
  silver. The one exception is the far account: the counter account a
  bank states on its own row is a fact only a collector can carry, so
  the adapters and collectors able to state one do (§4).
- **No new tier and no model.** The precedence lattice is untouched,
  `categorize` gains nothing, and the backlog is the two families'.
- **No new store.** Nothing is bought from a model, so `reload -a`
  carries nothing new across.
- **The centre of gravity is SQL.** One resolution macro maps each line
  to a node; everything above it is reports over that macro.

---

## 1. Scope: both directions, and the movements neither family claims

Income reads what arrived and spending reads what left. Cashflow reads
both, adds the kinds of movement neither books, and closes the
arithmetic:

```
operating + investing + financing + vehicles = change in cash
```

It answers the question a household-balance diagram answers: what came
in, what went out, and what was left.

| section | what crosses the edge | sign |
|---|---|---|
| `operating_in` | the income verdicts, plus the reimbursements income excludes | + (net of reversals) |
| `operating_out` | the spending verdicts, plus moves into a charitable or custodial vehicle | − (net of refunds) |
| `investing` | buys and sells; private capital called and returned; capital deployed to or returned from a destination the product does not track | net |
| `financing` | what the mortgage rule places, an own-account move to a mortgage account (matched or stated), `mortgage_transfer`, `loan_proceeds`, `debt_repayment` | net |
| `vehicles` | moves to or from a retirement plan, an education or health account, a non-grantor trust, or an untracked account of the household's own | net |
| `cash` | the residual: the four summed, seen from the pool's side | computed |

`summary` prints four statement sections — operating is the signed
difference of the two halves — while `flows` and `sankey` carry the six
values above, because a node has to know which half of operating it
belongs to.

**The sign rule, once:** positive is cash arriving in the pool,
negative is cash leaving it. gold's canonical signs already mean that
for every kind the statement admits, the card kinds included — a card's
balance is carried as negative cash, so a purchase on it drives the
pool down exactly as one on a deposit account does. Nothing in the
resolution negates anything.

What is deliberately **not** here:

- **Not a P&L.** Investing shows cash moved, not gain realised.
  Selling at a loss is an inflow.
- **Not returns.** "External to the thing being measured" and "across
  the household's cash edge" are different edges. The two agree on most
  rows and are allowed to disagree on the rest.
- **Not tax reporting.** Withholding a vehicle deducted before the
  household saw the money is on no node at all (§3).
- **Not the vehicles' own statements.** Giving routed through a
  vehicle — a plan distribution paid straight to a charity — is that
  vehicle's, not the household's.
- **Not accrual, not a budget, not a forecast, and not gross of
  payroll.** Income is what arrived, as the income family decided.

---

## 2. The pool

Cashflow measures movement across the edge of the household's **cash
pool**: cash and cash equivalents on the household's accounts. Deposit
balances, brokerage cash, exchange fiat, a card's owed balance (which
the product already carries as negative cash), and the instruments
whose asset class is `cash` — money-market funds and time deposits.

Two things draw the edge:

- **What the balance is.** A security is not cash, so buying one is
  cash leaving the pool. A money-market fund bought with idle brokerage
  cash is cash becoming cash, and the diagram does not show it.
- **Whose the account is.** The household's, or a vehicle's beyond the
  boundary of §3.

The pool is exactly `cashflow_pool_accounts()` — the scoped accounts
whose tax wrapper is on the household side — and every rule reads it
rather than re-deriving "household" per rule. An account the
configuration takes out of the pool is treated like any account the
product does not hold: a move to it is a crossing, not an invisible
internal step. That equivalence is what keeps the scope from silently
inflating the residual.

---

## 3. The household boundary

A household does not count its retirement plan, its education accounts,
its health savings account, its donor-advised fund or a trust it funded
as itself, even when it holds every login. The boundary is drawn by
**tax wrapper** (`canonical.DefaultWrapperSide`), an engine default a
deployment can override per wrapper (§7).

Two questions decide where a wrapper sits, in order:

1. **Is the money still the household's?** No — a completed,
   irrevocable transfer — and the crossing is `operating_out · Giving`.
2. **Is it earmarked** for a purpose or a life stage the household does
   not treat as current money? Yes, and the crossing is a `vehicles`
   move.
3. Otherwise it is the household, and a move is pool-internal.

**Reversibility is not the test.** A retirement account can be drawn on
at any time, with a penalty, and nobody counts it as this month's
money; an education plan and a health savings account are the same in
kind. What separates them from a current account is not the lock but
the earmark.

| tax wrapper | household? | a crossing lands in |
|---|---|---|
| `taxable_personal` `taxable_joint` `trust_grantor` `other`, and unset | yes | nothing: it is pool-internal |
| the seven US retirement wrappers, `pillar_2` `vested_benefits` `pillar_3a` | no — earmarked | `vehicles · Retirement savings` |
| `529` `coverdell_esa` | no — earmarked | `vehicles · Education savings` |
| `hsa` | no — earmarked | `vehicles · Health` |
| `trust_non_grantor` | no — a separate taxpayer | `vehicles · Trusts` |
| `charitable` `trust_charitable` `foundation` `custodial_utma` `custodial_ugma` | no — given away | out: `operating_out · Giving`; in: `operating_in · Other receipts` |

An education plan and a custodial account land on **different sides of
the same child**, which is the two-question test doing its work: the
plan's owner can revoke it and take the money back, so it is the
household's capital in an earmarked pool; a custodial account is
legally the minor's the moment it is funded, so funding it is a gift.

`other` is on the household side because the product cannot know
better. The enum covers two jurisdictions; a pension, insurance or
company wrapper from a third can only be `other` today. The remedy is a
new enum value rather than the per-wrapper override, which cannot tell
two `other` accounts apart.

**An unset wrapper reads as household** — the render-time default gold
applies everywhere else, and the safe direction, because nothing is
silently removed from the statement. It is not free: an adapter that
leaves the column unset puts a retirement or health account inside the
pool, where its own trades become household investing and its
contributions become invisible. Nothing in the reconciliation can see
that, because the crossing is **absent** rather than wrong. So the load
summary and `status -v` both count pooled accounts with no wrapper
(§10).

### What follows from the boundary

- **A vehicle's accounts contribute no lines.** Its dividends, its
  trades, its fees are not the household's cash flow. They stay in the
  income and spending reports, which count the holder's accounts rather
  than the household's — so the totals here are smaller than those
  reports' by **four** terms, not one (§8).
- **A crossing is one line, on the household's side.** A contribution
  is the household account's outgoing leg, sorted by the far account;
  the plan's incoming leg is the vehicle's and is not a line. The
  crossing therefore reads the same whether or not the plan is
  collected — only how it was placed differs.
- **Spending paid from a vehicle is not on the statement; spending
  reimbursed by one is.** A medical bill the health account pays
  directly draws nothing. The same bill paid from a current account and
  then reimbursed is two real lines, which is the honest reading of
  what the cash did.
- **A move between two vehicles is invisible**, and that has a cost: a
  plan distribution paid straight to a charity is giving the household
  would claim and the statement does not show. The Giving node is the
  household's giving, not the family's.
- **Tax withheld inside a vehicle is not on the statement.** The Taxes
  node is the tax the household paid from its own accounts, which in a
  drawdown year is less than the tax it bore.
- **Payroll-funded contributions are invisible**, for the reason income
  is net as booked: a deduction the household never received is on no
  account the product holds.
- **The wrapper decides, not the kind.** A brokerage account inside a
  retirement wrapper is a vehicle; a cash account inside a non-grantor
  trust is a vehicle.
- **The boundary is time-invariant.** A wrapper is one column with no
  validity range, so a grantor trust that becomes non-grantor changes
  every year at once. A dated override is a follow-up (§11).

---

## 4. The resolution

`cashflow_txn_nodes(from, to)` maps every transaction to a node —
`section.class.group` — and a `disposition` saying whether it is a line
at all: `line`, `internal` (pool-internal, and invisible) or `excluded`
(declined, and counted). `cashflow_lines_base(from, to)` is that,
restricted to the pool and to the lines.

It is **SQL rather than Go** because the dashboard needs the same node
assignment the CLI uses, computed after its pickers apply; a Go-side
resolution would have to be re-implemented in the serving view and
would drift. The returns engine is in Go because its maths cannot be
expressed in SQL. Nothing here is like that.

**One inversion:** cashflow reads each family's **resolution** macro,
never its `*_lines_base`. Four of the verdicts it wants are exactly the
ones a base excludes.

### Where each kind lands

A row's **kind** decides its section; the verdict outranks the kind
wherever both have something to say — except on the six kinds gold
pins no canonical sign for, which are excluded whatever a verdict
says, because a section that reads direction cannot admit them.

| kind | section | note |
|---|---|---|
| `dividend` `coupon` `staking` `capital_gain` `reward` `interest` (+) | operating in | by resolved income type |
| `deposit` | by verdict | an own-account move with a far account, matched or stated → by the far account; `capital_return` → investing; `loan_proceeds` → financing; a `*_transfer` delta → vehicles; `reimbursement` → operating in (income excludes it, cashflow keeps it — it is cash that arrived); otherwise operating in |
| `distribution` | investing | floors to `capital_return`; a rule or a pin promoting it to an income type moves it to operating in, as the holder's word should |
| `purchase` `refund` `fee` `tax` `interest` (−) | operating out | by resolved spend category |
| `withdrawal` | by verdict | an own-account move with a far account, matched or stated → by the far account; `investment` → investing; `debt_repayment` → financing; a `*_transfer` delta → vehicles (`deposit_transfer` → `vehicles · Bank deposits`), except `mortgage_transfer` → `financing · Mortgage`; a rule-placed `internal_transfer` with no far account → `vehicles · Untracked accounts`; otherwise operating out |
| `card_payment` | matched → by the far account; otherwise **excluded, counted** | an unpaired card bill is in neither family's population, so no tier ever saw it. It also has a known false shape: a cross-currency pair no road joins — the amount pass partitions by currency, and a card ledger mints its own ids, so no shared reference reaches it either — whose bank leg the provider tier already files as card spend. Counting it as a receipt would print a phantom inflow and double the bill |
| `buy` `sell` | investing | by the instrument's asset class; a `cash`-class instrument is pool-internal |
| `contribution` `distribution` | investing | private capital, by the vehicle's asset class |
| `transfer_in` `transfer_out` | matched → by the far account; otherwise **excluded, counted** | an unmatched one is an in-kind ledger leg or a source's own tagging, and is counted rather than guessed at |
| `fx` `fx_forward` `fx_swap` `corporate_action` | **excluded, counted** | gold pins no canonical sign for any of these, so their direction is a per-source convention and a section that reads direction cannot admit them. Admitting the last three needs a per-adapter sign pin first (§11) |
| `journal` `other` | **excluded, counted** | the same catch-all exclusion both families make |

**Excluded means counted.** A counter nobody reads is how a silent hole
starts, so `status -v` reports the declined rows on pooled accounts
(§10). Pool-internal rows are NOT counted there: they are movements the
statement deliberately does not draw, and a permanently large counter
is an unreadable one.

### Own-account moves are sorted by the far account

An `internal_transfer` verdict says the money stayed the holder's.
Cashflow needs one more fact: whether it stayed in the **pool**. The
far account is how it knows, and it arrives by two roads.

The **matcher's** road is a pairing: two legs the product collected,
joined by amount and day, by a reference the source stamped on both
halves, or by the other leg the source described on one of them, each
then naming the other's account (`far_silver_source_id`,
`far_account_external_id`, migration 0079).

The two joins are not interchangeable. A pairing on amount is an
inference, so its two legs necessarily agree in currency and very
nearly in size; a pairing on a reference is the source asserting an
identity, so its legs may differ in **both** — a conversion between two
of the holder's own accounts is exactly that shape, and it is the only
shape the amount join can never reach (SPENDING.md §3, *The reference
road*). Nothing here nets a pair: each leg is placed on its own figure,
by its own far account, so the disagreement costs the statement
nothing. Where both accounts are in the pool both legs are simply
invisible, which is what a move inside the household's cash is.

The **source's** road is the bank stating the counter account outright.
Where the product does not collect the far side at all — a wire to an
account of the holder's own whose transactions no feed reports — there
is one row, the matcher needs two, and only the narrative knows. The
UBS adapter reads it from whichever of its feeds states it, and the svb
collector from a deposit-ledger row's reference to a loan, and each puts
it in the row's payload; the pass resolves it against the accounts gold
already holds, and takes it only when it names one of them — in the
row's own source first, and in another source only when no account of
its own answers and exactly one account elsewhere does. A third party's
account resolves to nothing, which is the common case and the right
answer.

That road answers **where the money went and nothing else**. What the
movement WAS stays the tiers' question: a stated counter account places
no verdict, so a row still needs a matcher, a rule or a pin to be
called an own-account move at all. Where no far account is known by
either road, the rules that place the verdict carry a class instead
(`far_class`).

The test is ordered, first match wins, and **the wrapper is asked
before the kind** — a mortgage on a trust-owned property and a card
inside a foundation are both, and the wrapper is what says whose money
moved:

1. the far account's wrapper is on the **vehicle** side → that
   wrapper's class, with the leg's own direction. An outgoing leg to a
   retirement plan is a contribution, an incoming leg from one is a
   distribution, and both are `vehicles · Retirement savings`.
2. the far account's wrapper is a **giving** one → outgoing legs are
   `operating_out · Giving`, because the contribution is irrevocable
   and the vehicle's later grants are its own. **Incoming legs are
   `operating_in · Other receipts`**: a charitable remainder trust's
   annuity, a foundation reimbursing a trustee, a custodial account
   drawn for the minor's costs are all cash arriving, and none of them
   is negative giving.
3. the far account is kind `mortgage` → `financing · Mortgage`, either
   direction (a drawdown is an inflow).
4. the far account is **in the pool** → invisible.
5. anything else — the far account is outside the pool, or there is no
   far account because a rule placed the verdict — → `vehicles ·
   Untracked accounts`, unless the rule recorded a class of its own
   (the mortgage rule records `mortgage`).

Step 5 closes a hole the rule tier opens. A config rule exists
precisely to place a wire to the holder's own account at a bank the
product does not track, and that money **leaves the pool**. Drawn as
invisible it would vanish into the residual forever; drawn as
`Untracked accounts` it is visible as what it is, and the remedy —
collect that account — is obvious from the chart.

**The investing class has three roads, and they are ordered.** The
trade's own exposure first (0097/0100: a feed that cannot name the
instrument can still name what it traded), then the instrument's, then
— beside an `investment` or `capital_return` verdict — the
`asset_class` a config rule or a pins-ledger row carried (§7, migration
0102). The holder's word comes last, and is read AFTER the test that
sends a row naming an instrument with no dimension row to `other`: that
node is how a missing dimension row stays visible, and a stated
exposure read any earlier would hide the hole behind a plausible class.
A stated `cash` is ignored rather than drawn, because that class's
label is the statement's own residual node. Where none of the three
says anything the row is `elsewhere` — which after 0102 means exactly
that, and nothing weaker.

The GROUP stays the verdict on those rows. The class is what the money
went into and the group is what the money did, so a house bought and a
REIT traded share a class node and separate at group grain.

This ladder runs for `internal_transfer` and nothing else, so it is
not the only road to the nodes it reaches. `mortgage_transfer` (§5)
resolves to `financing · Mortgage` on the verdict alone, ahead of any
far-account test — which is the point of it: both steps above that
reach that node read a far side, and a servicer the product holds no
account for has none to read.

---

## 5. The vocabulary

Three levels, whose leaves are the two families' own values:

- **section** — the six values above.
- **class** — the diagram's inner nodes.
- **group** — the diagram's leaves.

A node's identity is **the whole key**, `section.class.group`. Three
values of the shared vocabulary — `gift`, `other` and the unplaced row
— exist on both sides of the household, and keying a node by its leaf
alone would collide a gift given with a gift received in one edge list.

| section | classes |
|---|---|
| operating in | `earnings` · `yield` · `benefits` · `other_receipts` · `(uncategorized)` |
| operating out | `consumption` · `fees` · `taxes` · `giving` · `(uncategorized)` |
| investing | the exposure, from whichever of three roads names one — the trade's own word, then the instrument's, then the holder's `asset_class` on the rule or pin that placed the verdict; every value but `cash`. `other` where a row names an instrument whose class is missing; `elsewhere` where none of the three said anything; all of them folded into `investments` under `--investing whole` |
| financing | `mortgage` · `loans` |
| vehicles | `retirement` · `education` · `health` · `trusts` · `deposits` · `untracked` |
| cash | `cash`, drawn as **Cash savings** |

**Four lifts and no new tier.** Yield is lifted out of income, and
fees, taxes and giving out of spending. The families' primaries cannot
do either: income has one vendored primary, and taxes hide inside a
government-and-non-profit bucket beside donations and a passport
renewal. Fees are lifted because the cost of being invested is a line
worth seeing beside the tax rather than inside a catch-all. The uncategorised class is its own node on each side rather than a
leaf inside Consumption: the families made the backlog a visible
label on purpose.

**`consumption` reads as "Consumption", not "Spending".** The class is
the spending family's categories minus the three lifts, and it is
deliberately not named after the spending report: that report's number
differs from this one by construction (§8), and one word for two
figures a reader is meant to compare is a trap. The same rule renames
the summary's two operating columns — see §8.

### The leaves

The leaves are the families' own values, at the level each family's
vocabulary makes readable — and the two differ:

- **operating in**: the income **detailed** value. Income has one
  vendored primary, so a primary-level leaf would fold every earned and
  yielded type into `INCOME` and say nothing.
- **operating out**: the spending **primary**. That vocabulary has
  ninety detailed values, and a diagram with ninety leaves is not a
  diagram. The detailed value is a column away, behind `-C +detailed`
  on the transactions view.
- **taxes, fees and giving** keep the detailed value. Each of those
  three classes was lifted out of spending precisely for the
  distinction inside it — a tax assessed against one withheld at
  source, a fee for banking against a fee for investing, a donation
  against a gift given — and a class whose one leaf repeats its own
  name is a self-edge.
- A **delta** is primary-level, so `card_spend`, `cash_withdrawal` and
  `other` are their own leaves either way.
- **One detailed value is promoted**: `GENERAL_SERVICES_EDUCATION`.
  The primary rule above is right and one primary breaks it —
  `GENERAL_SERVICES` is a catch-all rather than a category, and school
  fees are a bigger line in most households than several primaries that
  do get an edge. The criterion is a property of the vendored
  vocabulary rather than of any deployment's data: a detailed value
  whose primary does not describe it (migration 0088). Its siblings
  under the catch-all still group by primary.

The vehicle classes are named for the ACT rather than the subject
(migration 0087): **Education savings** and **Retirement savings**, not
`Education` and `Retirement`. Each of those words names two things — a
529 contribution and a tuition payment, a plan contribution and a
pension — and the two sit in different sections but would have read as
the same node on a diagram. `Health` and `Trusts` keep their names:
"Health savings" is right for an HSA and wrong for the class the day it
holds anything else, and nobody says "Trust savings".

Seven leaves are cashflow's own, where no family value says the right
thing: `trades` and `private_capital` on the investing classes;
`vehicle_giving` / `vehicle_receipt` for a crossing to or from a giving
vehicle — whose verdict is `internal_transfer`, which names a movement
rather than a kind of giving or a kind of receipt; and
`mortgage_interest`, `mortgage_amortization` and `mortgage_drawdown`,
which `cashflow_lines_base` mints from direction and the derived split
rather than reading from the resolution, no family value naming half a
row or naming money borrowed against an account the product holds.

On the vehicles and on cash the class **is** the leaf: nothing finer
exists to say, so the diagram draws them attached to the hub and their
leaf level shows in `flows --level group` and not in the Sankey. A
class enters the hub in its own right when it has no leaves; where it
has them, it enters as their sum and they draw below it. **Investing is
held out of that stage deliberately**, at either grain: its classes do
have leaves — `trades` and `private_capital`, which `flows --level
group` shows — but what a leaf stage there would draw, a trade against
a capital call, is a different question from the one the diagram asks,
so investing's classes enter the hub in their own right as well.

Financing has leaves on both its classes. `loans` splits into
`loan_proceeds` and `debt_repayment` — money borrowed against money
repaid, which is the distinction the class exists to carry.

`mortgage` has three, decided by direction first. An instalment is an
outflow and splits into **`Mortgage interest`** and **`Mortgage
amortization`**, which are not the same kind of thing. Interest is
consumed: it buys the use of the money and is gone. Principal is not
spending at all — it moves value from one side of the balance sheet to
the other, and the household is no poorer for it. Summed into one node
they overstate what was consumed by whatever was repaid — and in any
period that happens to carry an extraordinary repayment, that is most
of the node.

Money running the other way is **`Mortgage drawdown`**: a tranche
drawn, a line increased. It is a leaf rather than the bare class for a
structural reason as much as a readable one — a leaf-stage section
draws a row THROUGH a leaf, so a row whose group repeated its class
would be dropped by the self-edge rule and dropped again by its class
having other leaves, reaching the diagram nowhere while the residual
went on counting it. Where it draws follows from the ordinary rule
that a leaf running against its class's net detaches: in a year of
repayments it attaches to the hub in its own right, and in a year that
borrowed more than it repaid the repayment leaves are the ones that
detach.

**The split is derived, and it has to be.** No bank prints the share,
and the narrative cannot be made to yield it: a tranche may bundle its
scheduled amortisation into the same row as its interest under one
booking type, and a closing may book the principal and the final
interest as two rows identical in every field. So the rule reads
neither. Whatever a period retired of the mortgage's own outstanding
balance was principal, and the rest of what was paid into it that
period was interest; within a period the principal goes to the largest
instalment first, capped at its face value, which is the order the two
really occur in on a closing. The balance is **observed** — the
lender's own figure, carried as the mortgage account's position — which
makes this an estimator of the split and not of the debt.

A snapshot is the balance **before** that day's instalment posts, so an
interval runs from one observation up to — not including — the next;
and a tranche that closes stops being snapshotted, so its last interval
retires whatever was left, read from the account's disappearance from
its source's latest snapshot.

A mortgage the product does not hold has no balance to read, so its
instalments keep their whole amount and draw as interest: the
conservative direction, since it overstates what was consumed rather
than inventing a repayment. It is the second, quieter cost of not
holding the mortgage — the first being that the narrative rule which
places the payment fires on the words a feed prints (`MORTGAGE`,
`HYPOTHEK`, `HYPOTHEKARZINS`), so a feed that stops carrying them drops
its mortgage rows into `vehicles · Untracked accounts`, with nothing in
the statement to say that is what happened — which is what
`mortgage_transfer` (below) answers: a pin reaches `financing ·
Mortgage` on the verdict alone. A rule sees one row's text
and never the lender's balance: it names the class, and the statement
splits the amount. `cashflow_txn_nodes` still emits one row per
transaction, so the reconciliation memo is untouched; the split happens
in `cashflow_lines_base`, and the two shares sum to the row.

Both shares stay in **financing**. Putting interest in `operating_out`
would net one half of a payment against the section the other half sits
in, and a reader comparing years would see consumption jump on a
refinancing that changed nothing about the household. The leaves say
which is which without moving either.

### The seven new taxonomy values

[SPENDING.md](SPENDING.md) §2 and [INCOME.md](INCOME.md) §2 carry the
tables; the policy is here.

`debt_repayment` is the outflow mirror of `loan_proceeds` and what the
dropped `LOAN_PAYMENTS` primary was for, minus the interest share
nothing in the data splits out. Spending family, out of the spending
base. Without it a car or student loan paid to an untracked lender has
no honest home in either family and reads as uncategorised spending.

`retirement_transfer`, `education_transfer`, `health_transfer` and
`trust_transfer` are deltas of family **both**, in the manner of
`internal_transfer`: one row each, read from either side, out of both
bases. They exist for the crossing whose far side the product does not
hold. **The direction is the row's own**: a withdrawal placed
`retirement_transfer` is a contribution and a deposit placed the same
is a distribution.

`mortgage_transfer` is a delta of the same family **both**, and the
one that is not a crossing: it reaches `financing · Mortgage`, where
the interest/principal split applies, rather than the vehicles
section. It exists because that node had only two roads in, and both
read the far side — a far account of kind `mortgage`, or `far_class`,
which only the built-in tier may write. A servicer whose narrative is
its own legal entity cannot go in a tracked built-in, so an instalment
to one had no road at all and fell to `vehicles · Untracked accounts`.
`debt_repayment` stays the value for every other untracked lender: a
mortgage is split into interest and principal and a car loan is not.
A servicer with no balance series in gold has no principal to
apportion against, so its instalments draw whole as `Mortgage
interest`.

`deposit_transfer` is the fifth crossing and the one with no wrapper
behind it: a bank's own deposit product — a call deposit, a fixed-term
deposit, a notice account. The bank books every movement of one on the
account that FUNDS it and never lists the product beside it, so the
collector has no account to collect and the move reaches the resolution
with a single leg. Nothing can pair it, and drawn by the far-account
test it fell to `vehicles · Untracked accounts` — the node for a
destination nothing identifies, where this one is identified on every
row.

It is a verdict rather than a far account for a reason worth stating:
`far_class` lives on the spending overlay, which holds the outflow leg
and not the return. That road would have named the money going in and
left the money coming back anonymous. A verdict can be placed by either
family, so both legs carry it.

Two things it deliberately does not change. The move stays a **line**:
the money left the measured pool, so the statement has to say so, and
drawing it as internal netting would make the statement stop tying to
the balances it is drawn from. `deposits` is `untracked` made specific,
not made invisible. And the **interest** a deposit pays is untouched —
it is income, it arrives on its own row, and it is the one movement of
a deposit that is not a transfer.

One value per class rather than one `vehicle_transfer` for all four.
The alternative would need a rule to carry a second field saying which
pool it meant, which is a new shape in a config surface that today is a
pattern and a category — and money to the retirement pool and money to
the education pool are different decisions, read at different stages of
a life. This is the largest widening of the shared vocabulary since it
was vendored.

Because a rule only ever sees its own family's population, the
contribution side is a `spending.rules` entry and the distribution side
an `income.rules` entry, exactly as an own-account move is placed
today. **None of them is needed where the far account is tracked**: the
matcher's verdict already says everything, and it outranks a rule.
Giving needs no delta of its own — `GOVERNMENT_AND_NON_PROFIT_DONATIONS`
already lands there.

---

## 6. Netting

> A node is a net. The level a view groups by decides what nets, and
> the sign of that net decides the side.

Aggregating every sale as an inflow and every purchase as an outflow
makes two gross bands dominate any diagram for a household with
ordinary portfolio churn, and says nothing: a bond ladder rolling, a
rebalance between two equity funds, a coin traded for another coin are
not the household raising or deploying cash.

Both readings of investing are wanted, so the grain is a toggle and the
whole is the default. `--investing whole` nets the section and draws
one `Investments` node on the side the period's net puts it — the
answer to the question a reader opens with. `--investing class` nets
per asset class and shows the reallocation the whole nets away. Under
`whole` the Investments node has no leaves: a class on the other side
cannot be drawn as a child of a node on this one.

Financing and the vehicles net per class with no toggle, and cash is
one node by definition.

**The hub** is computed at the view's level, as the sum of the positive
nets there — which equals the sum of the negative ones, because the
identity above sums to zero once cash is a node. A finer level can
therefore have a **larger** hub than a coarser one: two leaves of
opposite sign inside one class net away at the class level and both
appear at the group level. That is not an inconsistency, it is what
netting means, and it is why `share` is defined against the hub of the
level being drawn.

One consequence of the universal side rule: a leaf whose net runs
opposite to its class is drawn **attached to the hub directly** rather
than as a child of a node on the other side, and its class's edge
carries only the leaves that stayed with it. Each stage still conserves
flow, and no node appears twice.

Netting is per period bucket: monthly for a monthly report, the whole
window for the diagram.

---

## 7. Configuration

```json
"cashflow": {
    "accounts": { "exclude": { "<source>": ["<account-id>"] } },
    "wrappers": { "529": "household", "trust_grantor": "trusts" }
}
```

One block, two fields, both optional.

`cashflow.accounts` is the pool, and the only account gate cashflow
applies. **It inherits neither family's scope**, and that is
deliberate: the two families keep their own scopes on purpose, and an
account one of them excludes is not thereby outside the household's
cash pool. The shape to hold in mind is an income scope dropping an
account whose funding leg cannot pair — inherited here, it would put
that account's withdrawals in the pool while its deposits vanished. Cashflow reads each family's verdicts, not its base, so the
families' scopes and base exclusions do not reach inside the pool.
Absent, every account is pooled.

**Exclusion only.** Every account is pooled by default, so an
`include` could fence nothing, and a knob that does nothing is worse
than one that is absent. The loader disallows unknown fields, so
writing one is an error rather than a silent no-op.

`cashflow.wrappers` moves a tax wrapper across the boundary. The value
is where a crossing lands: `household` (no crossing at all),
`retirement`, `education`, `health`, `trusts` or `giving`. Unlisted
wrappers keep the engine default; an unknown wrapper or destination
fails the load naming the entry. It is **per wrapper, not per account**:
an account a source mis-labelled is fixed with the existing per-account
`tax_wrapper` override, so that every consumer of the wrapper agrees
about whose money it is. The two knobs take effect on different
triggers — this block is re-stamped by every enrichment pass, while an
account override reaches the rows a load actually touches, so a reload
is what applies one to history.

Nothing else is configurable. Rules, pins and transfer overrides are
the families'; a verdict written there is what cashflow reads, and the
seven new values are placed through those surfaces.

One thing those surfaces now carry that only cashflow reads: an
optional `asset_class` on a `spending.rules[]` / `income.rules[]` entry
and in either pins ledger, admitted only beside `investment` or
`capital_return` and validated against the exposure set (TAXONOMY.md
§2) less `cash` and `other`. It says what the capital went INTO, for
the movement whose feed named no instrument — see §4. It is the one
exception to the paragraph above, and it is deliberately not in the
`cashflow` block: the verdict and the exposure are one statement about
one row, and splitting them across two config surfaces would let a
deployment carry half of it.

---

## 8. Reading it: `wealthdb cashflow <view>`

```
wealthdb cashflow <view> [FROM [TO]] [--period P] [--level L] [--investing G]
                         [-f FORMAT] [-C COLS] [-x CCY] [-p]
```

The families' idiom, unchanged. Five views:

| view | a row is | default columns |
|---|---|---|
| `summary` | a period bucket: the cash flow statement | period, operating_in, operating_out, operating, investing, financing, vehicles, net_cash_flow |
| `flows` | a (bucket, node) pair at `--level`, netted at that level | period, section, class, group, txn_count, inflow, outflow, net, share_% |
| `sankey` | an edge of the window's diagram | stage, source, target, value, share_% |
| `transactions` | a cashflow line, oldest first | silver_source, date, account, kind, section, class, group, name, currency, net_amount, value |
| `coverage` | an (account, currency, bucket): the ledger against the account's own balances | period, silver_source, account, currency, ledger, measured, gap, status |

**Three refusals, all usage errors rather than silent ignores.**
`sankey --period` would mean one edge list per bucket, which is a
loop's job; `sankey --level section` would draw a diagram with no inner
column; `coverage -x` would put a rate error on top of the one number
that view exists to make trustworthy. Ignoring any of them would hand
back a plausible answer to a question nobody asked. `transactions` DOES
ignore `--period`, as the families' transactions views do.

Behind `-C`: `section_id` / `class_id` / `group_id` on `flows` (the
dotted node keys), `source_id` / `target_id` / `section` on `sankey`,
and on `summary` the four memo columns — `yield`, `taxes`, `fees`,
`giving` — plus `savings_rate`.

`-p` redacts exactly as the families do: account ids, amounts, and
every name taken off a statement line. Section, class, group and the
shares stay legible, which is what makes the privacy twin of the
diagram **normalisation alone**: no node is ever a merchant, a payer,
an account or an instrument.

`wealthdb transactions` gains `cashflow_section`, `cashflow_class` and
`cashflow_group` behind `-C`, beside the spending and income trios, so
the one surface that shows a row from every side keeps doing so. They
read the **resolution** rather than the base, so a row on an account no
cashflow report charts still carries its node.

They are **blank** where the resolution reached no node at all: a
pool-internal move and a declined kind both land there, and the columns
say only that the statement does not draw the row. Which of the two it
was is `cashflow_txn_nodes`' `disposition`, one query below the
reports.

### Reconciliation

Two identities are **structural** and therefore guard arithmetic rather
than population: the four section figures sum to `net_cash_flow`, and a
bucket's `flows` rows — the Cash row included — sum to zero.

The check that can catch a wrong POPULATION is the memo on
`summary`, behind
`-C +cash_measured,+fx_effect,+cash_flow_measured,+unexplained`. The
cash section is computed, never measured, so an unpaired transfer or a
wire a bank books under a catch-all kind lands in the residual and
looks like cash saved or spent. The pool's balances, unlike the
residual, are **observed**.

- `cash_measured` — the pool's cash balances at the bucket's end minus
  at its start, each at its own boundary day's rate.
- `fx_effect` — the revaluation part of that change, in **two** terms:
  the opening balances revalued to the closing rate, and every
  movement in the bucket revalued from its own day's rate to the
  closing one. Without the second, a salary received mid-year and held
  to the year's end would masquerade as an error.
- `cash_flow_measured` — the flow the residual is taken against.
  **Not `net_cash_flow`**, and the difference is the point: the memo
  can only compare observed balances against the flow over the accounts
  and dates those balances cover, so this term counts the movements
  inside the pool and starts each account at its own first snapshot
  (the six narrowings below). Printed because without it the four
  columns do not close on their face and a reader doing the arithmetic
  finds a residual that is neither zero nor `unexplained`.
- `unexplained` — what is left. Near zero, **not** zero.

The four printed columns close exactly:

```
unexplained = cash_measured - fx_effect - cash_flow_measured
```

Six things narrow what the memo measures, and each is a way it would
otherwise be wrong about itself rather than about the statement:

1. pooled accounts only;
2. accounts that contribute transactions only — one loaded for
   balances alone has a real balance change and no lines;
3. accounts that have a balance history only, and never the synthetic
   per-portfolio `overlay` accounts;
4. each account is measured **from its own first snapshot**. Collectors
   onboard at different times: one begins, back-loads years of
   transaction history, and has balances only from the day it first
   ran. Blanking every bucket in which any account was unobserved
   blanks every year before the last collector was added;
5. the flow side is the same accounts over the same slice, because a
   diagnostic whose halves covered different populations would carry a
   structural term and stop being a diagnostic;
6. the flow side counts the movements the resolution calls
   **pool-internal** as well as its lines. The measured spine is
   narrower than the household: a wire to a pooled account whose
   balances are not collected leaves it, and so does a money-market
   fund bought with idle cash. Both move the spine and the statement
   rightly draws neither. What the flow side does NOT count is the rows
   the resolution declined — showing up in `unexplained` is exactly
   what the memo is for. Counting the pool-internal rows is
   load-bearing rather than merely harmless, and a pair whose two legs
   are one currency conversion is what makes it so: those legs do not
   offset each other in native terms, so a flow side that dropped them
   would put the whole conversion into `unexplained` on every bucket it
   touched. Included at their own amounts, they move the measured spine
   by exactly what the balances moved by.

A bucket reports blank where nothing is measurable, or where no rate
can value an amount at a boundary. It is a diagnostic with a stated
tolerance, never an input: `net_cash_flow` does not read it.

### Coverage: which accounts the statement can be trusted on

The memo above answers one question per period for the whole pool. When
it does not close it says so in one number and stops, and the account
behind that number is not in the statement.

`wealthdb cashflow coverage` is where it is: per account and per
period, the cash delta its transactions imply against the delta its own
balances show.

```
period  silver_source  account  currency      ledger    unsigned    measured         gap  status
2099    bank           Everyday CHF      -111111.11   999999.99  -22222.22  -88888.89  obscured
2099    bank           Mandate  USD        99999.99        0.00    1111.11   98888.88  measured
```

(Figures invented; the shapes are the two that matter. The first account
converts currencies, so the gap is smaller than the volume no sign could
be read from and nothing can be concluded. The second carries no
unsigned volume at all, so its gap is real.)

Three things separate it from the query anyone would write first, and
each is a way that query is wrong.

1. **It does not convert.** A per-account gap belongs in the account's
   own currency; carrying it to an output currency puts a rate error on
   top of the number the report exists to make trustworthy. `-x` is
   refused rather than ignored.
2. **It publishes its own blind spot.** Gold pins no canonical sign for
   six kinds — the FX family, corporate actions, the two catch-alls — so
   their amounts cannot be summed into a cash delta. Dropping them
   quietly makes an account that converts currencies look like an
   account with an enormous hole. The volume that had to be dropped is
   the `unsigned` column, and a gap no larger than it reads **obscured**:
   not clean, not damning, not answerable from what the adapters signed.
3. **It reports what it cannot measure.** An account with no balance
   history never enters a join-based version's output at all — absent
   reads as fine. Those rows are here as **unmeasurable**, and an
   account whose balance series begins inside the period reads
   **opening**, because the difference across that boundary is not a
   delta.

Read `status` first and sort by `gap` within `measured`. That ordering
puts at the top whichever account's balances moved most without flows
to explain it, which is how a feed that stopped covering an account
mid-window surfaces itself instead of waiting to be found.

### The gap to the family reports

Cashflow's income differs from `wealthdb income`'s total by exactly
four terms, each computed separately:

1. the **vehicles' own income** — a plan's dividends are not the
   household's cash flow;
2. the **reimbursements** cashflow keeps and income drops;
3. the **re-homed verdicts** — `capital_return` and `investment` to
   investing, `loan_proceeds` to financing;
4. any account the families' own **scopes** exclude but the pool keeps.

If the four do not close the gap, the resolution is wrong somewhere,
and that bridge is the cheapest place to see it.

---

## 9. The dashboard

**Cash Flow**, in the Spending and Income dashboards' shape, with a
privacy twin linked from the top row. Five pickers: time range, source,
a required currency, a section, and **Investing** — as a whole (the
default) or by asset class, bound to the diagram and the investing
trend.

**The Section picker is narrower than the other four**, and it is the
one picker the design did not ask for. It reaches the by-month charts
and `Largest flows`, and the headline figures and the diagram decline
it. A statement narrowed to one section is not a statement; a savings
rate or a yield share narrowed to one has lost a leg of its own ratio;
and a one-section diagram draws a `Cash savings` node that absorbs the
whole section rather than the residual it names. A by-month chart that is
already scoped to one section — Inflows, Outflows, Investing — goes
empty when a different section is picked, which is what a dashboard
filter means. The scope is enforced by withholding the filter's
template tag from the cards that decline it, so a card cannot drift
into the picker by being written later, and `test_provision.py` pins
it card by card.

**No account picker**, and that is the design rather than an omission.
The household boundary is what separates the household's cash flow from
its vehicles', and a picker that moved accounts in and out of the pool
would turn every crossing it split into an unexplained disappearance: a
wire between two of the household's own accounts is invisible only
while both are in the pool.

| row | cards |
|---|---|
| headlines | Operating in · Operating out · Net cash flow · Savings rate · Yield share |
| the diagram | Cash flow — the Sankey, full width, the window's four stages |
| the shape of the window | Cash flow statement by month: signed stacked bars per section |
| the two sides | Inflows by class by month · Outflows by class by month |
| the swing sections | Investing by month · Financing and vehicles by month |
| the lines | Largest flows: the fifty largest lines of the window |

Every tile is a **native query over `web_cashflow`**, the line-grain
serving view. The Sankey card computes its nets, its sides and its hub
**in-query**: a node's side is the sign of its net over the FILTERED
window, so a pre-netted table would be netted over the wrong window the
moment a reader moved a picker.

The twin is **normalisation, not redaction** — with the hub at 100 there
is nothing left to hide, because no node is ever a merchant, a payer, an
account or an instrument. One card is the exception and drops columns
the way every other twin card does: `Largest flows` names the account a
line moved through and the payer or merchant behind it, so the twin
ranks instead and projects neither.

---

## 10. Storage and lifecycle

Cashflow has no store of its own. What it adds to gold is three columns
on the spending overlay and two stamped tables:

| table / column | grain | survives `reset`? | carried by `reload -a`? |
|---|---|---|---|
| `spend_txn_enrichment.far_*` | per transaction | no | n/a — re-derived |
| `cashflow_wrapper_sides` | one row per tax wrapper | yes | re-stamped from config |
| `cashflow_account_scope` | config, stamped into gold | yes | re-stamped from config |

Nothing is stored for the memo: it reads `cash_balances` and the
resolution at query time.

The wrapper table is stamped **whole** — every value of the enum,
defaults included — which is what keeps `canonical.DefaultWrapperSide`
the single place a wrapper's side is decided. Stamping only the
overrides would put the defaults in SQL a second time, where a wrapper
added for a new jurisdiction would take two edits to reach the
statement.

`wealthdb load` prints a `cashflow:` block: the boundary stamped, how
many own-account moves carry a far account and how many of those the
source stated itself, what the matcher asserted on a shared reference
and on a described counter leg, and — loudly — how many pooled accounts
have no tax wrapper. The far accounts are rows, one per leg; the
matcher's figures are pairs, each writing two of them.

### The diagram's node order is the renderer's, not ours

Within a column the BI layer's Sankey orders nodes by a force
relaxation: each node is pulled toward the weighted centre of its
neighbours and the column is re-sorted on position, for a fixed number
of iterations. It converges to a fixed point that ignores the input —
reversing the entire edge list produces a byte-identical layout — and
the BI layer exposes no iteration count and no node-sort setting.

So a class's leaves are NOT drawn together. A large leaf settles high
and can land among another class's leaves, which reads as though it
belonged to that class. Where it lands depends on the data: over one
window a mortgage's amortisation sits second in the column, over
another it sits twenty-fourth.

Two things follow, and both were measured rather than assumed.

The edge list's own ORDER BY groups a class's leaves together, which
makes the rows readable — and does nothing for the diagram. The comment
on it says so.

And flattening a class into its leaves, so it has none to interleave,
is not a fix. It is tidier by the count (four to six order changes down
the column against eight to twelve) but only because it removes a
level: the same interleaving reappears between whichever classes still
have leaves, so a mortgage's amortisation stops landing among the
consumption leaves and a gift starts landing among the fees. Trading a
node of the vocabulary for that is a fix aimed at one window of one
deployment. If the BI layer ever exposes the layout's iteration count,
this becomes a visualisation setting and none of it is a data question.

### After an upgrade, the statement is wrong until a load has run

Applying the cashflow migrations creates the two boundary tables and
the three far-account columns **empty**. Nothing backfills them: they
are written by the enrichment pass, which runs inside `wealthdb load`
and `reload`. Between the two the statement is not merely incomplete,
it is wrong in a way that reads like a finding:

- `cashflow_wrapper_sides` is empty, so every wrapper reads as
  household and the pool is **every account**, vehicles included;
- no enrichment row carries a far account, so the far-account ladder
  (§4) falls to its last arm and **every matched own-account move**
  resolves to `vehicles · Untracked accounts`.

The dashboard then shows one enormous `Untracked accounts` node that
looks like a data problem and is not. `wealthdb load -a` ends the
state, and the load that does prints a line saying so — once, because
after it the counters alone tell the story.

A binary carrying unapplied migrations applies them **on any
read-write open of gold**, which includes `web-materialize` and every
command that is not explicitly read-only. So the state above can begin
without anyone running a migration on purpose.

`wealthdb status -v` prints a `cashflow:` block with three counters,
each naming a way the statement can be quietly wrong:

- **excluded by kind** on pooled accounts — the FX family, corporate
  actions, unmatched transfers, unpaired card payments and the
  catch-alls;
- **no tax wrapper** on pooled accounts — the boundary's coverage gap;
- **no far account** — rule-placed moves landing in `Untracked
  accounts`, which are also the ones collecting that account would
  resolve.

---

## 11. Open follow-ups

1. **A per-vehicle statement.** The same statement with the edge drawn
   around one plan or one trust. Every mechanism here applies
   unchanged; only the boundary moves. It is where a payroll-funded
   plan's growth, and giving routed through a vehicle, become visible.
2. **A sign pin for the unsigned kinds.** A per-adapter pin, recorded
   where the canonical signs live, is what would let the cash leg of a
   merger or a rights issue join investing.
3. **A dated wrapper override.** The boundary is time-invariant (§3); an
   effective-from date is the fix.
4. **Memo pairs for what the boundary removes** — the vehicles' own
   income and spending, and the tax they withheld before the household
   saw the money.
5. **Platform custody deposits**, **reimbursement netting** and the
   **cross-currency own-account moves no assertion reaches**: the two
   families' standing follow-ups, each of which shows up here too. The
   third narrowed twice — first when the matcher learned to pair on the
   transaction number a source stamps on both halves, then when it
   learned to read the counter leg a source describes on one of them.
   What is left is the movement whose source does neither, and the card
   bill whose two feeds share no id space.
6. **Net cash flow on the Wealth Overview**, once the feature has been
   read for a while.
