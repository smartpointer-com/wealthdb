# Income

What the tracked accounts received: wages, interest, dividends, staking
rewards, rent, gifts. Income is consolidated across every source, typed,
and attributed to a payer.

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
- **Netting reimbursements against spending.** An expense reimbursement
  is money back for money spent. Income excludes it. Netting it on
  spending's refund side is an open follow-up (§11).
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

When it is not, the matcher has nothing to pair. The leg stays in the
base as an unplaced receipt. That is the honest answer for a movement
whose other half the product cannot see:

- a source loaded for balances alone contributes no transactions;
- a greedy one-to-one matcher with a missing partner pairs nothing
  rather than guessing.

Widening the matcher's window or tolerance to reach these legs buys
false pairs faster than real ones. Funding amounts are round numbers
that recur across banks.

The remedy is `income.accounts.exclude`. A platform's wallet and custody
accounts are not accounts whose inflows are income, and the income scope
exists for this kind of reason. §11 records the ingestion follow-up and
the general fix.

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
| `capital_return` | the holder's own capital coming back | no |
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
provenance vocabulary is the same seven values ([SPENDING.md §7](SPENDING.md)). One
step is **re-ordered**: the kind floor is read over the model, not
under it.

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

- **Built-in rules.** There is one: `cash_deposit`, from a narrative
  naming a counter or machine deposit. Almost every structural inflow
  fact is the matcher's already. What is left is cash paid in, which
  has no counter-leg anywhere. The rule is **gated on `deposit`**, the
  one admitted kind the floor does not answer. The rule tier sits above
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
                       [-f FORMAT] [-C COLS] [-x CCY] [-p]
```

The flags and their defaults are in [DESIGN.md](DESIGN.md) §4.13 and
`wealthdb income -h`.

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
Type, provenance and `share` stay legible.

`wealthdb transactions` carries `payer`, `income_primary` and
`income_detailed` behind `-C`. A transaction can carry both families'
verdicts, and often does: a deposit the matcher paired is
`internal_transfer` in each overlay. That view is the one surface that
shows a row from both sides at once.

**Metabase.** An **Income** dashboard and its privacy twin, in the
Spending dashboards' shape. [web/DESIGN.md](../../web/DESIGN.md) §6 has
the detail.

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

1. **Three CLI views**: `summary`, `types`, `transactions`. No `payers`
   view: payers rank on the dashboard only, as merchants do.
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
9. The Wealth Overview's investment-income card reads the income base.
10. **Reimbursements are excluded** via the delta. Netting them against
    spending is an open follow-up (§11).
11. Window default: **trailing twelve months**.
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

---

## 11. Open follow-ups

- **The package name.** The deterministic pass for both families lives
  in `internal/spending`. Renaming it (`internal/enrichment`) would
  touch every import for no behavioural gain.
- **Reimbursement netting** on the spending side. An expense claim is
  money back for a specific outflow, and spending's refund side is
  where it would net. Income excludes it; the netting is not designed.
  An insurance payout is NOT this case and is income (§2).
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
- **Transactions for a balances-only source.** A source whose bronze
  holds statements read for balances alone contributes no
  transactions. Every funding wire it sent has no leg for the matcher
  to pair, and the receiving leg stays in the income base as an
  unplaced receipt (§1). Parsing those statements into transactions
  fixes it at the root and needs no income change. It is collector
  work. `income.accounts.exclude` is the remedy until then.
- **A built-in income rule for platform custody deposits.** A `deposit`
  on a private-market custody account is the holder's own capital going
  in, and should read `capital_return`. This is the general version of
  the exclusion above. It needs something the rule hook does not carry:
  the account kind and the source, not a narrative.
- **No `report_income_summary_multi`.** Spending publishes a
  multi-currency sibling for all three of its reports; income for two.
  Nothing reads either summary `_multi`, so the gap stays. An unread
  macro is a shape to keep true for nothing. A Metabase card that needs
  the summary in every reporting currency would close it.
