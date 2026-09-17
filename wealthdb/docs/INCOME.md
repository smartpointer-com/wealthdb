# Income

What the tracked accounts received: wages, interest, dividends, staking
rewards, rent, gifts — consolidated across every source, typed, and
attributed to a payer.

This document is a **delta against [SPENDING.md](SPENDING.md)**. The two
features are one engine read in two directions: one enrichment pass, one
precedence lattice, one internal-transfer matcher, one signature
normaliser, one model-tier protocol. Where a mechanism is shared, this
file says so and points at the section that explains it, rather than
explaining it twice — a second explanation is a second thing to keep
true.

---

## 1. Scope: inflows only

Income is **what arrived**, on any account the product tracks, that the
household did not already own.

That last clause is the whole of the difficulty, and §2's deltas are
where it is written down. Money arriving is not automatically income: a
funding wire between two of the holder's own accounts arrived, a private
fund returning contributed capital arrived, a loan being disbursed
arrived. None of them is earnings, and each leaves the base by a
different route.

What is deliberately **not** here:

- **Investment buys and sells**, maturities, corporate actions and FX
  legs — the future `cashflow` feature. Sale proceeds are not income
  under any reading and never enter the base.
- **Gross-of-payroll wages.** A bank sees the net salary; the gross, the
  deductions and the employer's contributions live on a payslip the
  product does not collect. Income is what arrived (§5).
- **Netting reimbursements against spending.** An expense reimbursement
  is money back for money spent. It is excluded from income here and
  belongs to spending's refund side later — a recorded follow-up (§11).
- **Tax reporting.** Withholding is visible beside income (§5) but no
  view claims to be a tax figure; jurisdictions differ on staking, on
  capital-gains distributions, on gifts.
- **Accrued but unpaid income.** Cash basis throughout, like spending.

### The population, layered

Three macros where spending has four, each narrower than the one it
reads, for the reason SPENDING.md §1 gives: the pass decides what the
base excludes, so it cannot read a base that already excluded it.

| macro | what it is |
|---|---|
| `income_scoped_accounts()` | every account, until an `income.accounts` entry takes one out |
| `income_enrichment_population(f, t)` | what the pass may write a verdict for |
| `income_lines_base(f, t)` | what a view charts: the population with its type resolved, less the four excluded deltas |

There is **no income matcher pool**. The internal-transfer matcher
already admits `deposit` on every account (migration 0044) precisely so a
funding wire pairs, and already writes `internal_transfer` on both legs.
Income reads those verdicts; it pairs nothing (§10, decision 8).

### Platform wallets and custody accounts

A `deposit` on a private-market platform — an investment wallet, a
custody account holding an SPV or an exercised grant — can only be the
holder's own money arriving to be invested. Such a platform pays OUT as
a `distribution`, which the floor already takes out of the base. So an
inbound leg there is a funding leg, and the matcher removes it as an
own-account move **when the funding bank's side of it is in gold**.

When it is not, the matcher has nothing to pair and the leg stays in the
base as an unplaced receipt. That is the honest answer for a movement
whose other half the product cannot see, and it is not a bug in the
matcher: a source loaded for balances alone contributes no transactions,
and a greedy one-to-one matcher given a missing partner correctly pairs
nothing rather than guessing. Widening the window or the tolerance to
reach these buys false pairs faster than real ones — the amounts are
round numbers that recur across banks, and half the near-misses have the
bank leg AFTER the credit.

Until the funding side is ingested, `income.accounts.exclude` is the
remedy: the platform's wallet and custody accounts are not accounts
whose inflows are income, and the income scope is its own for exactly
this kind of reason. §11 records the ingestion follow-up and the
general-audience fix.

### Which kinds are income

The design turns on where each transaction kind lands. Income selects
rows by kind, not by account, exactly as spending does.

| kind | where it goes |
|---|---|
| `dividend` | in — floor `INCOME_DIVIDENDS` |
| `coupon` | in — floor `INCOME_INTEREST_EARNED` |
| `staking` | in — floor `INCOME_STAKING` |
| `capital_gain` | in — floor `INCOME_DISTRIBUTIONS`; a public fund's payout of realised gains returns no basis |
| `reward` | in — floor `INCOME_REWARDS` |
| `distribution` | in the population, **out of the base** — floor `capital_return`; a private fund returns contributed capital first |
| `deposit` | in — **no floor**; the one kind the narrative tiers decide |
| `interest` | in when **positive**; the negative side is a finance charge and is spending's |
| `refund` | spending's — it nets inside the merchant's category there |
| `sell`, `contribution`, `transfer_in`, `card_payment`, `corporate_action`, `journal`, `other`, the FX kinds | out |

Seven of the eight admitted kinds carry **both signs**, so a negative row —
a dividend clawed back, a credit reversed — nets against what it reverses
inside its own type. On `capital_gain` and `staking` a negative row need
not be a reversal at all (a realised loss, a slashing penalty), and the
same arithmetic nets it in the same place, which is the right answer
either way.

`interest` is the one kind with a **sign guard**, and the guard is not a
policy about reversals: one canonical kind carries two opposite events —
interest credited and interest charged — and the sign is the only thing
that tells them apart, with spending having claimed the negative half in
migration 0041. `deposit` takes both signs like every other income kind:
its canonical sign is positive and `ApplyCanonicalSign` forces it, so the
only route to a negative one is an adapter deliberately bypassing that
helper on a reversal marker (`canonical/sign.go`), which makes a negative
`deposit` a reversal by construction.

---

## 2. The taxonomy

**Same provenance, same three classes, same predicates** as
SPENDING.md §2, in one table and one dimension. `spend_categories` gained
a `family` column in migration 0069: a row is `spending`, `income`, or
`both`.

**Vendored, verbatim:** Plaid's `INCOME` primary and its seven detailed
values, copied with descriptions untouched so a taxonomy refresh still
diffs cleanly — `INCOME_WAGES`, `INCOME_INTEREST_EARNED`,
`INCOME_DIVIDENDS`, `INCOME_RETIREMENT_PENSION`, `INCOME_TAX_REFUND`,
`INCOME_UNEMPLOYMENT`, `INCOME_OTHER_INCOME`. The other three dropped
primaries stay dropped: `TRANSFER_IN` is the own-account move the matcher
already names, and the two outflow families are spending's.

**Nine extensions**, ours, in the vendored shape under `INCOME` so the
model may emit them and a future Plaid value supersedes one as a clean
diff: `INCOME_SELF_EMPLOYMENT`, `INCOME_GOVERNMENT_BENEFITS`,
`INCOME_RENT`, `INCOME_ROYALTIES`, `INCOME_ALIMONY_AND_CHILD_SUPPORT`,
`INCOME_STAKING`, `INCOME_REWARDS`, `INCOME_DISTRIBUTIONS`,
`INCOME_INSURANCE_PAYOUT`. The bar each
clears is the bar an extension always clears — common, distinct on a
statement or a tax return, and absent from the vendored vocabulary — and
it is applied to households in general rather than to one, because the
product is published.

`INCOME_INSURANCE_PAYOUT` is the one whose placement is not obvious, since
`reimbursement` below also describes it. It is income because **the premium
was already counted as spending** and nothing in the data links a payout to
the premiums it answers — different amounts, different dates, often
different years. Netting the payout out would count the outflow and drop the
inflow. `reimbursement` keeps the cases where the outflow IS identifiable: a
utility credit against a bill, a merchant reversing its own charge.

**Twelve deltas**, ours, primary-level and lowercase, decided from
structure a payer's name cannot reveal:

| value | meaning | in the base? |
|---|---|---|
| `internal_transfer` | movement between two tracked accounts, either leg | no |
| `capital_return` | the holder's own capital coming back | no |
| `loan_proceeds` | money borrowed arriving; a liability incurred | no |
| `reimbursement` | money back for money spent | no |
| `retirement_transfer` | a payout from, or a contribution to, an untracked retirement plan | no |
| `education_transfer` | the same for an education plan or savings account | no |
| `health_transfer` | the same for a health savings account | no |
| `trust_transfer` | the same for a trust that is a separate taxpayer | no |
| `gift` | a cash gift or family support received | yes |
| `inheritance` | an estate's distribution to the holder | yes |
| `cash_deposit` | cash paid in at a counter or a machine | yes |
| `other` | a receipt no tier could place, that a tier nonetheless placed there | yes, labelled `Other` |

Seven of them — `internal_transfer`, `gift`, `other` and the four
crossings — are **one row** read from either side, which is what
`family = 'both'` means.

The four crossings arrived with the cash flow statement
([CASHFLOW.md](CASHFLOW.md), migration 0078) and leave the base for
`internal_transfer`'s reason: the money is the holder's throughout, in
a pool earmarked for a purpose rather than earned. The DIRECTION is the
row's own, so a deposit placed `retirement_transfer` is a plan payout
and a withdrawal placed the same is a contribution. None of them is
needed where the far account IS tracked — the matcher's verdict says
everything, and it outranks a rule.

`other` is **not** `(uncategorized)`, and the difference is the backlog.
`other` is a stored value with a label of its own: a tier looked at the
row and placed it there, and the row is out of the model tier's backlog
for good. `(uncategorized)` is what a row whose resolved type is NULL
reads as — nothing placed it, and it is exactly what the next
`categorize income` run asks about. A rule that files a class of receipt
as `other` therefore removes it from the backlog rather than leaving it
there.

Spending's `card_spend` has no mirror, deliberately. It exists because
deleting an unpaired card bill deletes real consumption; an unpaired
inbound wire is not deleted — it stays in the base, visible and unplaced,
which is the honest answer.

**Two validity predicates**, as on the spending side and for the reason
SPENDING.md §2 gives: `ValidIncomeDetailed` admits everything storable,
`ModelIncomeDetailed` refuses the deltas. Both are derived from the
`family` column rather than restated, so a value added to `canonical` is
admitted or refused everywhere at once. The predicates are **fenced by
family**: a spending rule naming `INCOME_WAGES` fails at config load, and
so does an income rule naming `FOOD_AND_DRINK_GROCERIES`.

---

## 3. The tiers and the precedence lattice

The lattice is spending's in every tier the pass writes, and the
provenance vocabulary is the same seven values (SPENDING.md §3). One
step is **re-ordered**: the kind floor is read over the model, not under
it.

```
spending:  pin > matcher > built-in rule > config rule > provider > model > kind floor
income:    pin > matcher > built-in rule > config rule > provider > KIND FLOOR > model
```

What differs is the **weight each tier carries**, and the re-order
follows from it. On the outflow side the narrative decides nearly
everything and the kind floor is a backstop. On the inflow side the
floor answers for every kind the population admits except one — a
`dividend` row is dividend income whatever its narrative says — and the
narrative tiers exist for `deposit`, which has no floor because nothing
but the narrative can say what it was.

- **Built-in rules** — one: `cash_deposit`, from a narrative naming a
  counter or machine deposit. The asymmetry is the design rather than an
  omission: almost every structural inflow fact is the matcher's already,
  and what is left is cash paid in, which has no counter-leg anywhere.
  It is **gated on `deposit`**, the one admitted kind the floor does not
  answer. The rule tier sits above the floor on both sides — a config
  rule promoting a `distribution` depends on it — so an ungated phrase
  match would not merely add a verdict, it would replace a right one:
  a call account's credited interest narrated "INTEREST ON CASH
  DEPOSIT" would read as cash paid in over a counter. Config rules are
  NOT gated; a rule is the holder's own instrument, and saying
  something the data does not is its whole purpose.
- **Config rules** — `income.rules[]`, the holder's own, with the value
  field named `type`. Same shape as `spending.rules`, scope included.
- **Provider tier** — a bank's booking type says "salary", "dividend",
  "interest"; the provider maps gained an income side. Per (silver kind,
  account kind), as on the spending side. One booking type can mean
  different things by direction — UBS books both halves of an account's
  interest settlement under one type — which is why the two maps are
  separate rather than one lookup.
- **Model tier** — asked about the payer signatures on `deposit` rows
  the deterministic tiers left unplaced, and about nothing else (§7).
  **The fence is identical** and matters more here (§6).
- **Pins** — `income.pins`, the spending ledger's format with
  `income_detailed` where `spend_detailed` was. SPENDING.md §3's *Pins*
  section is the whole of the mechanics, including what happens to a
  pin naming a transaction this family's population does not admit: it
  is written, counted as matched, and then seen by nothing but
  `wealthdb transactions`.

The deterministic pass is folded into `wealthdb load`, and it is **one
pass writing both families** in one transaction: one matcher run, both
overlays deleted and re-asserted, both verdict stores re-keyed, one
commit. Only the model tier is explicit.

### The floor above the model (migration 0073)

The two families ask a model **different questions**, and which of the
two outranks the other follows from which question it is.

On the outflow side the model names a merchant and picks that
merchant's category. It is never allowed to decide that a card purchase
was actually an investment: the nature of the row is the data's, and
what the model adds is a finer reading of who was paid. A merchant
verdict is therefore finer than any kind floor, and migration 0066 put
the floor under the store for exactly that reason.

On the inflow side the model is asked **what kind of income** a receipt
is. That is a claim about the transaction itself, not about who sent
it — and wherever the data already makes that claim, the model has
nothing to add and can only be wrong. So the floor is read over the
store here, and the model has a say on the one kind that has no floor
at all: `deposit`.

The shape this protects against is one payer, two kinds. A signature is
shared by every row that folds to it, so an employer whose shares are
also held would carry one verdict onto both: the salary deposit's
`INCOME_WAGES` would re-type that employer's dividends. Under this
ordering the dividend keeps `INCOME_DIVIDENDS` with provenance `kind`
and the deposit keeps the store's verdict with provenance `model`.

**Every tier the pass writes still outranks the floor**, because they
all write into the overlay and the overlay is still the head of the
resolution: a pin, the matcher, a built-in rule, a config rule and the
provider map all sit above it, exactly as before. Only the MODEL moved,
from above the floor to below it. Decision 4 is untouched: a
`distribution` floors to `capital_return` and a tier that writes the
overlay — in practice a rule or a pin, since no provider map claims one
today — is what promotes it.

The **kind floor** is read at query time and never stored, with
provenance `kind`. One arm places a DELTA — `distribution` →
`capital_return` — which no spending floor does, and that row therefore
leaves the base with no verdict written anywhere. The pass still records
its signature.

---

## 4. The payer

Spending's merchant resolves in three steps. The payer resolves in four,
with one in front that the inflow side needs and the outflow side never
did. It is **first that ANSWERS**, so a step yielding nothing falls
through:

1. a line whose resolved type is a **delta** has no payer, terminal. An
   own-account move names the holder's own bank and a gift names a
   relative; neither is a payer.
2. the **instrument**, where the line carries one — the company, the
   issuer, the protocol, the fund — by name, or by symbol where the name
   is unknown. This answers for every row carrying an instrument.
3. the **payer store's** name for the line's signature.
4. the **signature** itself, trimmed.

The signature is the same fold spending computes, from the same
`counterparty` and `description`, by the same normaliser at the same
`SignatureVersion` — a counterparty is keyed one way whichever direction
the money moved, and a version bump re-keys both stores. The **store is
separate**: `income_payer_categories`, keyed by signature, holding an
income type. One counterparty can be both a merchant and a payer, and the
two questions have two answers.

---

## 5. Gross as booked, withholding beside it

**Income is what the source booked as arriving.** Where a source books a
gross dividend and a separate withholding row, income shows the gross and
the withholding row is already spending's `WITHHOLDING_TAX`. Where a bank
books a net salary, that is the income — the gross is on a payslip the
product does not hold, and a gross the product cannot see is not a figure
it should print.

This keeps the household arithmetic exact: *income − spending* subtracts
withholding once, on the side that booked it. A net-of-withholding income
would subtract it twice.

**`withheld`** is a memo column on `summary`, off by default: the
same-window `tax` rows on the same accounts, shown beside the income they
were withheld from. `net_income` does not read it. It is not on `types`,
and that is a correction rather than an omission — a tax row names the
security it was withheld from, not the income type, so there is no honest
way to attribute it to one row of that report.

It is **every NEGATIVE `tax`-kind row on those accounts**, negated so it
reads as a positive magnitude. That is what a brokerage books
withholding at source as — and also what UBS books a transaction stamp
duty as, so a purchase-heavy month can show a `withheld` figure with no
income beside it. The column is named for what it usually is rather than
for what it always is; it is a memo pointing at the spending side's
`WITHHOLDING_TAX`, never an input to any total. A positive `tax` row —
a refund of withholding — is not in it, which is the one shape the
memo's arithmetic could not have shown without changing sign.

A bucket can hold **withholding and no income**. The two sides are FULL
JOINed by bucket, so neither can drop a row the other has, and such a
bucket prints with `txn_count 0` and empty money columns — `-C
+withheld` is what shows why it is there. Dropping it instead would have
hidden tax paid in a month that received nothing, which is precisely a
month worth seeing.

---

## 6. What leaves the machine

**The fence is spending's** — SPENDING.md §5 is the whole of it, and
§6 the loop it gates. What may leave the machine does not depend on the
direction of the money, so `TransferShaped`, `RowTransferShaped`,
`PersonShaped`, `Uninformative` and `FilingOnly` are called the same way
for both families.

It matters more here, and one arm was **added because of this side**.
Rails, IBANs and masked contacts are markers BESIDE a name; an inbound
credit transfer often has none of them, because its narrative simply IS
the sender. `PersonShaped` is the arm that reads the name itself, and it
is on by default (`spending.categorization.fence_person_names`,
inherited with the rest of the block).

What each arm refuses is SPENDING.md §5's table; nothing here changes
it.

What the model sees on the income side is what is left of the
**deposits** — candidacy is that one kind and no other (§7) — so in
practice: employers, letting agents, agencies and the platforms whose
names say what they are. A business named like a person is fenced with
the people and is placed by a config rule or a pin, which is the remedy
every fenced row has; a gift or maintenance payment from a relative is
fenced and stays uncategorised, which is the honest answer.

The person arm's cost — it cannot tell a person from an organisation
whose name carries no marker, and refuses both — is SPENDING.md §5's to
state. It lands harder here, because this is the side whose narratives
are names.

The context levels are the same three. `income.categorization.context`
accepts `payer` as the income spelling of the narrowest level, and
`merchant` as well, so that a `spending.categorization` block inherited
whole validates without being rewritten. Internally they are one set of
constants: the level decides how much of a TRANSACTION leaves the
machine, and that question has one answer per level.

---

## 7. The model tier

**One `categorize`, two families.** The batching, the rolling anchors,
the gauntlet, the per-batch persistence, the retry flush and the fence
are one loop over a family descriptor; SPENDING.md §6 describes the loop.
What a family brings is a vocabulary, a pair of tables, and the nouns the
conversation uses.

```
wealthdb categorize [spending | income] [-n] [--batch N] [--all | --refine]
wealthdb categorizations [spending | income] [-f FORMAT] [-d VALUE] [--forget SIG]
```

- No positional runs **both**, spending then income: two plans, two
  summaries, one model. A name runs that one alone.
- `--forget SIG` with no family removes the signature from **both**
  stores and reports per store, one counterparty being able to sit in
  both.
- The dump's columns are named for **neither** family — `family`,
  `signature`, `name`, `detailed` — because one listing carries both
  stores and a `merchant_signature` header over a payer's row would be
  wrong on half of it. `family` is what says which vocabulary a row's
  `detailed` belongs to.
- The income conversation names payers of money RECEIVED, offers
  `ModelIncomeCategories()` and forbids `DeltaIncomeCategories()` by
  name. `--refine` re-asks `INCOME_OTHER_INCOME`, the family's catch-all.
- **Candidacy is `deposit` and nothing else**, in every backlog mode.
  It is the one admitted kind with no floor, so it is the one a model
  can say anything useful about (§3, decision 13). `--all` therefore
  means *every deposit signature, including the answered ones* — it
  lifts the backlog filter and never the kind gate, so it cannot reach
  a floor-placed row and buy a verdict for every instrument in the
  holdings.
- **The backlog is the RESOLVED type being NULL**, over
  `income_txn_categories()` joined to the population. With the kind gate
  this is the difference between a few hundred payer verdicts and
  several thousand pointless ones: a dividend the floor placed has a
  signature and no stored verdict, and asking about it would be work
  with a known answer.
- `income.categorization` **inherits** `spending.categorization` whole
  when absent — one household, one local model. Inheritance is
  whole-block rather than per-field: a half-inherited endpoint is a
  configuration nobody wrote down, and the failure would be a quiet
  widening of what leaves the machine.

---

## 8. Reading it: `wealthdb income <view>`

```
wealthdb income <view> [FROM [TO]] [--period P] [--level L]
                       [-f FORMAT] [-C COLS] [-x CCY] [-p]
```

| view | a row is | default columns |
|---|---|---|
| `summary` | a period bucket | `period, txn_count, income, reversals, net_income` (+ `withheld` via `-C`) |
| `types` | a (bucket, type) pair with its share | `period, type, txn_count, income, reversals, net_income, share` (+ `type_id`) |
| `transactions` | an income line, oldest first | `silver_source, date, account, kind, payer, income_type, provenance, currency, net_amount, value` |

The window defaults to the **trailing twelve months**, as spending's
does. Income is often read per calendar year, and a positional year is
the idiom the whole CLI already uses for it:
`wealthdb income types 2025 --period annual`.

`--level` defaults to **`detailed`**, the one default that differs from
spending's. The income vocabulary has one vendored primary, so at the
primary level every vendored type and extension folds into `INCOME`,
beside the deltas the base keeps (§3's table) and `(uncategorized)`.
That view has a use —
what was earned or yielded against what was given — but it is not the one
a reader opens the report for.

`income` and `reversals` are positive **magnitudes** and `net_income` is
the difference, mirroring spending's `spend` / `refunds` / `net_spend`
exactly; the `transactions` view keeps canonical signs.

**Privacy.** `payer` takes the free-text class, exactly as `merchant`
does and for the same reason: step 4 publishes the fold of a narrative
that can carry a person's name, and the fence gates the store, not the
column. That step 2 fills the column with an instrument name for most of
the base does not change the class — a column takes the class of the
worst thing it can hold. `payer_signature`, `counterparty` and
`description` redact with it; type, provenance and `share` stay legible.

`wealthdb transactions` carries `payer`, `income_primary` and
`income_detailed` behind `-C`; the defaults are unchanged. A transaction
can carry both families' verdicts, and routinely does — a deposit the
matcher paired is `internal_transfer` in each overlay — and that view is
the one surface showing a row from both sides at once.

**Metabase.** An **Income** dashboard and its privacy twin, in the
Spending dashboards' shape; `web/DESIGN.md` §6 has the detail.

---

## 9. Storage and lifecycle

Everything in SPENDING.md §8 holds, with the income names:

| table | grain | survives `reset`? | carried by `reload -a`? |
|---|---|---|---|
| `income_txn_enrichment` | per transaction, per source | no | n/a — re-derived |
| `income_payer_categories` | global, per signature | yes | **yes** |
| `income_account_scope` | config, stamped into gold | yes | re-stamped from config |

The payer store joins the merchant store in `reload -a`'s carry-across: a
store that is not carried is lost with no backup, so the rebuild walks a
LIST of stores rather than naming one.

`wealthdb status -v` prints an `income:` block with the backlog count.
The catch-all-kind counter (`excluded_unmapped`) is **not** duplicated
per family, but not because the two count the same rows: it joins
`spend_scoped_accounts()`, and where the two scopes agree — the default —
one number answers for both.

---

## 10. Decisions of record

1. **Three CLI views**, `summary / types / transactions`. No `payers`
   view: payers rank on the dashboard only, as merchants do.
2. **Plaid's `INCOME` vendored verbatim**, plus nine extensions and
   eight deltas. Widened deliberately for a general audience — the
   product is published, so the vocabulary names what a household
   commonly receives rather than what one deployment does.
3. **Gross as booked.** `withheld` is a memo, off by default, never read
   by `net_income` (§5).
4. **A private fund's `distribution` is excluded until proven income**:
   the floor places `capital_return`, and a rule or a pin promotes the
   exception. `capital_gain` stays income — it returns no basis.
5. **The instrument is the payer** for a row carrying one; the column
   keeps the free-text privacy class regardless.
6. **One `categorize`, two families**, with a separate payer store keyed
   by the shared signature.
7. **Own account scope**; `income.categorization` inherits
   `spending.categorization` when absent.
8. **One matcher.** Income pairs nothing and reads the shared verdicts;
   `spending.transfer_overrides` serves both.
9. The Wealth Overview's investment-income card reads the income base.
10. **Reimbursements are excluded** via the delta; netting them against
    spending is a recorded spending follow-up, not this work.
11. Window default: **trailing twelve months**.
12. **Reversals net inside their type.** `interest` enters positive only,
    the sign being the only thing that separates two opposite events;
    `deposit` takes both signs like every other income kind (§1).
13. **The model decides the KIND of income only where the data does
    not.** On the spending side the model only ever names the merchant
    and files that merchant — it may never decide that a card purchase
    was really an investment. On the income side it is being asked what
    kind of income a receipt is, which is a claim about the transaction,
    and the data makes that claim itself on every admitted kind but
    `deposit`. So the kind floor is read over the payer store here
    (migration 0073, §3) and candidacy is restricted to `deposit` in
    every backlog mode (§7). Two independent guards, as the fence and
    the context level are two.

---

## 11. Open follow-ups

- **The package name.** The deterministic pass lives in
  `internal/spending` and is now the enrichment engine for both
  families. Renaming it (`internal/enrichment`) touches every import for
  no behavioural gain and was left out of scope.
- **Reimbursement netting** on the spending side: an expense claim is
  money back for a specific outflow, and spending's refund side is where
  that would net. Excluded from income here; not designed. An insurance
  payout is NOT this case and is income — see §2: the premium was
  already counted as spending, and nothing links a payout to the
  premiums it answers.
- **`TODO(cashflow)`**: investment buys and sells, maturities and
  corporate actions, beside income and spending. The interest-versus-
  principal split stays where SPENDING.md §10 leaves it.
- **A rewards credit reaches gold as `refund` today.** Every card adapter
  maps a statement credit to `refund`, being unable to tell one from a
  merchant credit once the issuer's descriptor is gone, so the `reward`
  kind's move to income does not yet bite. What changes that is an
  adapter learning the difference, not a gold migration.
- **The matcher pairs a deposit with its own same-amount reversal** when
  the two fall inside its window, so both legs read `internal_transfer`
  and leave the base rather than netting inside the payer's type. The net
  is identical either way; the account of it is not. The remedy, if one
  is wanted, is the transfer-override ledger that already exists for
  every other matcher false positive.
- **Cash-flow ingestion for a balances-only source.** A source whose
  bronze holds statements read for balances alone contributes no
  transactions, so every funding wire it sent has no leg for the matcher
  to pair and the receiving leg stays in the income base as an unplaced
  receipt (§1). Parsing those statements into transactions fixes it at
  the root and needs no income change at all — it is a collector
  project, and the same gap a returns reconciliation of the same era
  waits on. `income.accounts.exclude` is the remedy until then.
- **A built-in income rule for platform custody deposits** — a `deposit`
  on a private-market custody account is the holder's own capital going
  in, and `capital_return` is what it should read as. It is the
  general-audience version of the exclusion above, and it needs
  something the rule hook does not carry today: the account KIND and the
  source, rather than a narrative.
- **No `report_income_summary_multi`.** Spending publishes a
  multi-currency sibling for all three of its reports; income has two.
  Nothing reads either summary `_multi` today, so the gap is recorded
  rather than filled — an unread macro is a shape to keep true for
  nothing. A Metabase card that needs the summary in three currencies
  is what would close it.
- ~~**Whether a transfer override can assert a cross-currency pair.**~~
  Answered: it can. The partition by native currency belongs to the
  matcher's AMOUNT pass, and both phases that assert a pair outright —
  the override ledger and a reference the source stamped on both legs —
  run ahead of it and never consult a currency. So the two legs of a
  wire converted in transit are reachable by a `match` line, and
  reachable without one wherever the bank stamped one transaction
  number on both. SPENDING.md §3 says so in both places.
