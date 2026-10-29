# Income

What the tracked accounts received: wages, interest, dividends, staking
rewards, rent, gifts. Income is consolidated across every source, typed,
and attributed to a payer. The forward views add what the holdings are
expected to pay (§12).

This document is a **delta against [SPENDING.md](SPENDING.md)**. Income
and spending are one engine read in two directions. They share one
enrichment pass, one precedence lattice, one internal-transfer matcher,
one signature normaliser and one model-tier protocol. Where a mechanism
is shared, this file points at the section of SPENDING.md that explains
it. A second explanation would be a second thing to keep true.

---

## 1. Scope: inflows only

Income is **what arrived** on any tracked account that the household
did not already own.

That last clause is the hard part, and §2's deltas write it down. Money
arriving is not always income:

- a funding wire between two of the holder's own accounts arrived;
- a private fund returning contributed capital arrived;
- a loan being disbursed arrived.

None of them is earnings, and each leaves the base by a different route.

What is deliberately **not** here:

- **Investment buys and sells**, maturities, corporate actions and FX
  legs. They belong to the cash flow statement
  ([CASHFLOW.md](CASHFLOW.md)). Sale proceeds are not income under any
  reading and never enter the base.
- **Gross-of-payroll wages.** A bank sees the net salary. The gross,
  the deductions and the employer's contributions are on a payslip the
  product does not collect. Income is what arrived (§5).
- **Reimbursements.** An expense reimbursement is money back for money
  spent. Income excludes it, and spending nets it against the outflow
  it answers ([SPENDING.md](SPENDING.md) §3, *Reimbursements*).
- **Tax reporting.** Withholding is visible beside income (§5), but no
  view claims to be a tax figure. Jurisdictions differ on staking, on
  capital-gains distributions and on gifts.
- **Accrued but unpaid income.** Income is cash basis, like spending.

### The population, layered

Income has three macros where spending has four. Each is narrower than
the one it reads, for the reason SPENDING.md §1 gives: the pass decides
what the base excludes, so it cannot read a base that already excluded
it.

| macro | what it is |
|---|---|
| `income_scoped_accounts()` | every account, until an `income.accounts` entry takes one out |
| `income_enrichment_population(f, t)` | what the pass may write a verdict for |
| `income_lines_base(f, t)` | what a view charts: the population with its type resolved, less the deltas §2 keeps out of the base |

There is **no income matcher pool**. The internal-transfer matcher
admits `deposit` on every account (migration 0044), so a funding wire
pairs. It writes `internal_transfer` on both legs. Income reads those
verdicts and pairs nothing (§10, decision 8).

### Platform wallets and custody accounts

A `deposit` on a private-market platform can only be the holder's own
money arriving to be invested. The account is an investment wallet, or
a custody account holding an SPV or an exercised grant. Such a platform
pays out as a `distribution`, and the kind floor already takes that out
of the base. So an inbound leg there is a funding leg. The matcher
removes it as an own-account move **when the funding bank's side is in
gold**.

When it is not, the matcher has nothing to pair. On a custody account
the account kind still says what the leg is: a built-in rule places it
`capital_return` (§3). A leg the matcher pairs keeps the matcher's
`internal_transfer`, since the matcher outranks the rule. On any other
account the leg stays in the base as an unplaced receipt. That is the
honest answer for a movement whose other half the product cannot see:

- a source loaded for balances alone contributes no transactions;
- a greedy one-to-one matcher with a missing partner pairs nothing
  rather than guessing.

Widening the matcher's window or tolerance to reach these legs buys
false pairs faster than real ones. Funding amounts are round numbers
that recur across banks.

Where the funding bank is loaded for balances alone, the remedy for a
platform wallet is `income.accounts.exclude`. A wallet's inflows are
not income, and the income scope exists for this kind of reason. §11
states the limit behind it.

### Which kinds are income

Income selects rows by kind, not by account, exactly as spending does.

| kind | where it goes |
|---|---|
| `dividend` | in; floor `INCOME_DIVIDENDS` |
| `coupon` | in; floor `INCOME_INTEREST_EARNED` |
| `staking` | in; floor `INCOME_STAKING` |
| `capital_gain` | in; floor `INCOME_DISTRIBUTIONS`. A public fund's payout of realised gains returns no basis |
| `reward` | in; floor `INCOME_REWARDS` |
| `distribution` | in the population, **out of the base**; floor `capital_return`. A private fund returns contributed capital first |
| `deposit` | in, with **no floor**. It is the one kind the narrative tiers decide |
| `interest` | in when **positive**. The negative side is a finance charge and belongs to spending |
| `refund` | spending's. It nets inside the merchant's category there |
| `sell`, `contribution`, `transfer_in`, `card_payment`, `corporate_action`, `journal`, `other`, the FX kinds | out |

Seven of the eight admitted kinds carry **both signs**. A negative row,
such as a dividend clawed back or a credit reversed, nets against what
it reverses inside its own type. On `capital_gain` and `staking` a
negative row need not be a reversal: it can be a realised loss or a
slashing penalty. The same arithmetic nets it in the same place, which
is right either way.

`interest` is the one kind with a **sign guard**. The guard is not a
policy about reversals. One canonical kind carries two opposite events,
interest credited and interest charged. Only the sign tells them apart,
and spending claims the negative half (migration 0041).

A negative `deposit` is a reversal by construction:

- the canonical sign of `deposit` is positive, and
  `ApplyCanonicalSign` forces it (`canonical/sign.go`);
- only an adapter that keeps a source sign on purpose can produce a
  negative one. An adapter does this when the source marks a reversal.
  The Fidelity adapter keeps every sign, because its sources sign each
  row from the account's side.

---

## 2. The taxonomy

Income uses the same provenance, the same three classes and the same
predicates as SPENDING.md §2, in one table and one dimension.
`spend_categories` carries a `family` column (migration 0069). A row is
`spending`, `income`, or `both`.

**Vendored, verbatim:** Plaid's `INCOME` primary, version 2, and its
thirteen detailed values. Their descriptions are copied untouched, so a
taxonomy refresh still diffs cleanly. Each value reads in a report as
its label:

| value | label |
|---|---|
| `INCOME_SALARY` | Salary |
| `INCOME_GIG_ECONOMY` | Gig economy |
| `INCOME_CONTRACTOR` | Contractor |
| `INCOME_DIVIDENDS` | Dividends |
| `INCOME_INTEREST_EARNED` | Interest earned |
| `INCOME_RENTAL` | Rental |
| `INCOME_RETIREMENT_PENSION` | Retirement pension |
| `INCOME_UNEMPLOYMENT` | Unemployment |
| `INCOME_LONG_TERM_DISABILITY` | Long-term disability |
| `INCOME_MILITARY` | Veterans benefits |
| `INCOME_CHILD_SUPPORT` | Child support |
| `INCOME_TAX_REFUND` | Tax refund |
| `INCOME_OTHER` | Other income |

Plaid's five other primaries are not vendored, as on the spending side
(SPENDING.md §2):

- `TRANSFER_IN` says money arrived, not from whose account. The matcher
  pairs an own-account move, and the other tiers place the rest.
- `LOAN_DISBURSEMENTS` is money borrowed. The `loan_proceeds` and
  `mortgage_transfer` deltas name it.
- `OTHER` is Plaid's bucket for a row it could not place.
- `TRANSFER_OUT` and `LOAN_PAYMENTS` are outflows, and spending's.

**Eight extensions** are wealthdb's own, in the vendored shape under
`INCOME`. The model may emit them. A future Plaid value supersedes one
as a clean diff.

| value | label |
|---|---|
| `INCOME_GOVERNMENT_BENEFITS` | Government benefits |
| `INCOME_ROYALTIES` | Royalties |
| `INCOME_ALIMONY` | Alimony |
| `INCOME_STAKING` | Staking |
| `INCOME_REWARDS` | Rewards |
| `INCOME_DISTRIBUTIONS` | Distributions |
| `INCOME_INSURANCE_PAYOUT` | Insurance payout |
| `INCOME_ENERGY_FEED_IN` | Energy feed-in |

Each extension clears the usual bar:

- it is common;
- it is distinct on a statement or a tax return;
- the vendored vocabulary has no value for it.

The bar is set for households in general, because the product is
published.

`INCOME_INSURANCE_PAYOUT` is income, although `reimbursement` also
seems to describe it. The reasons:

- the premium was already counted as spending;
- nothing in the data links a payout to the premiums it answers. The
  amounts, the dates and often the years differ;
- netting the payout out would count the outflow and drop the inflow.

`reimbursement` keeps the cases where the outflow IS identifiable: a
utility credit against a bill, a merchant reversing its own charge. A
recurring disability benefit is not a payout, even from an insurer. It
is `INCOME_LONG_TERM_DISABILITY`, whoever pays it.

`INCOME_ENERGY_FEED_IN` is what a grid operator or an energy retailer
pays for electricity that the household's own generation fed into the
grid. It is income from an asset the household owns, earned without its
labour. The cash flow statement files it under yield, beside rental
income and royalties.

`INCOME_GOVERNMENT_BENEFITS` covers the state transfers no other value
names: child and family allowances, parental-leave pay, housing
benefits, social assistance, stimulus payments. A state pension, an
unemployment benefit, a disability benefit, a veterans benefit and
child support paid by an agency each have a vendored value. A tax
refund is never a government benefit, whichever tax office pays it and
in whatever language.

`INCOME_ALIMONY` is maintenance from a former spouse or partner. It is
the half of family maintenance that `INCOME_CHILD_SUPPORT` leaves out.
Child support is `INCOME_CHILD_SUPPORT`, whether a court ordered it or
the parents agreed it. Maintenance a person pays is person-shaped, so
the fence keeps most of it from the model. A rule or a pin places it.

**What the vendored descriptions leave out.** Plaid's texts are kept
verbatim, so the policies they cannot carry are written here:

- Tips paid through payroll are `INCOME_SALARY`. Cash tips paid into an
  account are `cash_deposit`.
- Military pay is `INCOME_SALARY`. `INCOME_MILITARY` is veterans'
  benefits only.
- Pay from a gig platform is `INCOME_GIG_ECONOMY`.
- `INCOME_CONTRACTOR` covers freelance and contract work, a sole
  trader's takings and an owner's draw.
- A returned tenancy deposit and the proceeds of selling a property are
  `capital_return`, not `INCOME_RENTAL`.
- A disability benefit is `INCOME_LONG_TERM_DISABILITY`, whoever pays
  it.
- Child support that an agency pays out or advances is
  `INCOME_CHILD_SUPPORT`.
- A payout from a plan whose balance is the holder's own is
  `retirement_transfer`: a 401(k), an IRA, a pillar 3a or a
  vested-benefits account. It is a delta, so a rule or a pin places it.
  A pension paid from a pool the holder does not own is
  `INCOME_RETIREMENT_PENSION`, social security among them.

**Fourteen deltas** are wealthdb's own. They are primary-level and
lowercase, and they record structure a payer's name cannot reveal:

| value | meaning | in the base? |
|---|---|---|
| `internal_transfer` | movement between two tracked accounts, either leg | no |
| `capital_return` | the holder's own capital arriving: returned, or paid into a platform's custody | no |
| `loan_proceeds` | money borrowed arriving; a liability incurred | no |
| `reimbursement` | money back for money spent | no |
| `retirement_transfer` | a payout from, or a contribution to, an untracked retirement plan whose balance is the holder's own | no |
| `education_transfer` | the same for an education plan or savings account | no |
| `health_transfer` | the same for a health savings account | no |
| `trust_transfer` | the same for a trust that is a separate taxpayer | no |
| `deposit_transfer` | the principal of a bank deposit product coming back, such as a call or fixed-term deposit repaid. The interest is `INCOME_INTEREST_EARNED`, not this | no |
| `mortgage_transfer` | a tranche drawn from a mortgage servicer the product does not hold as an account. The outflow leg is the instalment | no |
| `gift` | a cash gift or family support received | yes |
| `inheritance` | an estate's distribution to the holder | yes |
| `cash_deposit` | cash paid in at a counter or a machine | yes |
| `other` | a receipt no tier could place, that a tier nonetheless placed there | yes, labelled `Other` |

Nine of them are **one row** read from either side, which is what
`family = 'both'` means: `internal_transfer`, `gift`, `other`, the five
crossings and `mortgage_transfer`.

The five crossings (`retirement_transfer` to `deposit_transfer`) serve
the cash flow statement ([CASHFLOW.md](CASHFLOW.md) §5). They leave the
base for `internal_transfer`'s reason: the money is the holder's
throughout, in a pool whose far side the product does not hold. Four of
them are earmarked by a tax wrapper. `deposit_transfer` has no wrapper.
The direction is the row's own. A deposit placed `retirement_transfer`
is a plan payout, and a withdrawal placed the same is a contribution.
None of them is needed where the far account is tracked: the matcher's
verdict says everything, and it outranks a rule.

`other` is **not** `(uncategorized)`, and the difference is the backlog:

- `other` is a stored value with its own label. A tier placed the row
  there, and the row leaves the model tier's backlog for good. A rule
  that files a class of receipt as `other` removes it from the backlog.
- `(uncategorized)` is how a row with no resolved type reads. Nothing
  placed it, and the next `categorize income` run asks about it.

Nor is `other` the catch-all `INCOME_OTHER`, which reads "Other
income". A catch-all is not a verdict (SPENDING.md §3). A tier that can
only place a row there declines it, and `--refine` re-asks a model
verdict there. The two values keep separate labels, so a chart draws
them as two slices.

Spending's `card_spend` has no mirror, on purpose. It exists because
deleting an unpaired card bill deletes real consumption. An unpaired
inbound wire is not deleted. It stays in the base, visible and
unplaced, which is the honest answer.

**Two validity predicates**, as on the spending side and for the reason
SPENDING.md §2 gives. `ValidIncomeDetailed` admits everything storable.
`ModelIncomeDetailed` refuses the deltas. Both derive from the `family`
column, so a value added to `canonical` is admitted or refused
everywhere at once. The predicates are **fenced by family**. A spending
rule naming `INCOME_SALARY` fails at config load, and so does an income
rule naming `FOOD_AND_DRINK_GROCERIES`.

**Retired spellings.** Five values of an earlier vocabulary are not
values of this one. Validity is read off the table, so an income rule
or an income pin naming one is refused like any unknown value. The
error names the value to write instead (`canonical.RetiredDetailed`).
The model gauntlet refuses one the same way, and its feedback names the
value to emit instead.

| retired | write instead |
|---|---|
| `INCOME_WAGES` | `INCOME_SALARY`, or `INCOME_GIG_ECONOMY` for gig-platform pay |
| `INCOME_OTHER_INCOME` | `INCOME_OTHER` |
| `INCOME_SELF_EMPLOYMENT` | `INCOME_CONTRACTOR` |
| `INCOME_RENT` | `INCOME_RENTAL` |
| `INCOME_ALIMONY_AND_CHILD_SUPPORT` | `INCOME_CHILD_SUPPORT` for child support, or `INCOME_ALIMONY` for maintenance from a former partner |

A pins ledger is read before gold is opened for writing. So a stale pin
fails before anything is migrated or loaded.

Migration 0111 moves each stored verdict to the first value in its row
above. A stored verdict of the split value therefore reads
`INCOME_CHILD_SUPPORT`. A config rule on the payer's narrative moves a
payer of maintenance to `INCOME_ALIMONY`, and a pin moves one payment.
`reload -a` respells the payer store it carries across the same way
(§9).

A verdict placed under the wider meaning of a narrowed value keeps it.
Examples:

- a disability or veterans' benefit filed as `INCOME_GOVERNMENT_BENEFITS`;
- an insurer's disability benefit filed as `INCOME_INSURANCE_PAYOUT`;
- gig-platform pay filed as wages, which reads `INCOME_SALARY`.

`categorize income --all` re-asks them.

---

## 3. The tiers and the precedence lattice

The lattice is spending's in every tier the pass writes, and the
provenance vocabulary is spending's seven tier values
([SPENDING.md §7](SPENDING.md)). The eighth, `reimbursement`, marks a
row of spending's base only. One step is **re-ordered**: the kind floor
is read over the model, not under it.

```
spending:  pin > matcher > built-in rule > config rule > provider > model > kind floor
income:    pin > matcher > built-in rule > config rule > provider > KIND FLOOR > model
```

The re-order follows from the **weight each tier carries**:

- On the outflow side the narrative decides nearly everything. The
  kind floor is a backstop.
- On the inflow side the floor answers for every admitted kind but
  one. A `dividend` row is dividend income whatever its narrative says.
  The narrative tiers exist for `deposit`, which has no floor, because
  only the narrative can say what it was.

The tiers, where income differs:

- **Built-in rules.** There are two, both **gated on `deposit`**:
  - a deposit on a `custody` account reads `capital_return`. A custody
    account is a platform's holding of private-market interests, so
    cash paid into it is the holder's capital going in. The platform's
    narrative cannot say so. The account kind can;
  - `cash_deposit`, from a narrative naming a counter or machine
    deposit. Cash paid in has no counter-leg anywhere.

  Almost every structural inflow fact is the matcher's already, and a
  leg the matcher pairs keeps its verdict. `deposit` is the one
  admitted kind the floor does not answer. The rule tier sits above
  the floor, so an ungated phrase match would replace a right verdict.
  For example, a call account's interest narrated "INTEREST ON CASH
  DEPOSIT" would read as cash paid in over a counter. Config rules are
  NOT gated. A rule is the holder's own instrument, and its purpose is
  to say something the data does not.
- **Config rules.** `income.rules[]` takes the shape of
  `spending.rules`, scope and `far` included, with the value field
  named `type`. The far account an income rule names is written onto
  the spending overlay, the one overlay with far columns. So the inbound
  leg of a move to a declared account places like its outbound leg
  ([CASHFLOW.md](CASHFLOW.md) §4).
- **Provider tier.** A bank's booking type can say "salary",
  "dividend", "interest". The provider maps have an income side, keyed
  per (silver kind, account kind) as on the spending side. The two maps
  are separate because one booking type can mean different things by
  direction: UBS books both halves of an account's interest settlement
  under one type. Plaid's category taxonomy (`plaid`) has an income
  side too; [adapters/plaid.md](adapters/plaid.md) §10 lists its
  translations.
- **Model tier.** It is asked only about the payer signatures on
  `deposit` rows that the deterministic tiers left unplaced (§7). The
  fence is identical, and it matters more here (§6).
- **Pins.** `income.pins` uses the spending ledger's format, with
  `income_detailed` in place of `spend_detailed`. SPENDING.md §3,
  *Pins*, covers the mechanics. That includes a pin naming a
  transaction outside this family's population: it is written and
  counted as matched, and only `wealthdb transactions` shows it.

An `income.rules[]` entry or an income pin may carry an optional
`asset_class`, admitted only beside `capital_return`. No income report
reads it. It says what the returned capital had been deployed into, so
the return leg of a private holding draws on the same cash-flow node as
its funding leg ([CASHFLOW.md](CASHFLOW.md) §4 and §7).

The deterministic pass runs inside `wealthdb load`. It is **one pass
writing both families** in one transaction: one matcher run, both
overlays deleted and re-asserted, both verdict stores re-keyed, one
commit. Only the model tier is a separate command.

### The floor above the model (migration 0073)

The two families ask a model **different questions**. Which question it
is decides whether the model or the floor ranks higher.

On the outflow side the model names a merchant and picks that
merchant's category. It may never decide that a card purchase was
really an investment. The nature of the row is the data's, and the
model adds a finer reading of who was paid. A merchant verdict is
finer than any kind floor, so spending reads the floor under the store
(migration 0066).

On the inflow side the model is asked **what kind of income** a
receipt is. That is a claim about the transaction itself, not about who
sent it. Wherever the data already makes that claim, the model can only
be wrong. So income reads the floor over the store, and the model
decides only the kind with no floor: `deposit`.

The shape this protects against is one payer with two kinds. A
signature is shared by every row that folds to it. Take an employer
whose shares the household also holds. Without the re-order, the salary
deposit's `INCOME_SALARY` verdict would re-type that employer's
dividends. With it, the dividend keeps `INCOME_DIVIDENDS` with
provenance `kind`, and the deposit keeps the store's verdict with
provenance `model`.

**Every tier the pass writes still outranks the floor.** They all
write into the overlay, and the overlay heads the resolution. A pin,
the matcher, a built-in rule, a config rule and the provider map all
sit above the floor. Only the model sits below it. Decision 4 holds: a
`distribution` floors to `capital_return`, and a tier that writes the
overlay promotes it. In practice that is a rule or a pin.

The **kind floor** is read at query time and never stored. Its
provenance is `kind`. One arm places a delta, `distribution` →
`capital_return`, which no spending floor does. Such a row leaves the
base with no verdict written anywhere. The pass still records its
signature.

---

## 4. The payer

Spending's merchant resolves in three steps. The payer resolves in four,
with one step in front that only the inflow side needs. The first step
that **answers** wins, so a step yielding nothing falls through:

1. a line whose resolved type is a **delta** has no payer, and this
   step is final. An own-account move names the holder's own bank, and
   a gift names a relative. Neither is a payer.
2. the **instrument**, where the line carries one: the company, the
   issuer, the protocol, the fund. Its name is used, or its symbol where
   the name is unknown.
3. the **payer store's** name for the line's signature.
4. the **signature** itself, trimmed.

The signature is the fold spending computes, from the same
`counterparty` and `description`, by the same normaliser at the same
`SignatureVersion`. A counterparty is keyed one way whichever direction
the money moved, and a version bump re-keys both stores. The **store is
separate**: `income_payer_categories`, keyed by signature, holding an
income type. One counterparty can be both a merchant and a payer, and
the two questions have two answers.

---

## 5. Gross as booked, withholding beside it

**Income is what the source booked as arriving.**

- Where a source books a gross dividend and a separate withholding row,
  income shows the gross. The withholding row is already spending's
  `WITHHOLDING_TAX`.
- Where a source credits a dividend net but states its gross, the
  adapter books it the same way: the gross, and the withholding as its
  own `tax` row. The two rows sum to the cash credited. UBS states the
  gross in two places
  ([adapters/ubs.md](adapters/ubs.md#dividends-the-security-and-the-gross)
  §7, *Dividends: the security and the gross*). Its confirmation states
  it by amounts or by rates. Its export row states it in the currency
  credited, or beside the rate it converted at. A reversal of such a
  dividend takes back the gross. Its tax row is positive, so the
  `withheld` memo (below) leaves it out. Where no record states a gross
  or a rate it follows from, the net is the income.
- Where a bank books a net salary, the net is the income. The gross is
  on a payslip the product does not hold, and the product does not
  print a figure it cannot see.

This keeps the household arithmetic exact. *Income − spending*
subtracts withholding once, on the side that booked it. A
net-of-withholding income would subtract it twice.

**`withheld`** is a memo column on `summary`, off by default. It shows
the same-window `tax` rows on the same accounts, beside the income they
were withheld from. `net_income` does not read it. It is not on
`types`, on purpose: a tax row names the security it was withheld from,
not the income type. There is no honest way to attribute it to one row
of that report.

The memo is **every negative `tax`-kind row on those accounts**,
negated to read as a positive magnitude. That is how a brokerage books
withholding at source. It is also how UBS books a transaction stamp
duty, so a purchase-heavy month can show `withheld` with no income
beside it. The column is named for what it usually holds. It points at
spending's `WITHHOLDING_TAX` and is never an input to a total. A
positive `tax` row, a refund of withholding, is not in it.

A bucket can hold **withholding and no income**. The two sides are FULL
JOINed by bucket, so neither drops a bucket the other has. Such a
bucket prints with `txn_count 0` and empty money columns, and
`-C +withheld` shows why it is there. Tax paid in a month that received
nothing is worth seeing.

On the forward views `withheld` is a model, not these booked rows
(§12).

---

## 6. What leaves the machine

**The fence is spending's.** SPENDING.md §5 describes it, and §6 the
loop it gates. What may leave the machine does not depend on the
direction of the money. `TransferShaped`, `RowTransferShaped`,
`PersonShaped`, `Uninformative` and `FilingOnly` are called the same way
for both families, and each arm refuses what SPENDING.md §5 says.

The fence matters more here. Rails, IBANs and masked contacts are
markers beside a name. An inbound credit transfer often has none of
them, because its narrative IS the sender. `PersonShaped` reads the
name itself. It is on by default
(`spending.categorization.fence_person_names`, inherited with the rest
of the block).

On the income side the model sees what is left of the **deposits**.
Candidacy is that one kind (§7). In practice the model sees employers,
letting agents, agencies and platforms whose names say what they are.

- A business named like a person is fenced with the people. A config
  rule or a pin places it, which is the remedy for every fenced row.
- A gift or a maintenance payment from a relative is fenced and stays
  uncategorised, which is the honest answer.

The person arm cannot tell a person from an organisation whose name
carries no marker, and refuses both (SPENDING.md §5). That cost is
higher here, because on this side the narratives are names.

The context levels are the same three. `income.categorization.context`
accepts `payer` as the income spelling of the narrowest level. It also
accepts `merchant`, so a `spending.categorization` block inherited whole
validates without edits. Internally they are one set of constants: a
level decides how much of a transaction leaves the machine, and that
has one answer per level.

---

## 7. The model tier

**One `categorize`, two families.** The batching, the rolling anchors,
the gauntlet, the per-batch persistence, the retry flush and the fence
are one loop over a family descriptor (SPENDING.md §6). A family
brings a vocabulary, a pair of tables, and the nouns the conversation
uses.

```
wealthdb categorize [spending | income] [-n] [--batch N] [--all | --refine]
wealthdb categorizations [spending | income] [-f FORMAT] [-d VALUE] [--forget SIG]
```

`wealthdb categorize -h` lists the remaining flags. Income-specific
behaviour:

- With no positional, `categorize` runs **both** families, spending
  then income: two plans, two summaries, one model. A name runs that
  family alone.
- `--forget SIG` with no family removes the signature from **both**
  stores and reports per store. One counterparty can sit in both.
- The dump's columns are named for **neither** family: `family`,
  `signature`, `name`, `detailed`. One listing carries both stores, and
  `family` says which vocabulary a row's `detailed` belongs to.
- The income conversation names payers of money received. It offers
  `ModelIncomeCategories()` and forbids `DeltaIncomeCategories()` by
  name. `--refine` re-asks `INCOME_OTHER`, the family's catch-all.
- **Candidacy is `deposit` and nothing else**, in every backlog mode.
  It is the one admitted kind with no floor (§3, decision 13). `--all`
  means *every deposit signature, answered ones included*. It lifts the
  backlog filter and never the kind gate, so it cannot buy a verdict
  for every instrument in the holdings.
- **The backlog is the resolved type being NULL**, over
  `income_txn_categories()` joined to the population. A dividend the
  floor placed has a signature and no stored verdict. With the kind
  gate it is not asked about, because its answer is already known.
- When `income.categorization` is absent it **inherits**
  `spending.categorization` whole: one household, one local model.
  Inheritance is whole-block, not per field. A half-inherited endpoint
  is a configuration nobody wrote down, and its failure would be a
  quiet widening of what leaves the machine.
- **One payer can fold under several signatures.** A payer's narrative
  varies between bookings: a period, a reference, a department. So a
  tax office or an employer reaches the store as several keys, each
  asked about on its own, and the verdicts can disagree. A config rule
  on the narrative places every key at once and outranks the model. It
  is the fix; a pin per signature is only a patch.

---

## 8. Reading it: `wealthdb income <view>`

```
wealthdb income <view> [FROM [TO]] [--period P] [--level L]
                       [--rates R] [--maturing M] [--shift BP]
                       [-f FORMAT] [-C COLS] [-x CCY] [-p]
```

The flags and their defaults are in [DESIGN.md](DESIGN.md) §4.13 and
`wealthdb income -h`. The table below covers the views of what was
received; §12 covers the forward views and their flags.

| view | a row is | default columns |
|---|---|---|
| `summary` | a period bucket | `period, txn_count, income, reversals, net_income` (+ `withheld` via `-C`) |
| `types` | a (bucket, type) pair with its share | `period, type, txn_count, income, reversals, net_income, share` (+ `type_id`) |
| `transactions` | an income line, oldest first | `silver_source, date, account, kind, payer, income_type, provenance, currency, net_amount, value` |

The window defaults to the **trailing twelve months**, as spending's
does. A positional year reads a calendar year:
`wealthdb income types 2025 --period annual`.

`--level` defaults to **`detailed`**, the one default that differs from
spending's. The income vocabulary has one vendored primary. At the
primary level every vendored type and extension folds into `INCOME`,
beside the deltas the base keeps (§2) and `(uncategorized)`. That view
shows what was earned or yielded against what was given, but it is not
what a reader opens the report for.

`income` and `reversals` are positive **magnitudes**, and `net_income`
is the difference. This mirrors spending's `spend` / `refunds` /
`net_spend`. The `transactions` view keeps canonical signs.

**Privacy.** `payer` takes the free-text class, as `merchant` does and
for the same reason. Step 4 of §4 publishes the fold of a narrative,
which can carry a person's name. The fence gates the store, not the
column. Step 2 fills the column with an instrument name for most rows,
but a column takes the class of the worst thing it can hold.
`payer_signature`, `counterparty` and `description` redact with it.
A typed name, such as the account nickname or a category from the
config, prints as a pseudonym ([DESIGN.md §4.6](DESIGN.md), *Typed
names*). An account name that may be typed shows the account's id
instead. That id redacts where it is shaped as one, and prints as a
pseudonym otherwise. Type, provenance and `share` stay legible.

`wealthdb transactions` carries `payer`, `income_primary` and
`income_detailed` behind `-C`. A transaction can carry both families'
verdicts, and often does: a deposit the matcher paired is
`internal_transfer` in each overlay. That view is the one surface that
shows a row from both sides at once.

**Metabase.** An **Income** dashboard and its privacy twin. Its cards
of the income lines take the Spending dashboards' shape. Beside them it
reads the forward views (§12) over every income line. It shows
received and expected income by type, month, account and position, the
withholding, and a position's expected payments. Its Time range sets
what was received and its Horizon what is expected. Its Rate path,
Maturing and Shift (bp) pickers are the forward views' assumptions.
[web/DESIGN.md](../../web/DESIGN.md) §6 has the detail.

---

## 9. Storage and lifecycle

Everything in SPENDING.md §8 holds, with the income names:

| table | grain | survives `reset`? | carried by `reload -a`? |
|---|---|---|---|
| `income_txn_enrichment` | per transaction, per source | no | n/a, re-derived |
| `income_payer_categories` | global, per signature | yes | **yes** |
| `income_account_scope` | config, stamped into gold | yes | re-stamped from config |

`reload -a` carries the payer store across beside the merchant store. A
store that is not carried is lost with no backup, so the rebuild walks
a list of stores rather than naming one. After carrying a store, the
rebuild respells any retired value in it to its successor (§2). So a
carry from a file older than migration 0111 brings no retired spelling
into the rebuilt file.

`wealthdb status -v` prints an `income:` block with the backlog count.
The catch-all-kind counter (`excluded_unmapped`) is not repeated per
family. It joins `spend_scoped_accounts()`. Where the two scopes agree,
which is the default, one number answers for both.

---

## 10. Decisions of record

1. **Three views of what was received**: `summary`, `types`,
   `transactions`, beside the six forward views (§12). No `payers` view:
   payers rank on the dashboard only, as merchants do.
2. **Plaid's `INCOME` vendored verbatim**, version 2, plus eight
   extensions and fourteen deltas. The vocabulary names what a
   household commonly receives, not what one deployment does, because
   the product is published.
3. **Gross as booked.** `withheld` is a memo, off by default, never
   read by `net_income` (§5).
4. **A private fund's `distribution` is excluded until proven income.**
   The floor places `capital_return`, and a rule or a pin promotes the
   exception. `capital_gain` stays income, because it returns no basis.
5. **The instrument is the payer** for a row carrying one. The column
   keeps the free-text privacy class regardless.
6. **One `categorize`, two families**, with a separate payer store
   keyed by the shared signature.
7. **Own account scope.** `income.categorization` inherits
   `spending.categorization` when absent.
8. **One matcher.** Income pairs nothing and reads the shared verdicts.
   `spending.transfer_overrides` serves both families.
9. The Income dashboard's cards read the income base.
10. **Reimbursements are excluded** via the delta. Spending nets each
    one it can link against the outflow it answers.
11. Window default: **trailing twelve months**; on the forward views,
    the projection's horizon after the current month (§12).
12. **Reversals net inside their type.** `interest` enters positive
    only, because the sign is the only thing that separates two
    opposite events. `deposit` takes both signs like every other income
    kind (§1).
13. **The model decides the kind of income only where the data does
    not.** On the spending side the model names and files the merchant,
    and may never decide that a card purchase was an investment. On the
    income side it is asked what kind of income a receipt is, which is
    a claim about the transaction. The data makes that claim itself on
    every admitted kind but `deposit`. So two independent guards apply:
    the kind floor is read over the payer store (migration 0073, §3),
    and candidacy is restricted to `deposit` in every backlog mode (§7).
14. **The projection reads today's holdings.** A past month shows what
    was received, never what the holdings of then were expected to pay.
15. **Expected income starts the day after today and accrues by day.**
    A payment counts on its day, in full. A receipt booked today is
    received.
16. **`withheld` on the forward views is the withholding table's
    model**, received and expected alike. The booked tax is `summary`'s
    memo (§5). The dashboard's Withholding tile reads that memo for what
    was received and the model for what is expected.
17. **A liability earns nothing** in the projection.
18. **A convertible note pays nothing.** A loan held accrues its coupon
    month by month.
19. **The roll term never starts from an issue date**, because no source
    states one.

---

## 11. Known limits

- **A rewards credit reaches gold as `refund`.** Card adapters map a
  statement credit to `refund`. Once the issuer's descriptor is gone
  they cannot tell it from a merchant credit. No adapter emits the
  `reward` kind, so its income floor never fires. An adapter that
  learns the difference fixes this, not a gold migration.
- **The matcher pairs a deposit with its own same-amount reversal**
  when the two fall inside its window. Both legs then read
  `internal_transfer` and leave the base, rather than netting inside
  the payer's type. The net is the same either way; the account of it
  is not. The remedy, if one is wanted, is the transfer-override ledger
  that exists for every other matcher false positive.
- **A balances-only source has no transactions.** A source whose
  bronze holds statements read for balances alone contributes none.
  Every funding wire it sent has no leg for the matcher to pair. The
  receiving leg stays in the income base as an unplaced receipt (§1),
  except on a custody account, where it reads `capital_return`.
  `income.accounts.exclude` takes the receiving account out.
- **Only the transaction report has a `_multi` form.** The same holds
  for spending. The web reads the lines in every reporting currency and
  buckets them itself, so a summary or types `_multi` would have no
  reader. An unread macro is a shape to keep true for nothing. A
  Metabase card that needs either in every reporting currency would add
  it.

---

## 12. The forward views

The forward views put what was received beside what the holdings are
expected to pay. They read today's holdings and the yield report's
figures for them ([YIELD.md](YIELD.md)). Their window may reach into the
future. A day up to today shows what was received, and a day after it
what is expected. The current month holds both halves.

| view | a row is |
|---|---|
| `projection` | a calendar month, quarter or year of the window, then `total` and `monthly_average` |
| `calendar` | an expected payment, or a principal a maturity returns, in date order |
| `sources`, `portfolios`, `accounts`, `positions` | one entity's income over the window |

`wealthdb income -h` lists every view's columns and its default set.
Behind `-C`, `projection` adds `total` (received and expected), `rent`,
`pension` and `principal`. It also splits each type into
`<type>_received` and `<type>_expected`.

`wealthdb cashflow projection` sets this projection against the
household's outflows month by month: the cash flow projection
([CASHFLOW.md](CASHFLOW.md) §12).

### The window and the period

- FROM and TO take the dates the other views take (§8).
- A lone FROM is its own span: `income projection 2027` is all of 2027.
- An open bound (`-`) is refused. Ahead of today it has no meaning.
- With no bound, the window is the projection's horizon after the
  current month. That is `yield.horizon_months` of the config: 12 by
  default, at most 36 ([YIELD.md](YIELD.md) §8).
- The projection covers the current month and the 36 after it. A
  window that ends later is refused, and the message names the
  projection's last day.

Of the forward views, only `projection` takes `--period`. `calendar`
and the entity views refuse it. `projection` takes `monthly`,
`quarterly` or `annual`, and refuses the rest, `total` among them. The
periods are calendar periods, so the window cuts the first and the
last. The rows end with `total`, the window's sum, and
`monthly_average`, that sum over the window's months. A month the
window cuts counts its share of days.

`--rates`, `--maturing` and `--shift` belong to the forward views.
`summary`, `types` and `transactions` refuse them, and the forward views
refuse `--level`.

The MCP `income` tool follows the same rules:

- it takes the three flags as `rates`, `maturing` and `shift`;
- it takes `period` on `projection` alone;
- it refuses `-` in `from` or `to`;
- a lone `from` is its own span, and a lone `to` starts the window on
  the first day of next month.

### Received and expected

The received half is the window's income lines up to the end of today.
A line booked today counts as received, at any hour. It counts the six
types the projection knows: interest, coupons, dividends,
distributions, rent and pension. Interest a bond pays reads `coupons`,
but a bill's or a zero's discount reads `interest`. The bond's terms in
force at the end of the received half tell the two apart, a ledger
record's among them. A dividend a fund pays reads `distributions`.
Wages, gifts and the other types stay in the other three views. A line
counts under the holding it belongs to, else under its account.

The Income dashboard's two halves are wider than these views':

- its received half counts every income line, wages and gifts among
  them, each other line under its account and by its operating-in
  class ([CASHFLOW.md](CASHFLOW.md) §5);
- its expected half adds the other income's run-rate: the cash flow
  projection's `other_income`, which gold keeps by class and account
  ([CASHFLOW.md](CASHFLOW.md) §12). It covers the household's accounts
  alone.

`wealthdb income projection` reads the six types alone.

The expected half starts the day after today.

- Income that accrues covers its days exactly. A month that the window,
  today or a maturity cuts takes its share of days. A balance, a
  fund's yield, a floating coupon and a loan's interest accrue. A
  floating fund whose distributions cover the year, and pay less often
  than monthly, pays its accrual on its pay days instead (below).
- A payment counts on its day, in full: a fixed coupon, a bill's
  discount, a dividend.
- The day of a dividend or of a repeated payment is an estimate. The
  calendar shows its month only.

### What each holding is expected to pay

[YIELD.md](YIELD.md) §3 names the kinds.

| kind | expected income |
|---|---|
| `deposit` | its stated rate on its balance, as a floating rate that keeps its spread to the benchmark |
| `term_deposit` | its rate to its end day; then the maturing rule |
| `money_market` | its yield, as a floating rate |
| `floating_note` | the benchmark plus its spread, on its face, to maturity |
| `bond` | its coupons on their dates (`declared`); its face at maturity |
| `bill`, or a zero | its face less its price, on its maturity day |
| `note` | a loan held: its coupon on its amount, month by month, then its principal at maturity; a convertible note: nothing |
| `bond_fund` | a floating fund ([YIELD.md](YIELD.md) §3): a floating rate (below); any other: its indicated distributions, else its distribution or SEC yield, else its trailing yield, month by month, else `unknown` |
| `equity_fund`, `multi_asset` | its indicated distributions; else the trailing yield, or the distribution yield, its page states, month by month |
| `stock` | its instrument's indicated dividend ([YIELD.md](YIELD.md) §4) on its own quantity: a ledger record's (`declared`), else the last month's payment per share, specials aside, at the frequency of its pay months per month held, else its filings' dividends per share, quarterly (`indicated`) |
| `liability` | nothing |
| `other` | its payments of the last twelve months, repeated (`realized`); paid nothing, `unknown` |

Where a fund held under eleven of the twelve months shows one calendar
month of distributions, the yield its page states wins, if it states
one ([YIELD.md](YIELD.md) §4).

- A floating bond fund earns a floating rate, the first of these:
  - its implied yield;
  - the yield its indicated distributions pay, where they cover the
    year ([YIELD.md](YIELD.md) §4);
  - its distribution or SEC yield;
  - its trailing yield.

  Two readings of its distributions do not cover the year:
  - one half of a payer twice a year whose other half has no rate;
  - one calendar month of a fund held under eleven months.

  A fund whose distributions cover the year and pay less often than
  monthly pays its floating rate on its own pay days. Each payment is
  what its value accrued since the pay day before, at the rate of the
  payment's month. That rate moves with the curve and the shift. So on
  a rising forward curve a quarterly payer earns a little more than a
  monthly one. Any other floating fund accrues its rate month by month.
  With none of the rates, it pays its indicated distributions as they
  come. Else it reads `unknown`.
- A balance whose rate no source states repeats the interest its
  account received in the last twelve months (`realized`). With none
  received it reads `unknown`.
- A negative balance earns nothing.
- A bond, bill, floating note or note in default reads `unknown`. It
  pays no coupon and returns no principal.
- An account's income that names no holding, such as rent or a pension,
  repeats too (`realized`). It reads as the holding
  `income:<type>:<CCY>`, one per currency.
- Interest that names no holding but whose description names a bond is
  a coupon ([YIELD.md](YIELD.md) §4): the coupon of the bond of its
  account that it names. Where the account holds none, one that names a
  bond issue is the account's `income:coupons:<CCY>`, received but not
  repeated.
- A repeated payment falls on its day of every later year the
  projection reaches, not only the next. This holds for rent, pensions,
  a rate-less balance's interest and an `other` holding's payments.

A bond fund projected from its lines' yield earns nothing on a line
paid in kind ([YIELD.md](YIELD.md) §3). One projected from its
distribution yield is unaffected.

### When a holding matures

A holding that matures in the window returns its principal on its
maturity day. A holding a source still lists on or after its maturity
returns its principal tomorrow. `calendar` lists the principal as kind
`principal`, outside the income totals, and `projection -C +principal`
sums it per period. `--maturing` says what the principal earns from
that day on:

- `cash`, the default: what the cash of the holding's own account, in
  its currency, earns on it from that day. That is the account's
  marginal rate on the balance with the principal, its tiers applied,
  along the rate path. A term's expired rate never counts.
- `idle`: nothing.
- `roll`: the same holding again, for its original term. It buys at the
  forward rate of that term plus its own spread over the curve today,
  and buys again at each maturity.

The account's rate counts though the account holds no cash in that
currency today, where the rate names the currency. A rate that names
none is the rate of the cash the account holds. Cash whose account
states no rate leaves the reinvested income `unknown`. The holding itself keeps its own reading,
so it does not turn `unknown`. `yield coverage` lists it under `no
receiving rate` ([YIELD.md](YIELD.md) §10).

The term is the maturity less the day the term began: a term deposit's
rate's first day, else the day the holding was bought. Without either,
it is the term left today. It is at least a month. No source states an
issue date, so the term never starts from one.

A roll falls back where it lacks a figure, in this order:

1. a holding with no yield at all goes to cash (`roll_cash`);
2. a holding with no implied yield rolls at its current yield, else its
   stated rate, held constant (`roll_own`);
3. in a currency with no curve, or one whose curve cannot be built, it
   rolls at its own yield, held constant (`roll_constant`);
4. any other holding rolls along the curve (`roll`).

A roll keeps the holding's income type. A term deposit or a bill earns
interest, and a bond earns coupons. Cash after a maturity earns
interest.

### The rate path and the shift

- `--rates constant`, the default, holds today's benchmark for the
  whole window. A roll then earns the holding's own yield.
- `--rates forward` moves the benchmark along the currency's forward
  curve ([YIELD.md](YIELD.md) §8). Each floating holding keeps its
  spread to it, and a fixed coupon does not move. A currency with no
  curve, or one whose curve cannot be built, keeps today's rate.
- `--shift BP` moves every floating rate in parallel, by whole basis
  points. It moves a balance, a money-market fund, a floating note, a
  floating bond fund that earns a floating rate (the `bond_fund` row
  above), cash after a maturity and a rolled holding.
- A floating rate never earns below zero. A floating holding's rate,
  the benchmark on the path plus its spread and the shift, floors at
  zero, and so does a roll's rate with the shift. So a balance whose bank
  charges for it reads nothing, not a charge.

### What an amount rests on

`basis` says what each expected amount rests on:

| basis | the amount rests on |
|---|---|
| `declared` | a rate a ledger record states, a coupon by contract, a bill's discount |
| `indicated` | the last payment at its frequency, or the filings' dividends |
| `stated` | a rate a source states, held |
| `forward` | a stated, declared or indicated floating rate, moved along its currency's forward curve |
| `reinvested` | what a principal earns after a maturity |
| `realized` | last year's payments, where nothing else is known |
| `unknown` | nothing; the amount is zero |

`declared` needs a ledger record of the rate itself. A record that
states only a fund's duration leaves its filed yield `stated`. A
floating rate that no curve moves keeps its basis under
`--rates forward`.

On `projection`, `basis` gives each basis's share of the expected
income (`declared=76% stated=24%`). An entity view gives the largest,
and `calendar` each row's own.

`quality` gives the share of the reinvested income each way of
reinvesting earns: `cash`, `roll`, `roll_constant`, `roll_own`,
`roll_cash`. A way that is the only one prints without a share. One
that earns nothing beside another prints a share of zero:
`roll=100% roll_cash=0%`. Where income rests on a floating rate and a
shift is given, `shift=-50bp` follows.

`withheld` is what the `yield.withholding` table withholds at each
holding's situs and account wrapper ([YIELD.md](YIELD.md) §8). A
country's `default` row stands for any other wrapper. It reads the same
for received and expected income. It is a model, not the tax rows a
source booked, and it is blank where the table names no rate. The
booked tax is `income summary -C +withheld` (§5).

### The calendar and the entity views

`calendar` lists every expected amount in date order. A dated amount
prints its day. An accrual prints its month. So does a payment whose
day is an estimate. Both follow the holding's dated amounts of that
month. Each principal is a row too. No row is a payment of zero.

The entity views give, per entity:

- `received`, `expected` and `total` over the window;
- `last_year` (behind `-C`): what the same window a year earlier
  received. It is blank until that window is over, and zero where it is
  over and nothing came;
- `next_12m`: what the twelve months after the current one are expected
  to pay;
- `withheld` (behind `-C`), `basis` and `quality`.

`positions` lists every holding, the ones that pay nothing included. It
also lists any holding no longer held that received income in the
window, or in the same window a year earlier. Such a holding's income
is withheld at its security's situs, by its own account's rate or
wrapper. On
the dashboard it carries no symbol, no name and asset class `unknown`.
Its rows sort by source and account, then by `total` plus `next_12m`,
the largest first. The other entity views sort by that sum alone.

Every month's expected income equals the sum of its calendar rows of
kind `income`, and every entity view sums to the projection. All of
them read the same amounts, so they reconcile by construction.
`positions` and `calendar` name a holding by one key: its source, its
account and its position key.

**Privacy.** `-p` stars amounts, quantities, prices and account ids.
Every name a person typed prints as a pseudonym, such as `manual-1`, or
as its account's redacted id (DESIGN.md §4.6). That covers a typed
account or portfolio name, and a manual holding's name and key. Any
other `position_key` stays legible, as `holdings positions` keeps it.
Shares, rates, bases and labels stay legible too.

An invented household, on 2026-11-15, in Swiss francs:

```
$ wealthdb income projection 2026 --period quarterly -x CHF
 period          | received_CHF | expected_CHF | interest_CHF | coupons_CHF | dividends_CHF | distributions_CHF | withheld_CHF | status   | basis                                               | quality
-----------------+--------------+--------------+--------------+-------------+---------------+-------------------+--------------+----------+-----------------------------------------------------+---------
 2026-Q1         |       620.00 |         0.00 |       120.00 |        0.00 |        200.00 |            300.00 |        30.00 | received |                                                     |
 2026-Q2         |      1620.00 |         0.00 |       120.00 |     1000.00 |        200.00 |            300.00 |        30.00 | received |                                                     |
 2026-Q3         |       620.00 |         0.00 |       120.00 |        0.00 |        200.00 |            300.00 |        30.00 | received |                                                     |
 2026-Q4         |       100.00 |      1487.00 |       137.00 |     1000.00 |        200.00 |            250.00 |        30.00 | mixed    | declared=71% indicated=13% stated=14% reinvested=1% | cash
 total           |      2960.00 |      1487.00 |       497.00 |     2000.00 |        800.00 |           1150.00 |       120.00 | mixed    | declared=71% indicated=13% stated=14% reinvested=1% | cash
 monthly_average |       246.67 |       123.92 |        41.42 |      166.67 |         66.67 |             95.83 |        10.00 |          |                                                     |
(6 rows)

$ wealthdb income calendar -x CHF | head -8
 date       | silver_source | account   | symbol | type      | kind      | currency |    amount | value_CHF | basis      | quality
------------+---------------+-----------+--------+-----------+-----------+----------+-----------+-----------+------------+---------
 2026-12-15 | broker-1      | Brokerage | QB     | coupons   | income    | USD      |   1250.00 |   1000.00 | declared   |
 2026-12-16 | bank-1        | Term      |        | interest  | principal | CHF      | 100000.00 | 100000.00 | declared   |
 2026-12    | bank-1        | Savings   |        | interest  | income    | CHF      |     40.00 |     40.00 | stated     |
 2026-12    | bank-1        | Term      |        | interest  | income    | CHF      |     30.00 |     30.00 | declared   |
 2026-12    | bank-1        | Term      |        | interest  | income    | CHF      |     16.00 |     16.00 | reinvested | cash
 2026-12    | broker-1      | Brokerage | QSTK   | dividends | income    | USD      |    250.00 |    200.00 | indicated  |
```

The term deposit returns its principal on 2026-12-16. It earns its own
rate to that day. Its term's rate then ends. Under the default
`--maturing cash`, the principal earns the account's credit rate from
that day, which the account states beside the term. The calendar starts
in December: with no window it covers the horizon after the current
month.
