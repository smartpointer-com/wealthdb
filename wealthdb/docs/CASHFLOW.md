# Cashflow

The household's cash flow statement, and the Sankey that draws it. It
shows where cash came from and where it went over a window, across the
household's accounts. Moves inside the household are removed. Buying
and selling show as one net movement, not two gross ones.

It also projects the months ahead: the income the holdings are
expected to pay against the outflows of the months before (§12).

This document is a **delta against [SPENDING.md](SPENDING.md) and
[INCOME.md](INCOME.md)**. Cashflow is the third reading of one engine.
It shares one enrichment pass, one precedence lattice, one
internal-transfer matcher, one signature normaliser and one taxonomy.
Where a mechanism is shared, this file points at the section that
explains it.

What cashflow adds is small:

- **Almost no collector work.** The resolution reads neither bronze nor
  silver. The one exception is the far account. A counter account that
  a bank states on its own row is a fact only a collector can carry, so
  the adapters and collectors that can state one do (§4).
- **No new tier and no model.** The precedence lattice is unchanged,
  `categorize` gains nothing, and the backlog is the two families'.
- **No new store.** Nothing is bought from a model, so `reload -a`
  carries nothing new across.
- **The centre of gravity is SQL.** One resolution macro maps each line
  to a node. Everything above it is reports over that macro.

---

## 1. Scope: both directions, and the movements neither family claims

Income reads what arrived, and spending reads what left. Cashflow reads
both, adds the movements neither family books, and closes the
arithmetic:

```
operating + investing + financing + vehicles = change in cash
```

It answers the question a household-balance diagram answers: what came
in, what went out, and what was left.

| section | what crosses the edge | sign |
|---|---|---|
| `operating_in` | the income verdicts, plus the reimbursements income excludes that link to no outflow | + (net of reversals) |
| `operating_out` | the spending verdicts, plus moves into a charitable or custodial vehicle | − (net of refunds and linked reimbursements) |
| `investing` | buys and sells; private capital called and returned; capital deployed to or returned from a destination the product does not track | net |
| `financing` | what the mortgage rule places, an own-account move to a mortgage account (matched or stated), `mortgage_transfer`, `loan_proceeds`, `debt_repayment` | net |
| `vehicles` | moves to or from a retirement plan, an education or health account, a non-grantor trust, or a bank's own deposit product; and the own-account moves nothing paired | net |
| `cash` | the residual: the four summed, seen from the pool's side | computed |

`summary` prints the four statement sections, with operating as the
signed difference of its two halves. `flows` and `sankey` carry the six
values above, because a node has to know which half of operating it
belongs to.

**The sign rule:** positive is cash arriving in the pool, and negative
is cash leaving it. Gold's canonical signs already mean that for every
kind the statement admits, the card kinds included. A card's balance is
carried as negative cash, so a purchase on it drives the pool down like
a purchase on a deposit account. Nothing in the resolution negates
anything.

What is deliberately **not** here:

- **A P&L.** Investing shows cash moved, not gain realised. Selling at
  a loss is an inflow. Gains are [GAINS.md](GAINS.md)'s.
- **Returns.** "External to the thing being measured" and "across the
  household's cash edge" are different edges. They agree on most rows
  and may disagree on the rest.
- **Tax reporting.** Withholding a vehicle deducted before the
  household saw the money is on no node at all (§3).
- **The vehicles' own statements.** Giving routed through a vehicle,
  such as a plan distribution paid straight to a charity, is that
  vehicle's, not the household's.
- **Accrual, a budget, a forecast, or gross payroll.** Income is what
  arrived, as the income family decided.

---

## 2. The pool

Cashflow measures movement across the edge of the household's **cash
pool**: cash and cash equivalents on the household's accounts. That
covers:

- deposit balances, brokerage cash and exchange fiat;
- a card's owed balance, which the product carries as negative cash;
- instruments whose asset class is `cash`, such as money-market funds
  and time deposits.

Two things draw the edge:

- **What the balance is.** A security is not cash, so buying one is
  cash leaving the pool. A money-market fund bought with idle brokerage
  cash is cash becoming cash, and the diagram does not show it.
- **Whose the account is.** It is the household's, or a vehicle's
  beyond the boundary of §3.

The pool is exactly `cashflow_pool_accounts()`: the accounts not
excluded by `cashflow.accounts` whose tax wrapper is on the household
side. Every rule reads it rather than re-deriving "household" per rule.
An account excluded from the pool is treated like an account the
product does not hold. A move to it is a crossing, not an invisible
internal step. That keeps the scope from silently inflating the
residual.

---

## 3. The household boundary

A household does not count its retirement plan, its education accounts,
its health savings account, its donor-advised fund or a trust it funded
as itself, even when it holds every login. The boundary is drawn by
**tax wrapper**: the `defaultWrapperSides` table, which
`canonical.WrapperBoundaries` reads. A deployment can override it per
wrapper (§7).

Two questions decide where a wrapper sits. They are asked in order:

1. **Is the money still the household's?** If not, the transfer is
   complete and irrevocable, and the crossing is
   `operating_out · Giving`.
2. **Is it earmarked** for a purpose or a life stage the household does
   not treat as current money? If so, the crossing is a `vehicles`
   move.
3. Otherwise it is the household, and a move is pool-internal.

**Reversibility is not the test.** A retirement account can be drawn on
at any time, with a penalty, and nobody counts it as this month's
money. An education plan and a health savings account are the same.
The earmark separates them from a current account, not the lock.

| tax wrapper | household? | a crossing lands in |
|---|---|---|
| `taxable_personal` `taxable_joint` `trust_grantor` `other`, and unset | yes | nothing: it is pool-internal |
| the seven US retirement wrappers, `pillar_2` `vested_benefits` `pillar_3a` | no, earmarked | `vehicles · Retirement savings` |
| `529` `coverdell_esa` | no, earmarked | `vehicles · Education savings` |
| `hsa` | no, earmarked | `vehicles · Health` |
| `trust_non_grantor` | no, a separate taxpayer | `vehicles · Trusts` |
| `charitable` `trust_charitable` `foundation` `custodial_utma` `custodial_ugma` | no, given away | out: `operating_out · Giving`; in: `operating_in · Other receipts` |

An education plan and a custodial account for the same child land on
**different sides**. The plan's owner can revoke it and take the money
back, so it is the household's capital in an earmarked pool. A
custodial account is legally the minor's the moment it is funded, so
funding it is a gift.

`other` is on the household side because the product cannot know
better. The enum covers two jurisdictions. A pension, insurance or
company wrapper from a third can only be `other`. The remedy is a new
enum value. The per-wrapper override cannot tell two `other` accounts
apart.

**An unset wrapper reads as household.** Gold applies the same default
everywhere else. It is the safe direction, because nothing is silently
removed from the statement. It has a cost. An adapter that leaves the
column unset puts a retirement or health account inside the pool. Its
own trades then become household investing, and its contributions
become invisible. No reconciliation can see that, because the crossing
is **absent** rather than wrong. So the load summary and `status -v`
both count pooled accounts with no wrapper (§10).

### What follows from the boundary

- **A vehicle's accounts contribute no lines.** Its dividends, trades
  and fees are not the household's cash flow. They stay in the income
  and spending reports, which count the holder's accounts. So the
  totals here are smaller than those reports' by **four** terms (§8).
- **A crossing is one line, on the household's side.** A contribution
  is the household account's outgoing leg, sorted by the far account.
  The plan's incoming leg is the vehicle's and is not a line. So the
  crossing reads the same whether or not the plan is collected. Only
  how it was placed differs.
- **Spending paid from a vehicle is not on the statement; spending
  reimbursed by one is.** A medical bill the health account pays
  directly draws nothing. The same bill paid from a current account and
  then reimbursed is two real lines.
- **A move between two vehicles is invisible.** A plan distribution
  paid straight to a charity is giving the household would claim, and
  the statement does not show it. The Giving node is the household's
  giving, not the family's.
- **Tax withheld inside a vehicle is not on the statement.** The Taxes
  node is the tax the household paid from its own accounts. In a
  drawdown year that is less than the tax it bore.
- **Payroll-funded contributions are invisible**, for the reason income
  is net as booked. A deduction the household never received is on no
  account the product holds.
- **The wrapper decides, not the kind.** A brokerage account inside a
  retirement wrapper is a vehicle. So is a cash account inside a
  non-grantor trust.
- **The boundary is time-invariant.** A wrapper is one column with no
  validity range. A grantor trust that becomes non-grantor changes every
  year at once.

---

## 4. The resolution

`cashflow_txn_nodes(from, to)` maps every transaction to a node,
`section.class.group`. It also gives a `disposition` saying whether the
row is a line at all:

- `line`: drawn on the statement;
- `internal`: pool-internal, and invisible;
- `excluded`: declined, and counted.

`cashflow_lines_base(from, to)` is that macro restricted to the pool
and to the lines.

The resolution is **SQL, not Go**. The dashboard needs the same node
assignment the CLI uses, computed after its pickers apply. A Go-side
resolution would have to be re-implemented in the serving view, and the
two would drift. (The returns engine is in Go because its maths cannot
be written in SQL. Nothing here is like that.)

**Cashflow reads each family's resolution macro, never its
`*_lines_base`.** The verdicts it needs most are exactly the ones a
base excludes: the crossings, `investment`, `capital_return`,
`loan_proceeds`, `debt_repayment` and the `reimbursement` no outflow
answers.

### Where each row lands

The first matching step places a row:

1. **The six unsigned kinds are excluded, whatever a verdict says.**
   They are `fx`, `fx_forward`, `fx_swap`, `corporate_action`, `journal`
   and `other`. Their direction is a per-source convention, so a
   section that reads direction cannot admit them. Admitting the FX
   family or corporate actions needs a per-adapter sign pin first. The
   two catch-alls are excluded by both families too.
2. **`internal_transfer`** is placed by the far account, whatever the
   kind (see *Own-account moves* below).
3. **`mortgage_transfer`** goes to `financing · Mortgage`.
4. **A crossing delta** goes to `vehicles`: `retirement_transfer`,
   `education_transfer`, `health_transfer`, `trust_transfer` and
   `deposit_transfer` (`vehicles · Bank deposits`).
5. **`loan_proceeds` and `debt_repayment`** go to `financing · Loans`.
6. **`investment` and `capital_return`** go to `investing`.
7. **`distribution`** goes to `investing`. It floors to
   `capital_return`, so step 6 usually places it first. A rule or a pin
   that promotes it to an income type moves it to operating in. A
   distribution from a `cash`-class instrument is pool-internal.
8. **`buy`, `sell` and `contribution`** go to `investing`, classed by
   the instrument's asset class. A `cash`-class instrument is
   pool-internal.
9. **The income kinds** go to operating in, by resolved income type:
   `dividend`, `coupon`, `staking`, `capital_gain`, `reward`, `deposit`
   and positive `interest`. A `deposit` placed `reimbursement` that
   links to no outflow lands here too. Income excludes it, but it is
   cash that arrived. One that links ([SPENDING.md](SPENDING.md) §3,
   *Reimbursements*) is placed ahead of this step, on operating out in
   its outflow's category, netted as a refund is.
10. **The spending kinds** go to operating out, by resolved spend
    category: `purchase`, `refund`, `fee`, `tax`, `withdrawal` and
    negative `interest`.
11. **Everything else is excluded and counted.** In practice that is an
    unpaired `card_payment` or an unmatched `transfer_in` /
    `transfer_out`.

The two cases in step 11 are excluded for these reasons:

- **An unpaired card bill** is in neither family's population, so no
  tier ever saw it. It also has a known false shape: a cross-currency
  pair that no road joins. The amount pass partitions by currency, and
  a card ledger mints its own ids, so no shared reference reaches it.
  The provider tier already files its bank leg as card spend. Counting
  the bill as a receipt would print a phantom inflow and double it.
- **An unmatched transfer** is an in-kind ledger leg or a source's own
  tagging. It is counted rather than guessed at.

**Excluded means counted.** A counter nobody reads is how a silent hole
starts, so `status -v` reports the declined rows on pooled accounts
(§10). Pool-internal rows are NOT counted there. They are movements the
statement deliberately does not draw, and a permanently large counter
is an unreadable one.

### Own-account moves are sorted by the far account

An `internal_transfer` verdict says the money stayed the holder's.
Cashflow needs one more fact: whether it stayed in the **pool**. The far
account says so, and it arrives by two roads.

**The matcher's road** is a pairing. Two legs the product collected are
joined on one of:

- amount and day;
- a reference the source stamped on both halves;
- the other leg the source described on one of them.

Each leg then names the other's account (`far_silver_source_id`,
`far_account_external_id`, migration 0079).

The joins are not interchangeable. A pairing on amount is an inference,
so its two legs agree in currency and nearly in size. A pairing on a
reference is the source asserting an identity, so its legs may differ
in **both**. A conversion between two of the holder's own accounts has
exactly that shape, and the amount join can never reach it (SPENDING.md
§3, *The reference road*). Nothing here nets a pair. Each leg is placed
on its own figure, by its own far account, so the difference costs the
statement nothing. Where both accounts are in the pool, both legs are
invisible.

**The source's road** is the bank stating the counter account outright.
Where the product does not collect the far side at all, there is one
row. A wire to an account of the holder's own whose transactions no
feed reports is the typical case. The matcher needs two rows, and only
the narrative knows. An adapter or collector whose source states the
counter account puts it in the row's payload as `counter_account`. The
UBS adapter reads it from whichever of its feeds states it. The svb
collector reads it from a deposit-ledger row's reference to a loan.

The pass resolves a stated counter account against the accounts gold
already holds:

- it searches the row's own source first;
- it takes an account from another source only when no account of the
  row's own source matches and exactly one account elsewhere does;
- it refuses a row that names its own account;
- a third party's account resolves to nothing. That is the common case
  and the right answer.

That road answers **where the money went and nothing else**. What the
movement was stays the tiers' question. A stated counter account places
no verdict, so a row still needs a matcher, a rule or a pin to be
called an own-account move. Where neither road knows a far account, the
rules that place the verdict can carry a class instead (`far_class`).

The ladder is ordered, and the first match wins. **The wrapper is
asked before the kind**: a mortgage on a trust-owned property and a
card inside a foundation exist, and the wrapper says whose money moved.

1. The far account's wrapper is on the **vehicle** side → that
   wrapper's class, in the leg's own direction. An outgoing leg to a
   retirement plan is a contribution, an incoming leg from one is a
   distribution, and both are `vehicles · Retirement savings`.
2. The far account's wrapper is a **giving** one. An outgoing leg is
   `operating_out · Giving`: the contribution is irrevocable, and the
   vehicle's later grants are its own. **An incoming leg is
   `operating_in · Other receipts`.** A charitable remainder trust's
   annuity, a foundation reimbursing a trustee, and a custodial account
   drawn for the minor's costs are cash arriving. None is negative
   giving.
3. The far account is kind `mortgage` → `financing · Mortgage`, either
   direction. A drawdown is an inflow.
4. The far account is **in the pool** → invisible.
5. The rule recorded `far_class` `mortgage` (the built-in mortgage rule
   does) → `financing · Mortgage`.
6. Anything else → `vehicles · Unpaired transfers`. At group grain the
   node splits by what is missing:
   - **No far account** (`unnamed`): nothing paired the row.
   - **Fenced-out account** (`out_of_pool`): the far account is one
     gold holds, which `cashflow.accounts` excluded from the pool.

Step 6 closes a hole the rule tier opens. A config rule exists to place
a wire to the holder's own account at a bank the product does not
track. That money **leaves the pool**. Drawn as invisible, it would
vanish into the residual. Drawn as `Unpaired transfers`, it is visible
as what it is.

The statement assumes a **closed world**: an account it does not hold
is outside the household unless declared. Under that assumption the
node is a finding, not a residual. A row there is one of:

- a counter-leg gold should hold and does not: an account not yet
  collected, or a pair the matcher missed;
- a declaration not yet written.

The remedy is plain from the chart: collect that account, **declare**
it, or find the missing leg. A fenced-out row is the exception. It is a
setting, and is named as one so a reader can tell it from a finding
without opening the config.

**A declared account is a far account the holder states.**
`declared_accounts` in the config names an account the household owns
at an institution the product does not collect. Each entry carries the
kind and the wrapper the account would have if collected. The load
writes it into the accounts dimension under the reserved source
`declared`. A rule's `far` names it as the other side of the move the
rule places. The ladder above then reads it like any collected far
account:

- a household wrapper puts it in the pool, and the move is invisible
  (step 4);
- a retirement wrapper draws the move as `vehicles · Retirement
  savings` (step 1).

A declaration is how the holder opens the closed world, one account at
a time. A move to an undeclared, untracked account is income or
spending. Nothing the product measures can contradict a declaration, so
the load prints how many sit inside the pool. The inbound leg is the
income family's verdict. Its far account is written onto the spending
overlay, the one overlay with far columns, so both directions place
alike.

**The investing class has three roads, in order:**

1. the trade's own exposure (migrations 0097 and 0100). A feed that
   cannot name the instrument can still name what it traded;
2. the instrument's asset class;
3. beside an `investment` or `capital_return` verdict, the
   `asset_class` that a config rule or a pins-ledger row carries (§7,
   migration 0102).

A row that names an instrument with no dimension row goes to `other`
before the third road is read. That node keeps a missing dimension row
visible. A stated exposure read any earlier would hide the hole behind
a plausible class. A stated `cash` is ignored, because that class's
label is the statement's own residual node. Where none of the three
roads says anything, the row is `elsewhere`.

The group stays the verdict on those rows. The class is what the money
went into, and the group is what the money did. A house bought and a
REIT traded share a class node and separate at group grain.

This ladder runs for `internal_transfer` only, so it is not the only
road to the nodes it reaches. `mortgage_transfer` (§5) resolves to
`financing · Mortgage` on the verdict alone, before any far-account
test. Both ladder steps that reach that node read a far side, and a
servicer the product holds no account for has none.

---

## 5. The vocabulary

Three levels, whose leaves are the two families' own values:

- **section**: the six values of §1.
- **class**: the diagram's inner nodes.
- **group**: the diagram's leaves.

A node's identity is **the whole key**, `section.class.group`. Three
values of the shared vocabulary exist on both sides of the household:
`gift`, `other` and the unplaced row. Keying a node by its leaf alone
would merge a gift given with a gift received in one edge list.

| section | classes |
|---|---|
| operating in | `earnings` · `yield` · `benefits` · `other_receipts` · `(uncategorized)` |
| operating out | `consumption` · `fees` · `taxes` · `giving` · `(uncategorized)` |
| investing | the exposure from the first of the three roads of §4 that names one; every value but `cash`. `other` where a row names an instrument whose class is missing; `elsewhere` where no road says anything. All fold into `investments` under `--investing whole` |
| financing | `mortgage` · `loans` |
| vehicles | `retirement` · `education` · `health` · `trusts` · `deposits` · `unpaired` (grouped `unnamed` · `out_of_pool`) |
| cash | `cash`, drawn as **Cash savings** |

The operating-in classes sort the income values, vendored and extension
alike:

| class | income values |
|---|---|
| `earnings` | `INCOME_SALARY`, `INCOME_GIG_ECONOMY`, `INCOME_CONTRACTOR` |
| `yield` | `INCOME_DIVIDENDS`, `INCOME_INTEREST_EARNED`, `INCOME_DISTRIBUTIONS`, `INCOME_STAKING`, `INCOME_RENTAL`, `INCOME_ROYALTIES`, `INCOME_ENERGY_FEED_IN` |
| `benefits` | `INCOME_RETIREMENT_PENSION`, `INCOME_GOVERNMENT_BENEFITS`, `INCOME_UNEMPLOYMENT`, `INCOME_CHILD_SUPPORT`, `INCOME_ALIMONY`, `INCOME_LONG_TERM_DISABILITY`, `INCOME_MILITARY` |
| `other_receipts` | `INCOME_TAX_REFUND`, `INCOME_REWARDS`, `INCOME_INSURANCE_PAYOUT`, `INCOME_OTHER`, and the receipt deltas: `gift`, `inheritance`, `cash_deposit`, `reimbursement`, `other` |

Yield is what the household's assets produce without its labour, so
rental income and royalties sit beside the dividends. A card credit or
a referral bonus is a receipt, not income from wealth. The lists live
in `cashflow_txn_nodes`. An income value they do not name lands in
`other_receipts`.

**Four lifts and no new tier.** Yield is lifted out of income, and
fees, taxes and giving out of spending. The families' primaries cannot
do this:

- income has one vendored primary;
- taxes hide in a government-and-non-profit bucket, beside donations
  and a passport renewal;
- fees are worth a line of their own: the cost of being invested
  belongs beside the tax, not inside a catch-all.

The uncategorised class is its own node on each side, not a leaf inside
Consumption. The families made the backlog a visible label on purpose.

**`consumption` reads "Consumption", not "Spending".** The class is the
spending family's categories minus the three lifts. Its figure differs
from the spending report's by construction (§8). One word for two
figures a reader is meant to compare is a trap. For the same reason the
summary's operating columns are `operating_in` and `operating_out`, not
`income` and `spending`.

### The leaves

The leaves are the families' own values, at the level each vocabulary
makes readable. The two differ:

- **operating in**: the income **detailed** value. Income has one
  vendored primary, so a primary-level leaf would fold every earned and
  yielded type into `INCOME`.
- **operating out**: the spending **primary**. That vocabulary has
  nearly a hundred detailed values, which is too many leaves for a
  diagram. The detailed value is a column away, behind `-C +detailed`
  on the transactions view.
- **taxes, fees and giving** keep the detailed value. Each class was
  lifted out of spending for the distinction inside it: a tax assessed
  against one withheld at source, a fee for banking against a fee for
  investing, a donation against a gift. A class whose one leaf repeats
  its own name is a self-edge.
- A **delta** is primary-level, so `card_spend`, `cash_withdrawal` and
  `other` are their own leaves either way.
- **One detailed value is promoted**: `GENERAL_SERVICES_EDUCATION`
  (migration 0088). `GENERAL_SERVICES` is a catch-all rather than a
  category, and school fees are a bigger line in most households than
  several primaries that get an edge. The criterion is a property of
  the vendored vocabulary, not of any deployment's data: a detailed
  value whose primary does not describe it. Its siblings under the
  catch-all still group by primary.

The vehicle classes are named for the act, not the subject (migration
0087): **Education savings** and **Retirement savings**, not
`Education` and `Retirement`. Each bare word names two things: a 529
contribution and a tuition payment, a plan contribution and a pension.
Those sit in different sections but would read as one node on a
diagram. `Health` and `Trusts` keep their names. "Health savings" is
right for an HSA and wrong the day the class holds anything else, and
nobody says "Trust savings".

Seven leaves are cashflow's own, where no family value says the right
thing:

- `trades` and `private_capital`, on the investing classes;
- `vehicle_giving` and `vehicle_receipt`, for a crossing to or from a
  giving vehicle. The verdict there is `internal_transfer`, which names
  a movement, not a kind of giving or receipt;
- `mortgage_interest`, `mortgage_amortization` and `mortgage_drawdown`.
  `cashflow_lines_base` mints these from direction and the derived
  split. No family value names half a row, or money borrowed against an
  account the product holds.

On the vehicles and on cash the class **is** the leaf, because nothing
finer exists. The diagram attaches them to the hub, and their leaf level
shows in `flows --level group`, not in the Sankey. A class with no
leaves enters the hub in its own right. A class with leaves enters as
their sum, and the leaves draw below it.

**Investing is held out of that stage**, at either grain. Its classes
do have leaves, which `flows --level group` shows: `trades`,
`private_capital`, and the verdicts `investment` and `capital_return`.
But a leaf stage there would draw a trade against a capital call, which
is a different question from the one the diagram asks. So investing's
classes enter the hub in their own right.

Financing has leaves on both classes. `loans` splits into
`loan_proceeds` and `debt_repayment`: money borrowed against money
repaid, the distinction the class exists to carry.

`mortgage` has three leaves, decided by direction first. An instalment
is an outflow and splits into **`Mortgage interest`** and **`Mortgage
amortization`**. They are not the same kind of thing:

- interest is consumed. It buys the use of the money and is gone;
- principal is not spending. It moves value from one side of the
  balance sheet to the other, and the household is no poorer.

Summed into one node, they overstate what was consumed by whatever was
repaid. In a period with an extraordinary repayment, that is most of
the node.

Money running the other way is **`Mortgage drawdown`**: a tranche
drawn, a line increased. It is a leaf, not the bare class, for a
structural reason. A leaf-stage section draws a row through a leaf. A
row whose group repeated its class would be dropped by the self-edge
rule, and dropped again because its class has other leaves. It would
reach the diagram nowhere while the residual still counted it. Where it
draws follows the ordinary rule that a leaf running against its class's
net detaches. In a year of repayments it attaches to the hub directly.
In a year that borrowed more than it repaid, the repayment leaves
detach instead.

**The split is derived, and it has to be.** No bank prints the share,
and the narrative cannot yield it:

- a tranche may book its scheduled amortisation in the same row as its
  interest, under one booking type;
- a closing may book the principal and the final interest as two rows
  identical in every field.

So the rule reads neither. Whatever a period retired of the mortgage's
own outstanding balance was principal. The rest of what was paid into
it that period was interest. Within a period the principal goes to the
largest instalment first, capped at its face value. That is the order
the two really occur in on a closing. The balance is **observed**: the
lender's own figure, carried as the mortgage account's position. So
this estimates the split, not the debt.

A snapshot is the balance **before** that day's instalment posts. An
interval runs from one observation up to, but not including, the next.
A tranche that closes stops being snapshotted. Its last interval
retires whatever was left.

A mortgage's end is the ending every reader takes ([DESIGN.md](DESIGN.md)
§10.7), so the cash flow and holdings agree on it. Where its last
observation ends, the mortgage reads as repaid, and it retires from the
run that left it out. A mortgage observed on one day only takes a month
as its cadence. So a new mortgage stands through its first month. One a
single statement shows reads as repaid once its source runs more than
45 days past that statement. A mortgage reported in a run of its own
has no company, so it ends only once its source runs a year past it.

While a mortgage stands, its last interval retires nothing. Its
instalments there draw as interest until its next observation loads.
So a mortgage reported monthly, beside a custody feed that runs
nightly, stands between its own observations. The rule compares
instants, not days. A mortgage observed in its source's newest
snapshot stands, whatever the hour of that snapshot. A run that
writes only cash balances moves no clock here.

A mortgage the product does not hold has no balance to read. Its
instalments keep their whole amount and draw as interest. That is the
conservative direction: it overstates what was consumed rather than
inventing a repayment. Not holding the mortgage costs more:

- the built-in rule that places the payment fires on words a feed
  prints (`MORTGAGE`, `HYPOTHEK`, `HYPOTHEKARZINS`, and the phrases
  `HOME LOAN` and `MORTGAGE PAYMENT`);
- a feed that stops printing them drops its mortgage rows into
  `vehicles · Unpaired transfers`, and nothing in the statement says so.

`mortgage_transfer` (below) answers that: a pin reaches
`financing · Mortgage` on the verdict alone. A rule sees one row's text
and never the lender's balance. It names the class, and the statement
splits the amount. `cashflow_txn_nodes` still emits one row per
transaction, so the reconciliation memo is untouched. The split happens
in `cashflow_lines_base`, and the two shares sum to the row.

Both shares stay in **financing**. Interest in `operating_out` would
net one half of a payment against the section the other half sits in.
A reader comparing years would see consumption jump on a refinancing
that changed nothing about the household. The leaves say which is which
without moving either.

### The seven values cashflow adds to the taxonomy

[SPENDING.md](SPENDING.md) §2 and [INCOME.md](INCOME.md) §2 carry the
tables. The policy is here.

`debt_repayment` is the outflow mirror of `loan_proceeds`. It covers
what Plaid's `LOAN_PAYMENTS` primary would, minus the interest share
nothing in the data splits out. It is spending family and out of the
spending base. Without it, a car or student loan paid to an untracked
lender has no honest home in either family and reads as uncategorised
spending.

`retirement_transfer`, `education_transfer`, `health_transfer` and
`trust_transfer` are deltas of family **both**, like
`internal_transfer`. Each is one row, read from either side, and out of
both bases. They exist for a crossing whose far side the product does
not hold. **The direction is the row's own.** A withdrawal placed
`retirement_transfer` is a contribution, and a deposit placed the same
is a distribution.

`mortgage_transfer` is also family **both**, but it is not a crossing.
It reaches `financing · Mortgage`, where the interest/principal split
applies, not the vehicles section. Without it that node has two roads
in, and both read the far side: a far account of kind `mortgage`, or
`far_class`, which only the built-in tier writes. A servicer whose
narrative is its own legal entity cannot go in a tracked built-in. So
an instalment to one has no other road and would fall to
`vehicles · Unpaired transfers`. `debt_repayment` stays the value for
every other untracked lender, because a mortgage is split into interest
and principal and a car loan is not. A servicer with no balance series
in gold has no principal to apportion against, so its instalments draw
whole as `Mortgage interest`.

`deposit_transfer` is the fifth crossing, and the one with no wrapper
behind it. It covers a bank's own deposit product: a call deposit, a
fixed-term deposit, a notice account. The bank books every movement of
one on the account that funds it and never lists the product beside
it. So the collector has no account to collect, and the move reaches
the resolution with a single leg. Nothing can pair it. Without the
value, the far-account ladder would draw it as `vehicles · Unpaired
transfers`, the node for a destination nothing identifies. This
destination is identified on every row.

It is a verdict rather than a far account for a reason. `far_class`
lives on the spending overlay, which holds the outflow leg and not the
return. That road would name the money going in and leave the money
coming back anonymous. A verdict can be placed by either family, so
both legs carry it.

Two things it does not change:

- The move stays a **line**. The money left the measured pool, so the
  statement has to say so. Drawing it as internal netting would stop
  the statement tying to the balances it is drawn from. `deposits` is
  `unpaired` made specific, not made invisible.
- The **interest** a deposit pays is untouched. It is income, it
  arrives on its own row, and it is the one movement of a deposit that
  is not a transfer.

There is one value per class, not one `vehicle_transfer` for all four.
A single value would need a rule to carry a second field naming the
pool. And money to the retirement pool and money to the education pool
are different decisions, made at different stages of a life.

A rule only sees its own family's population. So the contribution side
is a `spending.rules` entry and the distribution side an `income.rules`
entry, exactly as an own-account move is placed. **None of them is
needed where the far account is tracked.** The matcher's verdict says
everything, and it outranks a rule. Giving needs no delta of its own:
`GOVERNMENT_AND_NON_PROFIT_DONATIONS` already lands there.

---

## 6. Netting

> A node is a net. The level a view groups by decides what nets, and
> the sign of that net decides the side.

Aggregating every sale as an inflow and every purchase as an outflow
makes two gross bands dominate the diagram for any household with
ordinary portfolio churn. They say nothing. A bond ladder rolling, a
rebalance between two equity funds, or a coin traded for another coin
is not the household raising or deploying cash.

Both readings of investing are wanted, so the grain is a toggle:

- `--investing whole`, the default, nets the section and draws one
  `Investments` node on the side the period's net puts it. That answers
  the question a reader opens with. The Investments node has no leaves,
  because a class on the other side cannot be a child of a node on this
  one.
- `--investing class` nets per asset class and shows the reallocation
  the whole nets away.

Financing and the vehicles net per class, with no toggle. Cash is one
node by definition.

**The hub** is computed at the view's level, as the sum of the positive
nets there. That equals the sum of the negative nets, because the
identity above sums to zero once cash is a node. So a finer level can
have a **larger** hub than a coarser one. Two leaves of opposite sign
inside one class net away at the class level and both appear at the
group level. That is what netting means, and it is why `share` is
measured against the hub of the level being drawn.

A leaf whose net runs opposite to its class is drawn **attached to the
hub directly**, not as a child of a node on the other side. Its class's
edge carries only the leaves that stayed with it. Each stage still
conserves flow, and no node appears twice.

Netting is per period bucket: monthly for a monthly report, the whole
window for the diagram.

---

## 7. Configuration

```json
"cashflow": {
    "accounts": { "exclude": { "<source>": ["<account-id>"] } },
    "wrappers": { "529": "household", "trust_grantor": "trusts" },
    "projection": { "lookback_months": 12 }
}
```

One block, three fields, all optional.

`cashflow.accounts` is the pool, and the only account gate cashflow
applies. **It inherits neither family's scope.** An account one family
excludes is not thereby outside the household's cash pool. Consider an
income scope that drops an account whose funding leg cannot pair. If
cashflow inherited it, that account's withdrawals would stay in the
pool while its deposits vanished. Cashflow reads each family's
verdicts, not its base, so the families' scopes and base exclusions do
not reach inside the pool. When the block is absent, every account is
pooled.

**Exclusion only.** Every account is pooled by default, so an `include`
could fence nothing. A knob that does nothing is worse than none. The
config loader refuses unknown fields, so writing `include` is an error,
not a silent no-op.

`cashflow.wrappers` moves a tax wrapper across the boundary. The value
is where a crossing lands: `household` (no crossing at all),
`retirement`, `education`, `health`, `trusts` or `giving`. Unlisted
wrappers keep the engine default. An unknown wrapper or destination
fails the load, naming the entry.

It is **per wrapper, not per account**. An account a source
mis-labelled is fixed with the per-account `tax_wrapper` override, so
every consumer of the wrapper agrees about whose money it is. The two
knobs apply at different times:

- this block is re-stamped by every enrichment pass;
- an account override reaches only the rows a load touches, so a
  reload applies it to history.

`cashflow.projection.lookback_months` is the cash flow projection's
look-back (§12): 1 to 36 whole months before the current one, 12 by
default.

Nothing else is configurable. Rules, pins and transfer overrides belong
to the families. A verdict written there is what cashflow reads, and
the seven values of §5 are placed through those surfaces.

Those surfaces carry one field that only cashflow reads: an optional
`asset_class` on a `spending.rules[]` or `income.rules[]` entry, and in
either pins ledger. It is admitted only beside `investment` (spending)
or `capital_return` (income). It is validated against the exposure set
([TAXONOMY.md](TAXONOMY.md) §2), less `cash` and `other`. It says what
the capital went into, for a movement whose feed named no instrument
(§4). It is not in the `cashflow` block on purpose: the verdict and the
exposure are one statement about one row. Splitting them across two
config surfaces would let a deployment carry half of it.

---

## 8. Reading it: `wealthdb cashflow <view>`

```
wealthdb cashflow <view> [FROM [TO]] [--period P] [--level L] [--investing G]
                         [--rates R] [--maturing M] [--shift BP] [-f FORMAT] [-C COLS] [-x CCY] [-p]
```

The families' idiom, unchanged. The flags and their defaults are in
[DESIGN.md](DESIGN.md) §4.14 and `wealthdb cashflow -h`. Six views:

| view | a row is | default columns |
|---|---|---|
| `summary` | a period bucket: the cash flow statement | `period, operating_in, operating_out, operating, investing, financing, vehicles, net_cash_flow` |
| `flows` | a (bucket, node) pair at `--level`, netted at that level | `period, section, class, group, txn_count, inflow, outflow, net, share` |
| `sankey` | an edge of the window's diagram | `stage, source, target, value, share` |
| `transactions` | a cashflow line, oldest first | `silver_source, date, account, kind, section, class, group, name, currency, net_amount, value` |
| `coverage` | an (account, currency, bucket): the ledger against the account's own balances | `period, silver_source, account, currency, ledger, measured, gap, status` |
| `projection` | a month ahead: the cash flow projection (§12) | `period, investment_income, principal_returned, vehicles_income, other_income, operating_out, debt_service, vehicles, net, quality` |

**Three refusals.** Each is a usage error, not a silent ignore.
Ignoring any of them would return a plausible answer to a question
nobody asked.

- `sankey --period`: a period on an edge list means one list per
  bucket, which is a loop's job.
- `sankey --level section`: the diagram would have no inner column.
- `coverage -x`: a rate error on top of the one number that view exists
  to make trustworthy.

`projection` refuses what it does not take (§12): `--level`,
`--investing`, `--period total`, and a window that starts before next
month or cuts a month. The other views refuse its `--rates`,
`--maturing` and `--shift`.

`transactions` DOES ignore `--period`, as the families' transactions
views do.

Behind `-C`:

- on `flows`: `section_id`, `class_id` and `group_id`, the dotted node
  keys;
- on `sankey`: `source_id`, `target_id` and `section`;
- on `summary`: the memo columns `yield`, `taxes`, `fees` and `giving`,
  plus `savings_rate`, plus the reconciliation memo below.

`-p` redacts as the families do: account ids, amounts, and every name
taken off a statement line. Every name a person typed prints as a
pseudonym, such as `manual-1`, or as a redacted id
([DESIGN.md](DESIGN.md) §4.6). Sections, classes, groups and shares
stay legible. So the diagram's privacy twin needs **normalisation
alone**: no node is ever a merchant, a payer, an account or an
instrument.

`wealthdb transactions` carries `cashflow_section`, `cashflow_class`
and `cashflow_group` behind `-C`, beside the spending and income trios.
They read the **resolution**, not the base, so a row on an account no
cashflow report charts still carries its node. They are **blank** where
the resolution reached no node: a pool-internal move or a declined
kind. Which of the two is the `disposition` column of
`cashflow_txn_nodes`.

### Reconciliation

Two identities are **structural**. They guard arithmetic, not
population:

- the four section figures sum to `net_cash_flow`;
- a bucket's `flows` rows, the Cash row included, sum to zero.

The check that can catch a wrong **population** is the memo on
`summary`, behind
`-C +cash_measured,+fx_effect,+cash_flow_measured,+unexplained`. The
cash section is computed, never measured. So an unpaired transfer, or a
wire a bank books under a catch-all kind, lands in the residual and
looks like cash saved or spent. The pool's balances, unlike the
residual, are **observed**.

- `cash_measured`: the pool's cash balances at the bucket's end minus
  at its start, each at its own boundary day's rate.
- `fx_effect`: the revaluation part of that change, in two terms. One
  revalues the opening balances to the closing rate. The other
  revalues every movement in the bucket from its own day's rate to the
  closing one. Without the second, a salary received mid-year and held
  to the year's end would look like an error.
- `cash_flow_measured`: the flow the residual is taken against. It is
  **not** `net_cash_flow`. The memo can only compare observed balances
  against the flow over the accounts and dates those balances cover.
  So this term counts the movements inside the pool too, and starts
  each account at its own first snapshot (the narrowings below). It is
  printed so that the four memo columns close on their face.
- `unexplained`: what is left. Near zero, **not** zero.

The four columns close exactly:

```
unexplained = cash_measured - fx_effect - cash_flow_measured
```

Six things narrow what the memo measures. Each prevents the memo from
being wrong about itself rather than about the statement.

1. Pooled accounts only.
2. Accounts that contribute transactions only. An account loaded for
   balances alone has a real balance change and no lines.
3. Accounts with a balance history only, and never the synthetic
   per-portfolio `overlay` accounts.
4. Each account is measured **from its own first snapshot**. Collectors
   onboard at different times. One may back-load years of transaction
   history but have balances only from its first run. Blanking every
   bucket in which any account was unobserved would blank every year
   before the last collector was added.
5. The flow side covers the same accounts over the same slice. A
   diagnostic whose halves covered different populations would carry a
   structural term and stop being a diagnostic.
6. The flow side counts the movements the resolution calls
   **pool-internal**, as well as its lines. The measured accounts are
   narrower than the household. A wire to a pooled account whose
   balances are not collected leaves them, and so does a money-market
   fund bought with idle cash. Both change the measured balances, and
   the statement rightly draws neither. The flow side does NOT count
   the rows the resolution declined: showing them in `unexplained` is
   what the memo is for. Counting the pool-internal rows is essential
   for a currency conversion between two own accounts. Its two legs do
   not offset in native terms, so without them the whole conversion
   would land in `unexplained`. At their own amounts they move the
   measured flow by exactly what the balances moved.

A bucket reports blank where nothing is measurable, or where no rate
can value an amount at a boundary. The memo is a diagnostic with a
stated tolerance, never an input: `net_cash_flow` does not read it.

### Coverage: which accounts the statement can be trusted on

The memo answers one question per period for the whole pool. When it
does not close, it says so in one number and stops. The account behind
that number is not in the statement.

`wealthdb cashflow coverage` names it. Per account and per period, it
sets the cash change the account's transactions imply against the
change its own balances show. With `-C +unsigned`:

```
period  silver_source  account  currency      ledger    unsigned    measured         gap  status
2099    bank           Everyday CHF      -111111.11   999999.99  -22222.22  -88888.89  obscured
2099    bank           Mandate  USD        99999.99        0.00    1111.11   98888.88  measured
```

The figures are invented; the shapes are the two that matter. The
first account converts currencies, so its gap is smaller than the
volume no sign could be read from, and nothing can be concluded. The
second carries no unsigned volume, so its gap is real.

It differs from the obvious query in three ways, and each fixes a way
that query is wrong.

1. **It does not convert.** A per-account gap belongs in the account's
   own currency. Converting it adds a rate error to the number the
   report exists to make trustworthy. `-x` is refused.
2. **It publishes its own blind spot.** Some volume moves no cash that
   a sign can be read from:
   - the six unsigned kinds of §4: the FX family, corporate actions and
     the two catch-alls;
   - an in-kind transfer (`transfer_in` / `transfer_out` carrying an
     instrument). It is valued securities that moved no cash.

   Dropping them quietly makes an account that converts currencies, or
   received a portfolio in kind, look like one with an enormous hole.
   The dropped volume is the `unsigned` column. A gap no larger than it
   reads **obscured**: not clean, not damning, and not answerable from
   what the adapters signed. An account with no unsigned volume is
   never obscured. It reads **measured**, and its gap stands. A gap of
   zero means the account reconciles.
3. **It reports what it cannot measure.** In a join-based query, an
   account with no balance history never appears, and absent reads as
   fine. Here it reads **unmeasurable**. An account whose balance series
   begins inside the period reads **opening**. One whose series stopped
   before the period reads **ended**. Either way the difference across
   the period is not a real change.

Read `status` first, then sort by `gap` within `measured`. The report
already lists each period's rows by gap size. That puts first the
account whose balances moved most without flows to explain it. A feed
that stopped covering an account mid-window shows up there.

### The gap to the family reports

Cashflow's income differs from `wealthdb income`'s total by exactly
four terms, each computed separately:

1. the **vehicles' own income**: a plan's dividends are not the
   household's cash flow;
2. the **reimbursements** that link to no outflow, which cashflow
   keeps and income drops;
3. the **re-homed verdicts**: `capital_return` and `investment` to
   investing, `loan_proceeds` to financing;
4. any account the families' own **scopes** exclude but the pool keeps.

If the four do not close the gap, the resolution is wrong somewhere,
and that bridge is the cheapest place to see it.

---

## 9. The dashboard

**Cash Flow**, in the Spending and Income dashboards' shape, with a
privacy twin linked from the top row. It has nine pickers:

- time range;
- source;
- a required currency;
- a section;
- **Investing**: as a whole (the default) or by asset class. It binds
  to the diagram and the investing trend;
- the projection's four: Horizon, Rate path, Maturing and Shift. They
  reach the projection section alone (§12).

The projection section takes the currency and the projection's four.
It declines the other four: it reads the months ahead and the
household's whole statement.

**The Section picker is narrower than the statement's other four.**
It reaches the by-month charts and `Largest flows`. The headline
figures and the diagram decline it:

- a statement narrowed to one section is not a statement;
- a savings rate or a yield share narrowed to one section has lost a
  leg of its own ratio;
- a one-section diagram draws a `Cash savings` node that absorbs the
  whole section, not the residual it names.

A by-month chart already scoped to one section (Inflows, Outflows,
Investing) goes empty when a different section is picked, which is what
a dashboard filter means. The scope is enforced by withholding the
filter's template tag from the cards that decline it. So a card added
later cannot drift into the picker, and `test_provision.py` checks it
card by card.

**There is no account picker, by design.** The household boundary
separates the household's cash flow from its vehicles'. A picker that
moved accounts in and out of the pool would turn every crossing it
split into an unexplained disappearance. A wire between two of the
household's own accounts is invisible only while both are in the pool.

| row | cards |
|---|---|
| headlines | Operating in · Operating out · Net cash flow · Savings rate · Yield share |
| the diagram | Cash flow: the Sankey, full width, the window's four stages |
| the shape of the window | Cash flow statement by month: signed stacked bars per section |
| the two sides | Inflows by class by month · Outflows by class by month |
| the swing sections | Investing by month · Financing and vehicles by month |
| the lines | Largest flows: the fifty largest lines of the window |
| the projection | the months ahead (§12): income parts against outflow parts by month, the net and the vehicles' income as lines; its table |

Every tile is a **native query over `web_cashflow`**, the line-grain
serving view. The Sankey card computes its nets, its sides and its hub
**in the query**. A node's side is the sign of its net over the
filtered window. A pre-netted table would be netted over the wrong
window the moment a reader moved a picker.

The twin is **normalisation, not redaction**. With the hub at 100 there
is nothing left to hide, because no node is ever a merchant, a payer,
an account or an instrument. One card is the exception and drops
columns, as every other twin card does. `Largest flows` names the
account a line moved through and the payer or merchant behind it, so
the twin ranks the lines and projects neither.

---

## 10. Storage and lifecycle

Cashflow has no store of its own. It adds columns to the family
overlays, two stamped tables, and one serving table:

| table / column | grain | survives `reset`? | carried by `reload -a`? |
|---|---|---|---|
| `spend_txn_enrichment.far_*` (three columns) | per transaction | no | n/a, re-derived |
| `spend_txn_enrichment.stated_asset_class`, `income_txn_enrichment.stated_asset_class` | per transaction | no | n/a, re-derived |
| `cashflow_wrapper_sides` | one row per tax wrapper | yes | re-stamped from config |
| `cashflow_account_scope` | config, stamped into gold | yes | re-stamped from config |
| `report_cashflow_plan` (view `web_cashflow_plan`) | per reporting currency, month ahead, part and class | yes | n/a, rewritten by every web refresh |

Nothing is stored for the memo. It reads `cash_balances` and the
resolution at query time.

The wrapper table is stamped **whole**: every value of the enum,
defaults included. That keeps the `defaultWrapperSides` table
(`canonical.WrapperBoundaries`) the single place a wrapper's side is
decided. Stamping only the overrides would put the defaults in SQL a
second time. A wrapper added for a new jurisdiction would then take two
edits to reach the statement.

`wealthdb load` prints a `cashflow:` block. It reports:

- the boundary stamped: wrappers, overrides, and accounts out of the
  pool;
- how many own-account moves carry a far account, and how many of
  those the source stated itself;
- how many pairs the matcher asserted on a shared reference, on a
  described counter leg, and on a narrative naming the other account;
- source references that named more than one movement and paired
  nothing, when there are any;
- loudly, how many pooled accounts have no tax wrapper;
- declared accounts, how many sit inside the pool, and how many no
  rule-placed row reached;
- `cashflow.accounts` ids that match no account.

Far accounts are rows, one per leg. The matcher's figures are pairs,
each writing two rows.

### The diagram's node order is the renderer's

Within a column, the BI layer's Sankey orders nodes by force
relaxation. Each node is pulled toward the weighted centre of its
neighbours, and the column is re-sorted by position, for a fixed number
of iterations. It converges to a fixed point that ignores the input
order. Reversing the whole edge list gives an identical layout. The BI
layer exposes no iteration count and no node-sort setting.

So a class's leaves are NOT drawn together. A large leaf settles high
and can land among another class's leaves, where it reads as though it
belonged to that class. Where it lands depends on the data in the
window.

Two consequences:

- The edge list's own ORDER BY groups a class's leaves together. That
  makes the rows readable and does nothing for the diagram. The comment
  on it says so.
- Flattening a class into its leaves is not a fix. It reduces
  interleaving only by removing a level. The same interleaving
  reappears between whichever classes still have leaves. Trading a node
  of the vocabulary for that would fit one window of one deployment. If
  the BI layer exposes the layout's iteration count, this becomes a
  visualisation setting and stops being a data question.

### Until the first load, the statement is wrong

The cashflow migrations create the two boundary tables and the far
columns **empty**. Nothing backfills them. The enrichment pass writes
them, and it runs inside `wealthdb load` and `reload`. Before that pass
the statement is not just incomplete. It is wrong in a way that reads
like a finding:

- `cashflow_wrapper_sides` is empty, so every wrapper reads as
  household, and the pool is **every account**, vehicles included;
- no enrichment row carries a far account, so the far-account ladder
  (§4) falls to its last step, and **every matched own-account move**
  resolves to `vehicles · Unpaired transfers`.

The dashboard then shows one enormous `Unpaired transfers` node. It
looks like a data problem and is not. `wealthdb load -a` ends the
state. The load that ends it prints a line saying so, once.

A binary with unapplied migrations applies them on any read-write open
of gold. That includes `web-materialize` and every command that is not
read-only. So this state can begin without anyone running a migration
on purpose.

`wealthdb status -v` prints a `cashflow:` block with three counters.
Each names a way the statement can be quietly wrong:

- **excluded by kind** on pooled accounts: the FX family, corporate
  actions, unmatched transfers, unpaired card payments and the
  catch-alls;
- **no tax wrapper** on pooled accounts: the boundary's coverage gap;
- **no far account**: rule-placed moves in `Unpaired transfers · No far
  account`. Collecting or declaring that account would resolve them.

---

## 11. Known limits

- **What the boundary removes has no memo.** The vehicles' own income
  and spending, and the tax they withheld before the household saw the
  money, appear on no line and in no memo (§3).
- **The two families' limits show here too:**
  - platform custody deposits ([INCOME.md](INCOME.md) §11);
  - reimbursements no road links to an outflow
    ([SPENDING.md](SPENDING.md) §3, *Reimbursements*);
  - cross-currency own-account moves no assertion reaches. These are a
    movement whose source neither stamps one reference on both halves
    nor describes the counter leg, and a card bill whose two feeds
    share no id space.

---

## 12. The cash flow projection: `wealthdb cashflow projection`

```
wealthdb cashflow projection [FROM [TO]] [--period P] [--rates R] [--maturing M] [--shift BP]
                             [-f FORMAT] [-C COLS] [-x CCY] [-p]
```

The cash flow projection sets the household's expected income against
its expected outflows, month by month, over the months after the
current one. It is a projection from today's holdings and the past
months, not a plan. Its window is the income projection's
([INCOME.md](INCOME.md) §12): by default the horizon,
`yield.horizon_months`. It is whole months from next month on: FROM a
month's first day, TO a month's last. `--period` is `monthly` (the
default), `quarterly` or `annual`.
`--rates`, `--maturing` and `--shift` are the income projection's
assumptions.

Two sources feed it:

- **the income projection**, for what the holdings are expected to
  pay and the principal their maturities return;
- **the look-back**, for the rest: the cash flow statement's lines of
  the whole months before the current one,
  `cashflow.projection.lookback_months` of them (§7). Each part is
  their sum spread evenly over those months, in the output currency at
  each line's own day's rate, as the summary converts it. So a seasonal
  expense is spread, not timed. An annual row says what a year looks
  like.

The look-back's months run from the month of the statement's first
line ever, counted whole, so a household with a shorter history
spreads over the months it has.

| column | reads |
|---|---|
| `investment_income` | the income projection's expected income in the month on the pool's accounts (§2), under the same assumptions. This view never recomputes it |
| `principal_returned` | the principal maturities return in the month on the pool's accounts, as the income projection's `principal`. It counts in `net` unless `--maturing roll`: under roll it buys the holding's successor |
| `vehicles_income` | the income projection's expected income and principal in the month on every account outside the pool: a vehicle's, a giving account's (§3), or one `cashflow.accounts` fences out. It stays there, so `net` leaves it out |
| `other_income` | the operating income the income projection leaves out: wages, benefits, receipts, and yield it does not read (staking, royalties). The monthly average of the look-back |
| `operating_out` | the operating outflows: the summary's `operating_out` over the look-back, its monthly average, positive. Taxes booked as spending stay inside it |
| `debt_service` | each mortgage's instalment, continued to its maturity, and the other financing outflows (below) |
| `vehicles` | contributions to and draws from the vehicles, the monthly average of the look-back, signed as the statement signs it |
| `net` | `investment_income + other_income - operating_out - debt_service + vehicles`, and `principal_returned` unless `--maturing roll` |
| `quality` | the tags of the parts: `projected` (the income projection's), `trailing` (the look-back's averages), `terms` (a mortgage's instalments run to its terms' maturity), `thin` (the look-back holds under six months) |

`-C +consumption,+fees,+taxes,+giving,+uncategorized` splits
`operating_out` by its classes (§5). The rows end with `total` and
`monthly_average`.

Debt service:

- A mortgage's instalment is what the look-back paid into it per
  month, interest and amortization together, from its first instalment
  on.
- It continues each month whose first day is on or before the
  mortgage's maturity: the latest of its tranches' terms.
- A mortgage with no maturity in its terms continues through the
  window.
- A mortgage today's holdings no longer hold was repaid and counts
  nothing.
- Any other financing outflow continues at its monthly average: a
  loan's repayments, or a mortgage instalment with no far account.

The two halves meet without overlap:

- the income projection reads interest, coupons, dividends,
  distributions, rent and pensions, so `other_income` leaves out the
  lines the statement classes as those types;
- the income projection earns nothing on a liability, so a mortgage's
  interest is in `debt_service` alone;
- the mortgage is under financing, never in `operating_out` (§5).

The income projection reads every holding, a vehicle's and an account
`cashflow.accounts` fences out too. The cash flow projection splits it
at the pool's boundary, as the statement does (§3):

- a holding of a pool account pays into `investment_income` and
  `principal_returned`;
- any other holding pays into `vehicles_income`, which `net` leaves
  out.

So `investment_income + principal_returned + vehicles_income` is
`wealthdb income projection`'s `expected` plus its `principal`, month
by month, to the cent.

The cash flow projection is a trailing model, not a schedule or a
budget:

- outflows and other income are averages, not a list of recurring
  payments;
- a mortgage's instalment is the one it was paid, continued; no
  amortisation schedule is computed from its rate, and a one-off
  repayment in the look-back is spread over its months like any
  instalment;
- no withholding is projected; the Income dashboard shows it.

The Cash Flow dashboard's projection section shows it (§9;
[web/DESIGN.md](../../web/DESIGN.md) §6). A web refresh stores the
look-back's parts (§10). The MCP `cashflow` tool serves the same view.
