# Spending

How gold turns transactions on the tracked accounts into
categorised spend: the vocabulary, the tiers that assign from it, the
order they win in, and the two gates that decide what a third-party
model is ever shown.

This is the spend-category taxonomy. The instrument taxonomy —
`asset_class` × `vehicle`, which classifies *holdings* — is a
different vocabulary with a different purpose and lives in
[TAXONOMY.md](TAXONOMY.md).

Related design: DESIGN.md §4.11 (the `categorize` and
`categorizations` subcommands), §4.12 (the `spending` read command),
§5.1 (the `spending` config block), §13.11 (the pins ledger), and
migrations 0040 / 0041 / 0045 / 0046 / 0047 / 0048 / 0049 / 0050 /
0052 / 0054 (the `spend_categories` dimension, the enrichment overlay,
the `investment`, `card_spend` and `gift` deltas, the blank merchant on
a delta line, the serving views' zone-free timestamps, the model tier's
resolved provenance, the issuer a card bill names, and the signature a
line with no store row falls back to).

---

## 1. Scope: outflows only

Spending covers money leaving the tracked accounts. Income, interest
credited and dividends are a future **cashflow** feature and are
deliberately absent here — including from the taxonomy, which drops
Plaid's four flow primaries rather than carry values nothing may
assign. Investment FEES are not in that list: they are money leaving,
they arrived with the brokerage accounts (§2), and they have a value of
their own.

The population is layered in SQL (migration 0041), each layer narrower
than the one it reads, so no Go-side predicate restates any of it:

| macro | what it is |
|---|---|
| `spend_scoped_accounts()` | which accounts count at all: EVERY account by default since migration 0068 — whether an account can pay a fee is not a property of its kind — and a `spend_account_scope` row is the only thing that takes one out |
| `spend_enrichment_population(f, t)` | what the enrichment pass may write a verdict for |
| `spend_matcher_pool(f, t)` | what the internal-transfer matcher sees — deliberately BROADER on two axes: the transfer-eligible KINDS, which include the income side the spending base excludes, on EVERY account rather than the scoped ones (migration 0044). ONE pool for both families: income reads its verdicts and pairs nothing ([INCOME.md](INCOME.md) §1) |
| `spending_lines_base(f, t)` | what a report charts: the population with its category resolved and its own-account moves and capital deployed removed — a bill on a card not itemised (`card_spend`) and a cash gift (`gift`) stay in |

The layering is not decoration. The enrichment pass is the thing that
*decides* which rows are own-account moves, so it cannot read a
population that has already dropped them — that would be circular. And
the matcher pool is not a narrowing of either: a matcher can only pair
legs it can see, and the receiving half of a movement out of a cash
account lands wherever the money went, which is usually an account no
spending report charts. A pool restricted to the spending scope leaves
every such movement one-legged, and a one-legged outgoing leg is
indistinguishable from spending.

The reports over that base — `report_spending_{summary,categories,
transactions}` and their `_multi` siblings (migration 0042), and the
`web_spending` / `web_card_balances_history` serving views (0043) —
restate none of it. Sign-split magnitudes, period bucketing, shares,
and why the summary-equals-categories identity cannot catch a wrong
exclusion: DESIGN.md §10.10.

`wealthdb spending <view>` is the CLI over those three macros, one
view each, and restates none of it either — see §7.

`spend_account_scope` is where an account leaves the scope, and since
0068 it is the only place that happens. A donor-advised fund is one
example: its grants would double-count giving already booked when the
fund was contributed to. Such a rule argues about a particular
arrangement rather than about a kind. It is configuration
stamped into gold, on the
`SetFxPriorities` precedent: the report macros need no runtime config
injection, and removing an entry from `spending.accounts` removes its
effect on the next pass rather than leaving it behind. The key is an
account id — `spend_scoped_accounts()` joins the stamped row to
`accounts` on exactly that — so an entry naming a nickname or a display
name scopes nothing; the pass counts those and the load summary reports
how many there were.

---

## 2. The taxonomy

### Provenance

The vocabulary is **Plaid's Personal Finance Category taxonomy**,
vendored verbatim into `internal/canonical/spendtaxonomy.go`
(retrieved 2026-09-04: 16 primaries, 104 detailed pairs). Values and
descriptions are copied exactly — only trailing whitespace trimmed —
so a refreshed CSV diffs cleanly against the table.

It is vendored rather than fetched, so a taxonomy revision arrives as
a reviewable diff instead of silently re-labelling history. Gold's
`spend_categories` dimension is seeded from this table — migration
0040 seeded the vendored rows and the first three deltas,
0045 / 0046 / 0047 one delta each, 0056 and 0065 the EXTENSIONS, 0069
the whole income side and the `family` column — and a
generator-style test pins the migrated dimension to the table so they
cannot drift. A new value
is a row here plus a new migration; an applied migration is never
edited.

The spend side is **12 primaries and 80 detailed values.** Three of
Plaid's sixteen primaries are dropped — `TRANSFER_IN`, `TRANSFER_OUT`
and `LOAN_PAYMENTS`. The fourth, `INCOME`, is vendored whole for the
other family (migration 0069): one table, one dimension, and a `family`
column telling the two vocabularies apart. See
[INCOME.md](INCOME.md) §2.

`LOAN_PAYMENTS` is the load-bearing drop. A mortgage payment leaving a
cash account is classified `internal_transfer` by a built-in rule,
because the mortgage is itself a tracked account
(`AccountKindMortgage`): the payment moves value between two accounts
the product already holds, which makes it an own-account move by the
product's own definition rather than spend.

### The six spending deltas

Six values the SPENDING side reads are the product's own rather than
Plaid's — eleven rows across both families, of which these six are
spending's and three are shared. They are primary-level (primary ==
detailed, so each groups as its own bucket) and keep the repo's
lowercase enum idiom, which also marks them at a glance as
not-from-Plaid. The three marked *both* mean the same thing read from
either direction and are ONE row in the dimension:

| value | meaning |
|---|---|
| `internal_transfer` | *(both)* movement between two accounts the product already tracks, either leg — card payments, funding wires, mortgage payments, a pension contribution arriving |
| `cash_withdrawal` | cash taken out at an ATM or a counter; what it was then spent on is unobservable |
| `card_spend` | a credit-card bill paid to an issuer whose card is not itemised in wealthdb — generic card spend, in the base until the card is collected |
| `gift` | *(both)* a cash gift or family support, given or received — with no merchant, employer or issuer behind it; not a gift item bought in a shop, and not a donation to a non-profit |
| `investment` | capital deployed from a cash account — a securities subscription, a deposit into a wallet — to a destination the product does not track; not consumed, and not an own-account move |
| `other` | *(both)* money moved that no rule, matcher or model could place — a payment on the outflow side, a receipt on the inflow side |

`cash_withdrawal` is its own primary rather than a guess, so a report
can show how much of a period's spending is simply unattributable
instead of hiding it inside a plausible-looking category. The built-in
atm rule places it from a narrative that names the machine; the
provider tier places it from a booking type that does (§3), which is
how the rows whose narrative is nothing but a bank tag are reached.

`investment` encodes one policy: **capital deployed is not spending.**
It is an `internal_transfer` when the destination is an account the
product tracks — the matcher sees both legs, or a rule knows the
account — and an `investment` when it is not. The vendored vocabulary
is consumption through and through, so without a value of its own an
outright securities purchase from a cash account, which the bank books
as a plain withdrawal, had no honest home: `internal_transfer` would be
false, `other` is excluded from nothing and explains nothing, and every
vendored category says the money was consumed. It is excluded from
`spending_lines_base` exactly as `internal_transfer` is (migration
0045), and it is a delta, so the model may never emit it; the rule
tier and the pins are what place it.

`card_spend` encodes the opposite policy, and is the one delta that
*is* spending: **an unpaired card bill is spend on an unitemised card,
never an own-account move.** A household pays its card bills from its
cash accounts. When wealthdb collects the card, the bill pairs with
the card's own `card_payment` leg through the matcher and nets out as
`internal_transfer` — correctly, because the purchases are itemised on
the card. When it does not — and no user collects every card — the
bill is the only trace of that spending, and filing it as
`internal_transfer` deletes real consumption from every total,
silently, at whatever size the bill was. So the built-in card-payment
rule (§3) places `card_spend`: a placeholder primary, **included** in
`spending_lines_base` (migration 0046 — the base's exclusion list
names `internal_transfer` and `investment` one by one, not "every
delta"), shown as its own line in every category report so it is
visible rather than hidden, and replaced by itemised purchases the day
the card is collected, because the matcher outranks the rule. It is a
delta, so the model may never emit it; the built-in rule, a config
rule, a pin, or the provider tier where the bank's own booking type
names a bill paid to a card (§3), is what places it.

`gift` is the other delta that *is* spending: **a cash gift is
spending, and it is visible as what it is.** A household gives money
away — a cash gift, an allowance, support for a relative — and the
bank books it as a plain transfer to a person. The vendored vocabulary
has no honest home for it: `GENERAL_MERCHANDISE_GIFTS_AND_NOVELTIES`
means buying gift items in a shop, `GOVERNMENT_AND_NON_PROFIT_DONATIONS`
means a non-profit, and left to itself the model files such a transfer
under `GENERAL_SERVICES_OTHER_GENERAL_SERVICES`, which is false —
nothing was bought and no service rendered. `internal_transfer` would
be false too, since the money left the household. So `gift` is a
primary in its own right, **included** in `spending_lines_base` by the
same construction as `card_spend` (migration 0047 — the base's
exclusion list is unchanged) and shown as its own line in every
category report. It is a delta, so the model may never emit it; and
no built-in rule places it either, because nothing in a transfer's
narrative says a person is family rather than a contractor or an
untracked account of the holder's own. A config rule (§3) or a pin
(§7), carrying the holder's own knowledge, is what places it.

### Extensions

A third class, and the newest. An EXTENSION is a value of ours in the
vendored SHAPE — a `PRIMARY_DETAIL` pair filed under an existing
primary — added where the vendored vocabulary has no word for
something a household buys often. There are three.

`GENERAL_SERVICES_DIGITAL_SERVICES` (migration 0056): software and
online subscriptions, for which the taxonomy offers ELECTRONICS
(physical goods), ONLINE MARKETPLACES (retail) and INTERNET AND CABLE
(the connection), none of which is a password manager or a model
subscription.

`BANK_FEES_INVESTMENT_FEES` and
`GOVERNMENT_AND_NON_PROFIT_WITHHOLDING_TAX` (migration 0065) came with
brokerage accounts, which joined the scope in 0064 and were followed by
every other kind in 0068 (§1). Holding
investments costs money in two ways the vendored vocabulary cannot
name, and both arrive in volume — together they are most of what a
managed account books.

Every vendored BANK_FEES value is a fee for BANKING: ATM fees,
foreign-transaction fees, insufficient funds, interest charges,
overdrafts. A custodian's ADR depositary charge, a pension platform's
quarterly fee and an investment manager's bill are fees for INVESTING,
and filing them under `OTHER_BANK_FEES` buries the cost of being
invested inside the cost of having an account. They sit under
BANK_FEES all the same — that is the primary for "what a financial
institution charged".

Withholding belongs beside TAX_PAYMENT and is not the same thing: a
tax payment is assessed and then paid, while withholding is deducted
before the money is ever received. A report that cannot tell them
apart cannot answer "what was paid in tax that was never seen", which
where a portfolio holds foreign dividend payers can be the larger
share.

It differs from a delta in the one way that matters: **the model MAY
emit an extension.** A delta is decided from structure a merchant name
cannot reveal — whose account the money reached, whether a card is
itemised — so the gauntlet refuses one. An extension is an ordinary
merchant judgement of exactly the kind a merchant name answers.

It goes under an existing primary rather than becoming one, which is
the opposite of the choice `gift` made. A delta earns its own primary
BECAUSE it is not a merchant category and must not fold into a
plausible-looking one; an extension is a merchant category, so it
belongs beside its siblings, rolls up with them, and — when the
vendored taxonomy eventually adds the value — is superseded by the
refreshed CSV as a clean diff instead of sitting beside it.

### Two validity predicates, and why

`canonical.ValidSpendDetailed` admits everything storable — the 80
vendored values, our extensions, *and* the six deltas.
`canonical.ModelSpendDetailed` is the stricter sibling: it admits
what a model may emit — vendored and extension — and refuses only the
deltas.

The model tier validates against the stricter one. A model verdict is
keyed by MERCHANT and therefore applies to every transaction that
merchant ever produced, forever, across every account. An
`internal_transfer` in that position would silently and globally
remove a merchant from spending — the one verdict whose effect is
invisible in a report, because the rows simply stop appearing. The
other five are milder but no more legitimate: all six are decided
from structure a model cannot see (a matcher that watched both legs of
a movement; a rule that knows which accounts the product itself
tracks and which card legs it holds; a provider whose own booking type
names the movement; a pin the holder wrote) or from the absence of a
decision. `card_spend` and `gift` are the two a model
might be tempted towards, being in the base — but the first is decided
by the *absence* of a counter-leg, which is exactly what a
merchant-keyed verdict cannot know, and the second by who the
counterparty is to the holder, which the narrative does not say and
the fence (§5) keeps from the model in any case.

The predicate is derived from the table rather than restated as a list,
so a delta added to `canonical` is refused by the gauntlet without the
command changing. `spending.rules` and `spending.pins` validate against
the wider predicate, `ValidSpendDetailed`: both are the holder's own
local input, applied inside the pass and never shown to the model, so
the model's restriction has no reason to reach them.

---

## 3. The tiers and the precedence lattice

Six tiers assign a category. Four are deterministic and free and run
inside `wealthdb load`; the fifth costs money and runs only when
`wealthdb categorize` is invoked; the sixth is a floor under both.

```
             transaction scope                       merchant scope
   ┌──────────────────────────────────────┐   ┌────────────────────────┐
   │ pin > matcher > rule > provider      │ > │ model                  │ > kind
   └──────────────────────────────────────┘   └────────────────────────┘
             spend_txn_enrichment                spend_merchant_categories
        (per source; derived, pins re-stamped)     (GLOBAL, paid for)
```

The **kind** floor (migration 0066, extended by 0067) is last, stands
outside both stores — it reads no verdict anyone wrote — and takes the
transaction's own kind: a row of kind `fee` that nothing else placed is
an investment fee, one of kind `tax` is withholding, and one of kind
`interest` is an interest charge when the amount is negative. The sign
is tested rather than assumed there, because this macro is read at
transaction grain too, where credited interest reaches it and is
income. It exists because a brokerage
books rows no narrative explains — a security-level fee or tax withheld
at source, whose narrative is the SECURITY or, on some sources, nothing
at all. There is no payee in them for a rule to key on. But the kind is
not a guess: each adapter derives it from whatever evidence its own
source gives, which is where source-specific knowledge belongs, so the
floor reads that verdict rather than re-deriving it from prose.

It sits UNDER the model, and that ordering is the whole design. The
model files a "Foreign Transaction Fee" as the vendored
FOREIGN_TRANSACTION_FEES, finer than any floor; a floor written into
`spend_txn_enrichment` would beat the merchant store and quietly
coarsen every such row. Resolving it in the macro instead means it
applies only where the pass AND the model both declined — so `kind`,
like `model`, is a provenance that exists only at query time and is
never stored.

**Across scopes**, `spend_txn_categories()` (migration 0042, re-issued
by 0048, 0050, 0052 and 0054 for the merchant column and the model
tier's resolved provenance) resolves with `COALESCE(transaction scope,
merchant scope)`: a per-transaction verdict beats the merchant-wide
one. Both `spending_lines_base` and the transaction reports read that
macro, so the lattice has exactly one definition. The merchant store is
reached *through* the enrichment row's signature, which is why the pass
records a signature even for rows it cannot categorise. The same macro
publishes the merchant column, in three steps (§7): a delta line shows
its issuer label or nothing at all, whatever the store holds for its
signature; otherwise the store's name; otherwise the line's own
signature, which is what every line this lattice places above the model
tier — and every line it places nowhere — has instead of a store row.

**Within the transaction scope** the order is pin > matcher > rule >
provider, and it reads weakest-first:

- The **provider** is the weakest signal. It is a third party's
  opinion about a row, published without any knowledge of which other
  accounts the product tracks. Weakest in precedence, not in
  vocabulary: **the provider tier may place a delta** when the
  provider's own word names the movement rather than a merchant's line
  of business — a bank's `ATM WITHDRAWAL` is `cash_withdrawal`, an FX
  conversion between the holder's own currency accounts is
  `internal_transfer`, a `PAYMENT TO CARD` is `card_spend`. The
  no-deltas restriction (§2) is the model tier's alone: a provider
  verdict is per transaction, placed from the provider's structured
  filing of that row rather than from a merchant's name, and every
  tier above still overrules it.

  **A catch-all is not a verdict.** Where the issuer's value translates
  only to a primary's own `OTHER_*` bucket, the tier records what the
  issuer said and DECLINES the row. A catch-all carries no more than
  the primary already did, and claiming with one would pre-empt the
  model — the only tier that reads the merchant name, and the one that
  can do better: the issuer knew the row was "shopping", and the
  descriptor said Apple. Declining is not the same as the issuer
  saying nothing; both leave the row for a later tier, and
  `provider_spend_detailed` tells them apart (§ The issuer's view,
  kept).
- A **rule** beats it because a rule encodes something structural
  about the product's own account graph — that the mortgage being paid
  is itself tracked, that cash out of an ATM is unattributable — which
  no issuer can know.
- The **matcher** beats those because it is not an inference at all:
  it has SEEN both legs of the movement. A provider that labels a card
  payment "Shopping" is simply wrong, and evidence outranks opinion.
  This is also what makes `card_spend` safe: the same card-bill
  narrative is `internal_transfer` via `matcher` when the card's own
  leg is in gold and `card_spend` via `rule` when it is not, and
  collecting the card flips the verdict on the next pass.
- A **pin** beats everything. It is the holder's own word about one
  transaction, written for exactly the row where every tier below has
  nothing to go on — or got it wrong.

`manual` is the provenance a pin lands with. The schema admitted it
from the first issue of the overlay, and the pass owns it exactly as it
owns the derived provenances (see *Pins* below).

### Pins

`spending.pins` names a CSV ledger of per-transaction corrections
(DESIGN.md §13.11 has the columns and the mechanics). It exists for the
row nothing else can classify: an FX roll settling as a bare
withdrawal has no descriptor for a rule and no counter-leg for the
matcher; a subscription booked as a plain debit looks exactly like a
large purchase. The only thing that identifies such a row is *which*
row it is, so a pin keys on the transaction's observable identity —
source, account, day, amount within a cent, currency — and applies to
**every** row matching it. Two identical rows on one day are
indistinguishable by design; that is a property of the data, not a
limitation the ledger could remove.

A pin may set **any** valid value, vendored or delta — the same
vocabulary a config rule may place. Pins are the override surface: a
rule fires on every narrative its pattern matches, a pin names one
transaction and outranks everything, including a matcher pairing the
holder knows to be wrong.

The ledger is config, and the pass treats it as it treats
`spending.accounts`: re-stamped whole on every pass, after every other
tier. So a pin removed from the ledger is gone on the next pass, a
fresh-file `reload -a` needs no carry-across, and a pin whose row has
not loaded yet is *counted* as unmatched — on the pass result and on
the `spending:` summary line — rather than treated as an error.

### The full re-assert

The deterministic pass runs after every load, over the whole history,
and re-derives every verdict it is capable of reaching. Three
properties a cheaper incremental pass would have to give up:

- **Late arrivals.** A provider's category can appear days after the
  row; a counter-leg loaded from a different source turns yesterday's
  apparent spending into today's own-account move. Only re-deriving
  both sides catches either.
- **Idempotence.** A full re-assert has no accumulated state to be
  wrong about, so a re-run after a crash or a partial load produces
  exactly what an unbroken run would have.
- **Evolution.** When a rule or the normalisation changes, the whole
  history moves at once instead of leaving a stratum of rows enriched
  by whichever build first saw them.

The pass runs after the per-source loop, not per source: the
withdrawal that funds a card payment and the card payment itself
routinely arrive from two different sources, and neither leg is
recognisable as internal until both are in gold.

### The matcher

`internal/spending/matcher.go` calls gold's shared transfer-matching
core with spending's own settings: same-source pairing is **allowed**
(the commonest own-account move is cash account to card inside one
bank), same-**account** pairing is allowed too (`AllowSameOwner`),
**every** account in gold participates whatever its kind and
whether or not spending is scoped to it, and amounts and currencies
are **native**.

Same-account pairing is the one knob the returns caller leaves off. A
withdrawal and a deposit of the same amount on the same account within
the window is a round trip that nets to zero — a transfer bounced
back, a reversal booked as its own line — and left unpaired its
outgoing half counts as spending; the returns engine reads the same
two rows as two boundary flows, not one movement, and pairing them
would net capital that really did leave and return. Two things bound
the new false-pair surface. The kind set below excludes `purchase` and
`refund`, so a card purchase and its refund never reach the matcher
and cannot pair through it. And when a withdrawal finds equally good
partners on its own account and on another — a payroll credit landing
the day a transfer of the same size leaves — the far side wins the tie,
so the own-account coincidence cannot steal the partner that is really
on the other end.

Two further bounds close what those leave open, because amount and day
proximity alone are only ever a coincidence of size, never evidence
about what either row IS.

**A leg may demand a rail of its partner.** A card's record of being
paid — "payment thank you", "payment received thank you" — is
unmistakable: nothing but a payment TO that card produces the line. So
it demands that its partner be a card payment, and any other debit of
about the right size is refused. The demand is one-directional: the
bank-side half of the same movement often carries only the issuer's
name, the holder's own, or nothing, so a card payment demands nothing
of its partner and a leg that names no rail constrains nothing at all.
Without this the receipt pairs with whatever is nearest in amount — a
utility bill, a cheque, a payment to a person — and because a pair
removes BOTH legs, that debit is real spending that silently
disappears, in the one direction a chart cannot show.

**The tolerance is capped in absolute terms.** The percentage exists to
absorb a fee deducted in transit, and such a fee is FLAT: a fixed
charge per wire, not a share of the sum. Uncapped, half a percent of a
large transfer is tens of units — more than any real fee, and wide
enough to reach an unrelated credit. `DefaultTransferFeeCap` bounds it
so a genuine wire fee still pairs and a coincidence at that distance
does not. The returns caller applies the same cap; the rail demands it
never sets, so its matching is unchanged.

### Manual overrides on the matcher

`spending.transfer_overrides` names a CSV ledger of decisions the data
cannot settle, in the same idiom and for the same reason as the pins
ledger: a leg is named by what a person reads off a statement —
source, account, day, amount, currency — never by gold's opaque
transaction id.

| verb | legs given | meaning |
|---|---|---|
| `unmatch` | both | those two may not pair with **each other**; each stays free to find its real partner |
| `unmatch` | first only | that leg is not half of a movement at all, whatever sits near it |
| `match` | both | assert the pair, ahead of the day window, the tolerance and the rail rules |

`match` exists for the movement whose halves no window can reach: a
bank posting its side of an ACH days after the other side credited.
Forced pairs are asserted before the greedy pass and withdrawn from
it, so a stated pair cannot lose either half to a nearer coincidence.

A rule that names no leg is **reported, not dropped** — the pass counts
it exactly as it counts an unmatched pin. An override is something a
person wrote down about a row they believe exists, so one that quietly
does nothing is the worst outcome available.

The ledger is read once and handed to BOTH matchers. The core is
shared, so a pair the holder has settled must be settled the same way
in spending and in returns; a movement called internal in one report
and external in the other is worse than either answer alone.

The kind set — `deposit`, `withdrawal`, `card_payment`, `transfer_in`,
`transfer_out` — is the one narrowing that stays. Those are the kinds
that mean "cash moved into or out of an account" *and* carry a pinned
canonical sign, so every leg can be oriented as debit or credit. The
catch-alls (`journal`, `other`) and the unsigned kinds (the `fx`
family, `corporate_action`) carry whatever sign the source supplied;
`contribution` and `distribution` are both booked from the funding
account's own perspective, so neither can ever be the receiving half
of a movement out of it. Widening the set buys false pairs, and a
false pair does not read as a wrong category — it deletes a real
spending line.

**Known limitation:** a cross-currency own-transfer cannot match. The
core partitions candidates by native currency, because matching
converted amounts would make the same movement pair differently per
output currency — a report's display currency must not change what
counts as spending. A withdrawal in one currency funding a card in
another stays one-legged and is left to the rule tier. `wealthdb
categorize` surfaces these as *cross-currency near-pairs* rather than
pretending to fix them.

That limitation has a sharp edge once a card IS collected. A card billed
in one currency and settled from an account in another leaves both legs
one-legged, so its purchases are itemised *and* its bill stays in the
base as `card_spend` — the same spending, counted twice. The card rule is
not narrowed to prevent it: the alternative, suppressing `card_spend`
wherever the source holds any card account, would delete a genuine bill
for a card that is not collected, and an over-count is visible in a
report where an under-count is not. The near-pair canary is what makes
the shape findable; a pin or a config rule is what fixes a deployment's
own.

### The rule tier

The built-in rules are evaluated in order, first match wins. They are
**engine constants, not user configuration**: each exists because a
whole class of rows is structurally mis-read without it, and the right
answer follows from what the product already knows about its own
accounts rather than from anyone's preference.

| rule | verdict | why |
|---|---|---|
| `card_payment` | `card_spend` | A card bill with no counter-leg in gold is a bill for a card wealthdb does not itemise — a card no collector exists for, or the deep era, where a card payment is dated before the card's own ledger begins — and the bill is the only trace of that spending. So it is kept in the base as generic card spend, not deleted as an own-account move; a bill whose card *is* in gold never reaches this verdict, because the matcher outranks it. Matched on card-payment phrases and an issuer table, never on a store card that names its merchant, and refused outright on a row the `atm` rule matches — cash taken at a machine carries the same masked card number. A match on a named issuer's descriptor also LABELS the bill with that issuer, which is what the line carries as its merchant (§7). |
| `atm` | `cash_withdrawal` | The money is gone, but *what it bought* has no record anywhere. |
| `mortgage` | `internal_transfer` | The mortgage is a tracked account; counting the payment as spend would double-count against the liability it reduces. |
| `investment_fee` | `investment_fees` | A custodian's per-security pass-through, such as an ADR depositary charge, booked once per security per period. The narrative names the security and never a payee, so nothing else can reach it. It is a cost of INVESTING rather than of banking, which is what the extension exists to say. |
| `wire_fee` | `other_bank_fees` | Sits on the same statements as the pass-through above and is deliberately not one: paying to move money is a banking service, and filing it as an investment fee would overstate what holding the assets costs. Not the wire itself, which no rule places — that is the matcher's to pair or nobody's to guess. |
| `withholding` | `withholding_tax` | Tax deducted at source on foreign dividend income. The gross dividend is booked as income, so leaving the withholding out would report the gross as though it were net. |
| `management_fee` | `investment_fees` | The fee an account pays for being managed. It is levied on the account where the pass-through is levied on the security, but both are the cost of holding the assets, which is why they share one value. |

Rules run against three texts, each on its own: the merchant
**signature**, and the raw `counterparty` and `description` it was
built from. All three go through the same fold, so a rule sees
upper-case, punctuation-free tokens and does not restate a narrative's
spelling variants; each field is tested whole, so a phrase never
straddles the join between two of them, and rule order outranks field
order. The raw fields are read because the signature is not always
where the creditor is: the UBS adapter's counterparty is silver's
promoted first narrative segment, which on a PDF-era transfer is the
bank's own name while `C/O UBS CARD CENTER` sits in the description,
and a description can run past the signature's cap.

One rule declines rather than fires. Cash taken at a machine is booked
against the card that opened the drawer, so its counterparty is the
masked card number the card-payment rule reads as a bill — and the
card rule leads the table, which would file cash out of an ATM as
spend on a card, inflating the `card_spend` placeholder and emptying
the `cash_withdrawal` line. It stays wrong even once the card is
collected, because a withdrawal is not on the card statement either.
So the card rule refuses any row the `atm` rule's own patterns match,
and the row goes to whichever tier can place it: the `atm` rule, where
the narrative names the machine, or the provider tier, where only the
bank's booking type does. That refusal is the one thing a built-in
reads the **provider's filing** for. It may never place a verdict from
it — that would be the provider tier wearing this tier's provenance
and outranking it — but declining a row on it is the opposite move: it
hands the row down to the tier that owns the filing.

The description is read up to its **memo separator** only (§4). What
follows it is the payer's own words, not the bank's narrative, and a
built-in that matched on them would place a delta from the payer's
vocabulary — `Hypothekarzins` typed on a wire to a lender the product
does not track, `Bancomat` on a payment to a shop — with this tier's
provenance and, for `internal_transfer`, by deleting the row from the
base: the failure that is invisible in a report. **Built-in rules read
the narrative only; config rules read narrative and memo** (below).

The card-payment rule's issuer table is a list of ISSUERS, each with
the phrases a deposit-account export can put on a bill paid to it, one
per statement format, digits masked — `PAYMENT TO CHASE CARD ENDING IN
####`, `CHASE CREDIT CRD AUTOPAY`, `AMERICAN EXPRESS ACH PMT`,
`AMERICAN EXPRESS CREDIT CARD`, `CITI CARD ONLINE PAYMENT ####`, `CITI
AUTOPAY PAYMENT ####`, `CITI CREDIT CARD PAYMENT`, `UBS SWITZERLAND
AG;C/O UBS CARD CENTER`, the Swiss direct-debit shape `…; UBS CARD
CENTER; CREDIT CARD STATEMENT …` / `…; CARD PAYMENT …`, and a masked
card number as the whole counterparty, which is a card top-up, in
either of two masking conventions: the spaced `XXXX XXXX XXXX ####`,
and the one-token `####XXXXXXXX####` — first four and last
four digits kept, the middle eight masked, often followed by a date
fragment. The two masked forms are the entries that name no issuer:
they say a row is a card bill and nothing about whose card it is, so
they place the verdict and label nothing.
Each descriptor is a whole-word phrase matched *anywhere* in
the signature or in either narrative field, so the mandate notice a
direct debit puts before the creditor (`DIRECT DEBIT; <CODE> OBJECTION TO <BANK>; WITHIN 30 DAYS;`,
§4) makes no difference whether or not the normaliser stripped it. The
one-token mask is the exception: its digits differ per card, so it is
matched as a token *shape* rather than a phrase, and narrowly — exactly
four digits, eight `X`, four digits, so a plain 16-digit number is not
it.
The table holds **issuers only**: a store card that names its merchant
— a fuel card, a retailer's own card — is that merchant's spend and
belongs in that merchant's category, which the model or a config rule
places; adding a retailer to the table would re-file its purchases as
generic card spend, which is precisely the information the
placeholder exists to stop losing — and would put a retailer's name in
the merchant column of a line that is not its purchases.
The name in each entry is what a bill matched by it is **labelled**
with, and the label is the merchant column of the `card_spend` line
(§7). It is the rule's own output, carried out of the tier beside the
category and stored on the enrichment row: no other tier writes one,
and a tier that overrules the card rule clears it with the verdict it
replaces, so the label never outlives the bill it describes.
Phrases in every built-in rule
match whole tokens, never substrings, so `SCORECARD PAYMENTS` does not
carry the phrase `CARD PAYMENT`.

#### Config-supplied rules

One input to the rule tier is not an engine constant: `spending.rules`,
a list of `{ "match": <regex>, "category": <spend_detailed>, "scope": {…} }`
entries, of which `scope` is optional. `match`
is compiled case-insensitively and tested against a row's raw
narrative (`counterparty` and `description`, each on its own — the
description whole, memo included, so a rule may key on what the payer
wrote a payment was for) **and against `provider_category`, the
issuer's own filing of the row**, on the same terms. That third field
is how a rule reaches a class of merchant the descriptor never names —
a membership, a trade — and it is what makes the issuer an input to
our classification rather than a tier that outranks it. `category` is
what a match places, with provenance `rule`. The key is
named for what the entries are — rules in the same tier as the three
built-ins, with the same provenance — rather than for a pattern with
one fixed verdict.

**Scoping a rule.** `scope` narrows where and when a rule may fire:
`source` (a silver_source_id), `portfolio`, `account` and the inclusive
date range `from` / `to` (`YYYY-MM-DD`, compared against the row's own
day). Every field is optional and an omitted one does not constrain, so
a rule with no scope — every rule written before scopes existed —
matches everywhere.

It exists because a pattern specific enough for one booking is rarely
specific enough for the whole future. `^\s*closing\s*$` reads a
mortgage settlement exactly right, and is a liability the day another
bank writes "Closing" on something else; scoped to the account and the
month it stays surgical, and a later row that merely reads the
same falls through to be asked about rather than inheriting last
year's answer. Prefer the narrowest scope that still admits what the
rule is FOR: a self-describing phrase wants the account but not a date
(another extraordinary amortization on the same mortgage would be the
same thing again), while a generic word wants both.

An inverted range is a config error rather than a rule that silently
never fires — a scope that can admit nothing is a typo every time.

It exists for three populations neither the matcher nor the model can
ever reach. One is own-money movement whose receiving side is booked
nowhere in gold: a wire to the holder's own account at a bank the
product does not track, money sent to a family member's account
elsewhere, a transfer to a crypto exchange the holder also tracks. The
receiving side never arrives, so the outgoing leg is one-legged
forever — and one-legged outgoing legs are indistinguishable from
spending. The second is capital deployed to a destination the product
does not track — a subscription the bank books as a plain withdrawal —
which is `investment` by the policy in §2. The third is **consumption
paid by wire**: a lawyer, a contractor, a tax office. For a European
household most genuine spending off the card moves this way, and where
the narrative is person- or IBAN-shaped the fence (§5) keeps it from
the model — correctly, and by design; a creditor named with nothing
but a postal address is the model's to place. In all three the only thing
that identifies the row is text in the narrative, and that text is
personal: a name, an IBAN, a fund's or an exchange's legal entity, a
payee's.

Which is why the knob is **deployment-specific by nature**, and the
documentation is deliberately plain about it: this is where the
account holder's own name, own account numbers at untracked
institutions, own exchange counterparties, the funds subscribed to and
the recurring wire payees go; it lives in the user's config, never in
the repository.

The category may be **any** valid `spend_detailed` value, vendored or
delta (`canonical.ValidSpendDetailed`), in the taxonomy's own
case-sensitive spelling. A consumption category is allowed on
purpose: a rule is the holder's own local input, which the model never
sees, so the no-deltas restriction — which guards what the *model*
may say (§2) — does not apply to it. A recurring fenced counterparty,
a lawyer or a contractor or a tax office paid by wire, is kept from
the model by design and would otherwise be classifiable by nothing but
a per-transaction pin, one row at a time; a rule is the instrument for
a counterparty that recurs. `ModelSpendDetailed` still gates what
the model may say. A merchant the model can see and has mis-placed
still belongs in the merchant store, where the verdict is per-merchant
and visible; a single row belongs in the pins.

A config rule is consulted after the built-ins, so an ATM
withdrawal that happens to carry the holder's name is still
`cash_withdrawal`, and the matcher outranks it as it outranks every
rule: a withdrawal whose counter-leg *is* in gold is
`internal_transfer` via `matcher`, rule or no rule. Among the config
rules the first written wins.

Rules are compiled at config load, with `(?i)` prepended. An invalid
pattern fails the load with an error naming the entry's index and
text; so does one that matches the empty string (`""`, `.*`, `^`),
which would fire on every row and re-label the whole population; so
does a category string the taxonomy does not know, a known value in
the wrong case included. An empty or absent list is the default and
changes nothing.

Config rules read the raw narrative as the built-ins do, each field on
its own — an IBAN or a legal entity may fall past the signature's
64-character cap, and a pattern anchored to one field must not
straddle the join — but never the signature: a regex is the holder's
own spelling, written against what the bank printed. Unlike the
built-ins they read the description's memo as well: what the payer
typed on an order is the holder's own input about the holder's own
rows, and a deployment rule is the right place to act on it.

### The provider tier

Providers already file the rows they publish, and the filing is carried
into gold verbatim as `transactions.provider_category`: a card issuer's
spend category, a bank's booking type. The translation maps are per
**product** rather than per source, because the vocabulary belongs to
the provider's product — a source's key is its silver kind, optionally
narrowed to one account kind (`ubs/card` before `ubs`), and a kind with
no entry of its own inherits the source's. Lookups fold case and
surrounding space so one entry covers every era's spelling of a value.

One source needs that narrowing today. UBS files a bank account's rows
by booking type and a card's by merchant category, and the two shapes
disagree about what an unmapped value *means* — so they are two
vocabularies, one categorical and one not, rather than one map that
would have to call both the same thing.

Two shapes of vocabulary, and the shape decides what a value the map
does not translate *means*:

- A **categorical** vocabulary — a card issuer's (`chase`, `amex`) — files
  its rows under a spend category rather than naming a payment rail.
  **The unmapped case is the
  load-bearing one.** The map is the vocabulary this build translates,
  which is narrower than what an issuer can publish, so a value it does
  not hold is one nobody reviewed: it falls through to the model tier
  and is *counted* as drift, never guessed into a plausible neighbour —
  a wrong category is invisible in a report, while an uncategorised row
  is visible as backlog and an unmapped-value count is visible as
  drift. `wealthdb categorize` prints those counts, and `load` prints
  the pass's. The exceptions are the values the vocabulary lists as
  `untranslatable` beside the translations: the issuer's own residual
  bucket — its literal "Other" — and any value that names a money
  MOVEMENT rather than a line of business, such as a bank's catch-all
  for the card rows that transferred money instead of buying something.
  Both are reviewed, so a miss on either is not drift, and both are
  deliberately left untranslated, because a row the provider itself
  could not place, or placed under a movement, is the row the model tier
  exists for. Translating one would lose the row to a bucket; counting
  it would inflate the drift signal with a value there is nothing to
  review.
- A **booking-type** vocabulary — a bank's (`ubs`) — names how each
  entry was booked, in whichever spelling the era used: the statement
  PDF's `E-BANKING PAYMENT ORDER`, the MT940 feed's `NTRF`, the web
  export's `e-banking payment order`. Most values name a payment rail,
  and a rail says nothing about what the money bought, so only the
  types whose meaning is the movement itself translate: the bank's fee
  and charge types to `BANK_FEES_*`, ATM and Bancomat withdrawals to
  `cash_withdrawal`, FX conversions between the holder's own currency
  accounts to `internal_transfer` — the matcher cannot pair those,
  because the legs differ in currency — and a bill paid to a card to
  `card_spend`. A value the map does not hold is the normal case, not
  drift: the row falls through exactly as an unmapped category does,
  and nothing is counted. The shape is a flag on the map
  (`categorical`), so a bank's payment orders can never drown the
  canary a card issuer's drift is meant to trip.

The UBS **card** vocabulary is the categorical one: ISO 18245 merchant
category descriptions in UBS's own spelling, typos and airline names
included, written as observed rather than corrected. One value is
deliberately left untranslated — the bank's own catch-all for a card row
that moved money rather than bought something (a mobile-payment
transfer, a card top-up). It names no line of business, so a translation
would file person-to-person transfers as shopping; leaving it unmapped
sends the row to the model, which sees the descriptor.

The map carries every UBS FX spelling for vocabulary completeness, and
none of them can now be placed: every era's FX booking reaches gold as
an `fx` kind, which the enrichment population excludes by kind (§1), so
it is the kind and not the tier that keeps those rows out of spending.
The MT940 `NFEX` shape — a movement whose narrative is a bare tag — was
the one exception until the adapter learned to read the `:61:` type
code for exactly those rows. See docs/adapters/ubs.md §7.

The UBS web export puts the payer's own message ahead of the booking
type in the column the adapter reads (`THANKS; e-banking payment
order`); the adapter splits it off, so the provider tier sees the
booking type alone and the message travels as the row's memo, which
gold stores at the end of the description behind the memo separator
(§4, docs/adapters/ubs.md §7).

---

#### Reading the taxonomy

The vendored values shout in full caps and repeat their primary in the
detail; the six deltas are lower-case, because one vocabulary is
Plaid's and the other is ours. Each row therefore carries what it
should READ as beside what it IS: `spend_categories.label` and
`.primary_label` (migration 0058), seeded from `canonical.SpendLabel`.
`FOOD_AND_DRINK_GROCERIES` reads "Groceries",
`GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE` reads "Other general
merchandise", `internal_transfer` reads "Internal transfer".

The rule is mechanical, so a value it reads wrongly is corrected by
hand — `canonical.spendLabelOverrides`, and the same correction seeded
into the dimension. One so far: `card_spend` reads **"Uncategorized
card spend"** (migration 0062). The rule read it "Card spend", which is
true of every card purchase in the product, so among the merchant
categories on a chart it read as a KIND of spending rather than as the
placeholder §2 defines it to be.

The label is presentation and nothing more. The value stays the join
key, the name a rule and a pin write, and what the model gauntlet
validates — so a taxonomy refresh still diffs against the vendored
spelling. The CLI's `category` column and the Metabase pickers render
the label; `category_id` and the model's `spend_*_id` columns carry the
value for a caller that needs a key a rewording cannot move.

#### The issuer's view, kept

`transactions.provider_category` holds what the issuer called a row,
verbatim. `spend_txn_enrichment.provider_spend_detailed` holds what our
vocabulary translates that string to (migration 0057), and
`spend_txn_categories()` publishes it beside our own verdict together
with the primary it rolls up to.

It is a record, never a verdict of ours. It is written for every row
the vocabulary translates — whether or not the tier went on to claim
the row, and whether or not a tier above overruled it — so the
disagreement between the issuer and us is a reportable number instead
of a silent overwrite. Three values, three meanings:

| `provider_spend_detailed` | the issuer |
| --- | --- |
| a specific value | filed the row under a real line of business |
| a catch-all (`*_OTHER_*`) | filed the row, but said no more than the primary |
| NULL | published nothing this build translates |

The last two must never be collapsed. A mapped catch-all is a real if
uninformative opinion; NULL is no opinion at all, and the distinction
is exactly what the decline rule turns on.

Reports show our verdict by default and the issuer's on request.
Nothing sums across the two — they disagree on roughly a third of the
rows an issuer classifies, which is the reason both are kept.

A rule may also MATCH on `provider_category` (§ The rule tier), which
is what makes the issuer an input to our own classification rather
than a tier that outranks it.

### Re-asking the model: `--all` and `--refine`

A run asks about the backlog — signatures no tier could place. Two
flags widen it, and they choose different things:

- `--all` re-asks every signature candidacy admits, placed or not. That
  is what a taxonomy revision or a model change wants.
- `--refine` re-asks only where the MODEL's own verdict is a catch-all:
  it was asked, and could say no more than the primary already did.

`--refine` is scoped by PROVENANCE, not by value, and that is the whole
point. A catch-all a rule or a pin placed is a considered decision —
the taxonomy has no word for a portrait photographer or for household
removals, so one was chosen deliberately after checking — and a pass
that re-asked those would undo the work and push private individuals at
a model. `spend_categories.catch_all` (migration 0060) is the same
predicate as `canonical.CatchAllSpendDetailed`, as data, so the query
and the enrichment pass share one definition of what a catch-all is.

## 4. Merchant signatures

`spending.Normalize` reduces a narrative to the key that groups every
visit to one merchant into a single thing worth categorising once: fold
to upper-case ASCII (with Latin-1 accents folded, because the same
merchant is spelled with and without them depending on the export),
collapse every non-alphanumeric run to a space, strip leading
direct-debit mandate boilerplate, drop a leading payment-processor
token, drop reference-number-shaped tokens (four or more digits and
nothing else), drop every phone-shaped run of what is left (spaced
digit groups: two or more consecutive all-digit tokens carrying six
digits or more between them, at least one of them three digits or
more — a phone number as a statement spaces it, with or without its
country code, and still over that bar when a four-digit group has
gone to the reference test; a single digit group between words is not
a run, so a house number or a numbered chain stays, a house number in
front of a postcode is a lone group by then, the postcode being gone,
a run under the floor — two house numbers, `24/7`, a day and month
once the year is gone, a time, a price in francs and cents — stays
too, and so does a run of pairs over it with no group of three — a
date with a two-digit year, a day and month with a time behind them
once the year is gone, three house numbers, and, deliberately, a
number written as five pairs), and cap at 64 characters on a
whole-token boundary.

A narrative is also read for its **head**: the segment that names the
payee. A statement narrative is written in segments — the payee, the
street, the country and town, then what the payment was for — and an
MT940 `:86:` narrative leads with the structured-field tag the payee
was written into (`Z44?`: a letter, two digits, a question mark). The
head is the narrative less that tag and less everything from the first
`;`. Neither half is the merchant. The tag names the field slot and
differs across bookings of one merchant; the address behind it is
spelled differently across bookings too — a town in full or
abbreviated, a postcode before the name or after it — so left in, one
merchant gets a separate signature per spelling, and an address
trailing a name reads to a model as part of it. Only the KEY is
trimmed: the fence that decides whether a signature may be sent
anywhere reads the whole narrative (`RowTransferShaped`), so no rail
written in a later segment slips through because the key stopped
showing it.

Both fields are reduced, and which one becomes the signature is
decided on the reduced forms, in this order. When the counterparty
is, whole, an e-bill rail marker — `EBILL-RECHNUNG`, `EBILL INVOICE`
or `E-BILL`, hyphen or space, any case — the description from the
segment after the marker, reduced segment by segment and less the
payment-order boilerplate around the creditor (below): the marker
names the rail the bill came in on, never the creditor, and it has
words in it, so no later step would refuse it. When the description
reduces to nothing, the counterparty, whatever it holds — a bare code
is kept over an empty signature, which would drop the row out of every
store. When the counterparty reduces to nothing, or to no word at all
(`Uninformative`), the head if the narrative was structured — a field
tag led it — and the head carries a word, else the description whole.
The tag is what says the first segment is the payee: an untagged
narrative leads with the booking type as often as with a payee
(`Direct debit; <merchant>; <what for>`), and trimming that to its
first segment would file every direct debit under one key and lose
the merchant behind it. When the head begins with
the counterparty's tokens and carries more, the head: the
counterparty is then a truncation of it, and the rest of the segment
is what tells one creditor from another — unless what follows the counterparty's
tokens, the counterparty read with its own runs gone, is a
phone-shaped run, when the counterparty stands whatever else follows:
a name with its number behind it is the whole name, not a truncation
of one, so `<counterparty>; <phone>; <more>` keys on the counterparty
alone. The shape that needs this is the person-to-person contact
line, the payee, the number and the rail's reference, keyed the same
whether the payee carries the number in the description alone or in
both fields; a merchant's line with the number in front of its
address keys on the name by the same rule, and with the number behind
its address on the name and the address, the number gone. Only the
spaced form is a run: a number printed unbroken is a reference number
to the four-digit test, gone before the precedence is read, and the
description is then a truncation like any other. Otherwise the
counterparty — it is the merchant field proper, and adapters are
contractually bound not to reformat it. The first two exceptions exist
because an adapter's counterparty can be silver's promotion of the
narrative's first segment, and that segment can be nothing but the
bank's own notice (`CRD1W OBJECTION TO UBS`) while the creditor sits
in the description. What the head trim leaves behind is not lost with
it: a care-of line or an address past the separator is the bank's
filing of where to send the post, and the rule tier reads the raw
narrative, so a card bill written `UBS SWITZERLAND AG;C/O UBS CARD
CENTER` keys on the payee and is still placed by the card rule.

The description is read only up to its **memo separator**
(`canonical.DescriptionMemoSeparator`, a spaced em dash). Where a
source carries the payer's own free text about a row — the message
typed on a UBS e-banking order — the adapter emits it apart, as the
change's `Memo`, and the gold writer stores it *after* everything the
bank wrote, folding any separator the narrative itself carried before
it joins the two; every adapter's rows enter gold through that one
writer, so the separator has exactly one meaning there. What follows
it is the payer's words, not the payee's identity: it is shown
wherever the description is shown and a config rule can key on it
(what a payment was for), but it never enters the signature, never
fires a built-in rule (§3), and `categorize` cuts it from a descriptor
before the narrative stands in for a missing counterparty (§5). On the
UBS caption shape the description is the field the signature is built
from, so without the cut every message would have been its own
merchant.

The boilerplate steps are the two rewrites among the folds. A Swiss
direct debit (LSV) arrives with the mandate notice *before* the
creditor — `DIRECT DEBIT; CRD1W OBJECTION TO UBS; WITHIN 30 DAYS;
EXAMPLE CREDITOR AG; …` (synthetic values, exact structure) — so keyed
on its leading tokens every such row folded to the signature of the
notice: one fake merchant swallowing every creditor on the rail alike,
for the model to name and file as one merchant. The notice
is the bank's format, not the holder's data, so it comes off before
the signature is built and the signature starts at the creditor. The
match is narrow: the `DIRECT DEBIT` marker (optional — the web adapter
carries it as a booking type, so the free text may begin at the
notice), then `<CODE> OBJECTION TO <BANK>` (required, one token each:
the notice's fingerprint), then `WITHIN <N> DAYS` (optional), at the
head of the narrative and nothing else. A narrative with the words but
not the shape is untouched. When the notice is all there was, the
mandate code is kept as the signature — it is the creditor's LSV
identity, and being a code rather than a word it is refused at
candidacy by `Uninformative` instead of becoming an empty signature.
That last case is every direct debit on the UBS
adapter, whose counterparty is the promoted first segment and so holds
the notice and nothing else; the precedence above is what reaches the
creditor there, by building the signature from the description
whenever the counterparty reduces to no word.

An e-bill is the second rewrite, and the one that precedence cannot
reach. In the statement era the bank prints the rail marker as the
*first* narrative segment and the creditor second, so the adapter
promotes the marker into the counterparty and composes the
description as `PAYNET ORDER; EBILL-RECHNUNG; EXAMPLE TELECOM AG; CH
EXAMPLETOWN 9999; QRR; 0000…; 1 times E-Banking domestic` (synthetic
values, exact structure; the three spellings a statement prints are
`EBILL-RECHNUNG`, `EBILL INVOICE` and `E-BILL`). The marker has words
in it and is no prefix of a description that leads with the booking
type, so keyed on the counterparty every e-bill on the rail would
share one signature per spelling of the marker, and two creditors
would share one verdict. A rail marker is never a merchant. When the
counterparty is one — whole, hyphen or space, any case — the
signature is built from the description's segments after the marker,
so the creditor leads, and the lines the payment order prints around
the creditor come off: the `QRR` marker, the `<N> times E-Banking
domestic` execution line, a page footer — two lines, `Form without
signature Page …` and then the form id, a counter and the statement
date, dropped as a pair wherever the break falls, between the marker
and the creditor included, where the second line would otherwise
land in the creditor's slot — and any segment with no word in it at
all, a lone country code (a segment that is only a long reference
number is already gone to the digit stripping). The
creditor's own slot, the first segment kept, is exempt from the
no-word test, since a name can be an abbreviation the word test
refuses; and nothing else is stripped — the postal address stays,
because a creditor named with nothing but an address is still that
creditor, and the fence (§5) knows an address from an IBAN. A marker
with nothing behind it yields no signature rather than the marker:
unlike the mandate code it carries no creditor identity worth a key,
and having words in it it would reach the model and become the one
fake merchant the strip removes. A plain payment order —
`E-BANKING PAYMENT ORDER; EXAMPLE PAYEE AG; CH; 1 times E-Banking
domestic`, its counterparty the payee — is untouched, and so is a
creditor whose name merely carries the word: only a field that holds
the marker and nothing else is the rail.

`SignatureVersion` stamps every signature the current rules produce.
It exists so a change to the normalisation can be told apart from a
change in the data: the merchant store keys paid-for verdicts by
signature, and a silent re-normalisation would orphan them behind keys
nothing will ever compute again. On a bump, the pass carries
older-version verdicts forward onto the new keys — but only where the
new key has no verdict yet, so a real re-categorisation is never undone
by a stale copy, and `model_name` is preserved verbatim so a carried
verdict does not claim to be new work.

**A verdict is carried only across a one-to-one move**: every row that
carried the old key must now carry the same new key. That is what a
refinement of a genuine merchant's signature looks like — a store
number dropped, a processor prefix stripped — and a verdict about the
merchant is as true at the new key as at the old. When an old key's
rows **split** across several new keys, the old key never was a
merchant but a shape several creditors shared, and the verdict bought
at it describes the shape. Version 2 is that case: the boilerplate
strip above moves every direct-debit row's key from the notice to its
creditor, and one notice key covers every creditor on the rail under
whatever the model made of the notice. A carry by old key alone would
file each of them under that verdict; instead the verdict is left in
the store, unreachable, and `load` reports the count (`spending: N
merchant verdict(s) left behind …`), which is what puts those
merchants back in the backlog visibly. A partial move — some rows
stay at the old key, others leave — is a split too; the rows that
stayed keep their verdict. `wealthdb categorizations --forget` (§8)
removes a verdict outright, and forgetting an artefact's key *before*
a bump is the clean way to keep it from carrying anywhere, one-to-one
move or not.

Version 3 corrects version 2 on the UBS adapter, whose counterparty is
the promoted first segment: there the strip left the bare mandate code
as the whole signature, one code per creditor; a code is
`Uninformative`, so the model was never re-asked, and the re-key moved
each notice verdict onto its code one-to-one — the split guard was
correctly silent, because a code key really does cover one creditor — so
the notice verdicts carried onto the code keys as artefacts, and the
card rule, which read only the signature, never saw `UBS CARD CENTER` in
the description. Ordinary transfers failed the same way through
`UBS SWITZERLAND AG` alone. Version 3 builds the signature from the
description in both shapes (the precedence at the top of this section),
and the built-in rules read the narrative fields as well as the key
(§3). A verdict carried onto a code key that way is what forgetting
before the bump (§8) is for.

Version 4 lands three reductions at once. The first is the e-bill
strip above. Version 3 keys an e-bill on the UBS adapter's statement
era by its counterparty, the marker, so its key there is one of three,
one per spelling, and covers every creditor the rail carries; under
version 4 those rows split onto their creditors, and a verdict at any
of the three keys is left behind and reported. The second is the memo
cut. The cut itself moves no key: a description composed without a
memo never carries the separator, so for every such narrative the key
under version 4 is the string version 3 computed. What does move keys
is the UBS adapter change that ships with it. A web-export row with a
message and no caption was keyed *on* the message — `<message> <type>
<detail>`, one key per message — and is keyed on `<type> <detail>`
now; a reference-led order (`<reference>; order`) was one key per
reference and is the bare `ORDER`. Several old keys collapse onto one
new key there, each a one-to-one move from its own side, and the bump
is what lets the carry run: the first old key in row order carries its
verdict onto the new key, and the others' verdicts stay behind at keys
nothing computes any more — not counted as splits, because each old
key moved whole. `ORDER` and its like — a signature that is nothing
but the bank's booking type — are refused at candidacy (§5), so the
collapse buys no verdict about a booking type. The third is the
phone-run drop with the contact-line exception to the truncation rule
that goes with it. A run is spaced digit groups — six digits or more
in total, at least one group of three or more — and version 3's digit
test let such a number's groups through one by one, so a
person-to-person mobile payment in the web-export shape — `<payee>;
<phone number>; <reference>`, the payee promoted as the counterparty
— was keyed on the payee, the number and the reference and is keyed
on the payee alone, the key the statement era gives the same shape
behind its booking type, while a number anywhere else in a narrative
drops out of its key the same way; the digit pairs a narrative
carries for other reasons — a date, a time, a price, house numbers —
have no group of three and keep their keys, and each old key moves
whole. Everything else is keyed exactly as version 3 keys it.

Version 5 closes the memo fold's edge shapes
(`canonical.JoinDescriptionMemo`). Version 4 folded each separator a
narrative carried, so one that merely happened to print a spaced em
dash could not be read back as carrying a memo. Three shapes outlived
that fold and were read as a memo boundary anyway, and two of them
move keys. Two **adjacent** separators overlap, so the second survived
a single pass: the description was split at the survivor and
everything behind it dropped as the payer's words, where the whole
narrative is keyed now. A narrative **opening** on the bare separator
read as all memo and no narrative at all, so the description
contributed nothing and the counterparty decided the key alone; it is
a narrative again. The third — a narrative **closing** on the bare
separator — only moves where the memo boundary falls, and the memo is
dropped from the key either way. The rows that shared one cut-down key
move onto keys of their own: the split case above, where the verdict
stays in the store and `load` reports it. Everything else is keyed
exactly as version 4 keys it.

Version 6 closes the memo cut's own mirror of that hole
(`canonical.SplitDescriptionMemo`). The cut searched for the separator
anywhere in the description before it tested the memo-only prefix, so a
description that is **all memo** — no narrative at all — whose memo
carried a spaced em dash in the payer's own words was cut *inside the
memo*: part of what the payer wrote came back as the narrative and was
keyed, tested by the built-in rules and eligible as a model descriptor,
which is the one thing the separator exists to prevent. The prefix is
tested first now, so such a description is memo whole and the row falls
back to the counterparty, or to no signature at all. Folded with it is
the last narrative that could open on the bare prefix without being one:
a narrative that is **nothing but the dash**, which the separator's
leading space turned into the prefix, so its description read as all
memo and the counterparty decided the key alone; it is a narrative
again. Both shapes move rows that shared one cut-down key onto keys of
their own — the split case above — and everything else is keyed exactly
as version 5 keys it.

Version 7 changes no reduction at all: the rules are version 6's. What
moves is what the UBS adapter hands them. The MT940 feed and the
account-statement export both record the bank's own bookings, and
where they overlap the era cut gives the MT940 row to gold — which,
when the bank wrote nothing but its own code into the `:86:` narrative,
reaches gold as that code, with the `:61:` type code as its whole
provider filing and no payee at all. Such a row is keyed on the code: a
key no tier can place, refused at candidacy as the bank's own filing,
and in the backlog for good. The adapter now folds the export's record
of the same entry — matched on the bank's transaction number, which the
MT940 line repeats as its own reference — onto the columns the MT940
row left as codes, so the row is keyed on the payee the export names,
and the booking type the export prints reaches the provider tier in
place of the SWIFT code. Amounts, dates, kinds and ids stay the MT940
row's, untouched. Each moved row leaves behind a code key that other
rows may still share — a code-only booking the export never carried
keeps it — so this is the split case above: a verdict bought at a code
key stays in the store, and the rows that moved are back in the backlog
under a key that names someone. Everything else is keyed exactly as
version 6 keys it.

---

## 5. What leaves the machine

Two **independent** gates, and neither can be relaxed by the other.

### The fence gates candidacy

`spending.TransferShaped` reports whether a narrative looks like money
moving between accounts or between people rather than money being
spent at a merchant: whole-token rails (`WIRE`, `ACH`, `SEPA`,
`ZELLE`, `P2P`, …), multi-word spellings of the same rails, and an
IBAN-shaped run — two letters, two check digits and an account body,
spaces removed — that begins where a token begins. The anchor is what
keeps a postal address out of it: `HAUPTSTRASSE 12; CH EXAMPLE 9999`
holds the same letters-digits-letters sequence once its spaces are
gone, and a fence that read it as an account number kept every
wire-paid bill — a tax office, a dentist, a utility — from the model,
which is the population only the model can place.

It is a **fence, not a classifier**. The rule tier decides what such a
row *is*; this decides what may be shown to a model at all. A P2P
narrative carries a counterparty's NAME where a merchant would be and
an IBAN carries an account number, so the fence errs towards firing —
a false positive costs nothing worse than one uncategorised row.

The fence gates **candidacy, at every context level**. A wire never
becomes a candidate whatever the context is set to. This is what makes
the context knob a *description* setting rather than a *scope*
setting: widening it changes how much is said about the same
candidates, never which signatures are candidates.

The fence runs a second time on the raw narratives the wider levels
send. A signature is a folded, capped view of the narrative it came
from, so a rail token past the 64-character cap survives in the
narrative and not in the key; a fenced narrative is dropped while its
candidate stays, which keeps candidacy identical across levels.

A reduction can also drop a rail the row still carries. A mobile
person-to-person rail *leads* a narrative rather than naming the payee,
so a reduction that prefers the counterparty leaves a key that is a
private individual's name and nothing else — not transfer-shaped,
carrying a word, and not the provider's filing, so every key-only
refusal passes it. `spending.RowTransferShaped` is the reading that
closes that: it applies the fence to the reduced key, to the provider's
own filing of the row and to the description's narrative half, and
fires if the rail is written in any of the three. The memo half is not
read — it is the payer's words about the row, it names no rail, and
reading it would refuse a merchant over a note beside it.

That is the reading candidacy uses, and it is applied to a SIGNATURE
rather than to one row: the reduction is many-to-one, a key is what
leaves the machine, so a key is refused when ANY row under it is
fenced, whichever other row would have carried it in. `wealthdb
categorize` therefore reads the fence over the whole spending
population once and applies the result at all three places a signature
leaves: the candidate list, the neighbour lists the `transaction` level
attaches, and the anchor block. Reading it row by row at each site
would fence less — a key admitted from a clean row while the rail sits
on a row that site never sees.

ATM narratives are deliberately **not** fenced: they name a bank and a
place, never a person, and the rule tier categorises them without any
model involvement.

### The context level decides depth

`spending.categorization.context`, in increasing order of what leaves
the machine:

| level | what is sent |
|---|---|
| `merchant` *(default)* | merchant signatures and a transaction count — folded, truncated, reference-number-free strings. No amounts, no dates, no accounts. |
| `descriptor` | plus the raw statement narratives the signatures were folded from, which carry the spelling, the branch, the city. Capped by `descriptor_samples`. |
| `transaction` | plus date, amount, currency, account kind, and the signatures of what was bought within a day of it on the same source. |

**The default is the most private level, by decision.** Every level
above it improves accuracy on ambiguous merchants and widens what a
third-party endpoint learns about a household in exchange. A
deployment that wants that trade makes it explicitly; nothing makes it
silently. The name is validated at config load, so a typo fails rather
than resolving to some other level.

---

## 6. The model tier

`wealthdb categorize` buys a verdict **per merchant signature**, never
per transaction, and stores it in the GLOBAL
`spend_merchant_categories` table. A merchant met on several accounts,
at several sources, is paid for once and answered once. This is a
deliberate departure from `symbol_resolutions`, which is per-source
because a symbol is only meaningful inside its source's id space.

**Candidates** are the backlog: signatures with at least one
transaction nothing deterministic could place *and* no verdict in the
store. `--all` widens that to every signature in the spending
population — what re-asks a merchant after a taxonomy revision or a
model change. The fence applies to both.

So does a narrower refusal, `spending.Uninformative`. A
signature with no word in it — no all-letter token of three or more
letters, which is what an MT940 `:86:` narrative reduces to when the
bank wrote nothing but its own booking code or a two-digit tag — is
never a candidate either, at any context level. It carries nothing to
name, so a model can only echo it back and the gauntlet can only
reject the echo; refusing it at candidacy saves the round-trips. The
claim is deliberately *not* about which words are merchants, only
that no word at all means nothing to name: a token mixing letters and
digits is a code, while an all-letter code cannot be told from a word
and goes to the model, where the gauntlet's verbatim-echo check
remains the backstop. One more refusal is counted with it:
`spending.FilingOnly`, a signature that is nothing but the provider's
own filing of the row — the booking type an adapter composes a
description from when the bank wrote nothing else (`order`, `credit`,
`FEES`), reduced as the signature was. There is a word in it, but it
names how the bank booked the row, not whom it paid: one such key
covers every row the bank filed that way, and a verdict bought at it
would cover them all. A narrative that carries more than the filing
(`credit; Ref 7`) is not refused. Both counts print with the plan — "N
signature(s) fenced as transfer-shaped and never sent", "N
signature(s) uninformative and never sent" — so a row that stayed
uncategorised for either reason is accounted for rather than wondered
about.

**Anchors** are verdicts already in the store, newest first, shown as
in-context examples. They exist to steer the model towards its own
prior vocabulary: left to itself it will place one coffee chain under
`FOOD_AND_DRINK_COFFEE` and the next under
`FOOD_AND_DRINK_RESTAURANT`, and a report that splits one habit across
two categories is worse than one that puts it in a single wrong
category. An anchor carrying a delta value is skipped — it would
teach exactly the vocabulary the gauntlet then rejects. So is an
anchor whose signature the current transfer fence refuses: the store
is append-only across signature revisions, so a verdict keyed on a
signature an older fence admitted would otherwise keep going out in
every prompt, long after no transaction carries it. The verdict itself
stays usable locally — the enrichment lookup applies it by signature —
and the fence is read both ways here, on the key alone (all an orphaned
verdict offers) and over the rows that still carry it. Within a run
the set is refreshed between batches: the verdicts a batch has just
had accepted are the freshest sample of the model's vocabulary there
is, so they are folded in ahead of the store's rows and the block is
cut back to `--max-anchors`. It never grows with the run.

**Batches.** The candidates are sent `--batch` at a time (default 40).
The size is set by what a local model answers inside the transport's
five-minute call ceiling: a backlog offered whole in one call times
out, and no local model should be asked for an answer that size.
Each batch carries the same instructions,
the whole taxonomy and the anchor block; each retries on its own
feedback, so a bad row costs its batch a round-trip and never the run;
and each batch's accepted verdicts are **stored as it completes**, in
their own transaction, before the next batch is asked. A run that
dies part-way therefore keeps every batch before the failure, says so,
and a re-run asks only about what is still unanswered — the backlog
excludes whatever the store now covers. The batch plan (count, sizes,
anchors, the first prompt's size and rough token count, the best- and
worst-case call count) prints before the first call, and one progress
line prints per batch; the run summary aggregates across batches.
`--show-prompt` prints the first batch's prompt in full and each later
batch's size only.

**The protocol** is CSV at temperature 0, with retry-on-feedback: each
rejected row is quoted back with its reason and the model re-emits.

**The gauntlet** rejects a row when:

- the signature was not in the candidate set that was sent;
- the category is one of the deltas (rejected with a specific reason,
  so the retry says *why*, in any casing);
- the category is otherwise outside the vendored values;
- the merchant name is empty, or is the signature verbatim.

The row must carry exactly three columns. Unlike the resolve-symbols
parser — where the key is three columns and the value is one, so "the
last column" disambiguates a padded row — two of these three fields
are free text and a padded row cannot be read without guessing.

**A dry run is a pure read.** It opens gold read-only, which means it
*cannot* run the deterministic pass first, because that pass is a
write. So it does not, and it labels its plan as computed against the
enrichment *as of the last load* rather than quietly planning against
a different world than a real run would see. It still asks the model,
batch by batch, exactly as a real run does — that is the precedent's
shape, and the only way to see verdicts without writing them — which
is why the batch plan prints before the first call: it is the cost
signal that arrives before any waiting.

**Every run ends with canaries**, because the run's own counters say
whether the model behaved and nothing about whether the spending
picture is right:

- **categorisation rate per source** — one source falling behind the
  others is usually a normalisation or adapter problem, not a model
  one;
- **provider-map misses** — a card issuer's categorical vocabulary has
  moved and the map has not; a bank's untranslated booking types are
  rails, and the values the map marks `untranslatable` — an issuer's own
  residual bucket, and a category that names a money movement rather
  than a line of business — are reviewed, so none of those is a miss
  (§3);
- **the matched-pair listing, both legs** — the audit surface for the
  matcher, the one tier that *removes* rows from spending. Its
  mistakes are invisible in a chart: an over-eager pair makes a month
  cheaper, it does not make a category wrong. Only pairs with at least
  one leg in the spending population are listed — those are what left
  spending. The matcher pool is every account in gold, so most of what
  it pairs was never a spending candidate (a move between two
  brokerage accounts, wallet to wallet); those are counted on one
  line, `N pair(s) matched outside the spending population, not
  listed`, rather than listed or silently dropped;
- **the largest unmatched legs**, including opposite-sign
  cross-currency shapes the native-currency matcher structurally
  cannot pair;
- **a stratified sample of what is still uncategorised**, spread
  across sources rather than taken from the head of the biggest one.

---

## 7. Reading it: `wealthdb spending <view>`

Three views, one per report macro, with the grain in a positional and
everything else in a flag — the returns family's shape:

| view | a row is |
|---|---|
| `summary` | a period bucket: `txn_count`, `spend`, `refunds`, `net_spend` |
| `categories` | a (bucket, category) pair, plus its `share` of the bucket |
| `transactions` | a spending line: merchant (the store's name, else the line's own signature; none on a delta line, the issuer on a card bill), category, provenance, amount |

The window is positional and defaults to the **trailing twelve
months**, where the returns window defaults to inception. A return is a
cumulative fact about the whole history; spending is read against
recent habit, and a window reaching back past the day a source's card
ledger begins covers a cash-only population — a `total` over it answers
a different question than it appears to.

`--period` is `daily | weekly | monthly | quarterly | annual | total`
(default `monthly`), which is the returns family's vocabulary extended
downwards: a quarter is the coarsest interesting return bucket and
about the finest interesting spending one. `--level primary|detailed`
picks the vocabulary `categories` groups by.

**There are no row-filter flags**, matching `holdings positions` and
`transactions`. Filtering by merchant, category or account is `-f json`
plus a downstream filter, SQL, or the dashboard. Beyond keeping the
surface small: a filter flag on a report whose numbers are shares of a
bucket would have to decide whether the denominator moves with the
filter, and either answer is wrong for some reader.

### The provenance column

`provenance` names the tier that decided the category, in the
vocabulary of §3. Seven values:

| value | what it means |
|---|---|
| `manual` | a pin — the holder's own word about one transaction |
| `matcher` | the internal-transfer matcher, which has seen both legs |
| `rule` | a built-in or configured rule |
| `provider` | the source's own filing of the row |
| `model` | the merchant store's verdict for the line's signature |
| `kind` | the floor: what the transaction's own kind says a row IS, where nothing else placed it (migrations 0066, 0067) |
| `signature-only` | no verdict: the pass reached the row, recorded its signature and could not place it |

Five of them are stamped on the overlay row by the enrichment pass and
are what the `spend_txn_enrichment` CHECK admits (migration 0041).
`model` and `kind` are stored nowhere. The model tier writes to the
merchant store rather than the overlay, and the kind floor reads no
verdict at all; both are derived by
`spend_txn_categories()` where the two scopes meet (migration 0050),
which is the one place a store verdict is distinguishable from the
backlog — at table level such a line carries `signature-only`, the same
value an unplaced row carries. `signature-only` is the backlog
`wealthdb categorize` asks about (§6).

### The merchant column

`merchant` resolves in three steps, and the first that answers wins:

1. On a line whose resolved category is a **delta**, the enrichment
   row's `merchant_label` — the issuer on a card bill, and nothing on
   every other delta (migrations 0048 and 0052, below).
2. Otherwise the **merchant store's name** for the line's signature:
   the prose a model wrote for that merchant.
3. Otherwise the **line's own `merchant_signature`**, verbatim
   (migration 0054). A line whose signature is absent, empty or blank
   shows nothing.

Step 3 is there because step 2 can only ever answer for a line the
model tier saw. The store is written by the model tier alone, and the
model tier is the weakest scope in the lattice (§3): a line a stronger
tier placed is never a model candidate and so never acquires a store
row — a card row the provider filed under its own merchant category, a
row a rule or a pin named. A line nothing placed has no store row by
construction, a store row being exactly what would have resolved it.
All of them carry a signature the pass computed, which is the very key
a store row would have hung on, and that signature is a merchant
identity: the fold of the statement narrative the merchant printed.
Falling back to it publishes what the line already knows instead of a
blank.

The fallback is deliberately **not** restricted to a resolved
category. The backlog names its merchants too — which is most of what
makes a backlog readable at all.

It is not restricted by **candidacy** either, and that is the one
widening to know about. `Uninformative` and `FilingOnly` (§6) refuse
the model a signature that is nothing but a booking code, or nothing
but the provider's own filing of the row: one such key covers every
row the bank filed that way, so a verdict bought at it would be
bought for all of them. Those gates fence the STORE, not this column,
and the normaliser keeps such a fold rather than storing an empty
signature — so a line whose whole narrative was the bank's own tag
shows that tag here, and ranks under it in a report that ranks
merchants. The alternative is a blank, which says less and hides a
line a reader could recognise.

**Verbatim, not cased for display.** A store name is prose and a
signature is upper-cased tokens (§4), and the column keeps both as
they are. The cell is then the fold itself: the exact value of the
`merchant_signature` column beside it, where casing would print a
string that exists nowhere else in gold. The fold also labels itself —
upper case tells a reader the name was derived rather than written,
without consulting `provenance`. And identity is a stable rendering by
construction, so one signature always renders one way and a report
groups its lines as one merchant; a display caser could fold two distinct signatures, or a
signature and a store name, onto one label and merge rows the
resolution holds apart. `initcap` does not exist in this DuckDB
besides, so casing would mean a hand-rolled `list_transform` caser
inside a macro every report reads, for a worse answer on acronyms and
brand casing.

**A delta line carries no store name** (migration 0048), whatever the
store holds for its signature, and no fallback either, because a delta
line is not a merchant transaction: a gift names the recipient, an
own-account move the holder's own bank, cash out of an ATM a bank and
a place, and none of those is who the money was spent with. The rule is per line, not per signature: a pin that
places `gift` on one row of a signature the store has named blanks
that row alone, and its siblings keep the name and the category the
store gave them. Nothing else moves. The store keeps the verdict —
`wealthdb categorizations` still lists it — and the resolution
underneath is the lattice of §3, unchanged: the delta placed at
transaction scope outranks the store's verdict, and the merchant
column follows whichever value won.

**A card bill is the one exception, and it names its ISSUER**
(migration 0052). A `card_spend` line is a bill for a card wealthdb
does not itemise, so there are no purchases to name — that absence is
the whole reason the placeholder exists — and the issuer the bill was
paid to is the only handle there is on which card the money went to.
Without it the line is one undifferentiated bucket per period. The
name does not come from the store, which holds whatever a model made
of a narrative naming the payer's own bank: it comes from the
card-payment rule's issuer table (§3), carried out of the tier as a
label and stored on the enrichment row (`merchant_label`), and the
macro reads it exactly where the store's name is refused. It is the
card rule's alone — a bill recognised by a generic card-payment phrase
or by a masked card number names no issuer and stays blank, and the
model tier, the provider tier, a config rule, a pin and the matcher
never write one — so an issuer appears in the column only on a line
this rule filed as a bill to that issuer.

The label is a merchant column, not a merchant: a report that ranks
merchants excludes the delta categories rather than reading a blank
merchant as "not a delta", or an issuer ranks among shops. The
dashboard's two rankings do exactly that (web/DESIGN.md), which is
also why the fallback needed no change there — the lines it names rank
as the merchants they are. What ranks widens with it: every non-delta
line carrying a signature, at the grain the fold gives it, so ranks
and rank 1's share move with the population.

Delta-ness is read off the `spend_categories` dimension rather than
restated as a list: a delta is primary-level (`spend_primary =
spend_detailed`, §2), so a delta added to `canonical` and seeded is
blank in the merchant column with no change to the macro — and, being
unlabelled, blank whatever the enrichment row holds. Everything
downstream inherits the column from `spend_txn_categories()` by name —
`spending_lines_base`, the three spending reports and their `_multi`
siblings, `report_transactions` (so `wealthdb transactions` shows no
merchant on an own-account move or a subscription either, and shows
the fold on a provider-placed purchase), `web_spending` and the
dashboard's merchant rankings. None of them re-derives the column, and
none was re-issued for any of the three steps.

### What `-p` redacts

`merchant` redacts as **free text**, like the narrative it was named
from, and the fallback is the plainest reason why. The fence (§5)
gates candidacy for the merchant STORE, not the column: a wire, an
ACH, a P2P narrative — the shapes that carry a counterparty's NAME
where a merchant would be — is refused a verdict and therefore has no
store name, and lands on step 3 with a signature folded from that
narrative. So a payment to a person now surfaces its payee here, on
the non-private view, exactly as `merchant_signature` and
`counterparty` beside it already do. The column is free text rather
than a name because that is what it holds.

Step 2 carries the same exposure by a slower route, and carried it
before the fallback existed. The store is append-only across signature
revisions and across widenings of the fence itself, and the enrichment
lookup applies a stored verdict by signature for as long as it is
there: a name bought while the fence was narrower outlives the fence
that would now refuse it, and the name is what a model wrote from the
narrative it was shown. The guarantee is about what may reach the
store, not about what is in it — and never about what the column
prints.

`counterparty`, `description` and `merchant_signature` reach the same
class more directly: they are the raw narrative and its fold, present
for every row the fence let through *and* every row it stopped.

Free text means the cell is replaced whole by `***`, in every output
format, whatever the value looks like. That last part is the point.
The narratives that carry a person's name — a wire, a P2P transfer, a
cheque payee — are multi-word, so a redactor that first asks "is this
shaped like an identifier?" hides the harmless single tokens and
prints the names in full. A merchant name is the other half of the
same trap: a single legible token, exactly what such a redactor waves
through.

The account's external id and display name redact as account ids — a
card's display name is typically its masked number — and amounts
redact as money. Categories, provenance and the `share` percentage
stay legible, like the returns percentages.

## 8. Storage and lifecycle

| table | keyed by | lifecycle |
|---|---|---|
| `spend_txn_enrichment` | (silver_source_id, transaction_external_id) | derived; rewritten whole by every pass, pinned rows included; `reset <source>` clears THAT source's rows only, and the surviving leg of a cross-source matcher pair keeps its `internal_transfer` verdict until the next `load -a` re-asserts the pass — which is what `reset` prints; a fresh-file `reload -a` does not carry it (the rebuild re-projects the transactions it is derived from, and the pins are re-stamped from the ledger) |
| `spend_merchant_categories` | merchant_signature | GLOBAL and paid for; survives `reset`, and `reload -a` carries it across the fresh-file swap because nothing could regenerate it; a row leaves only by `categorizations --forget` |
| `spend_account_scope` | (silver_source_id, account_external_id) | configuration stamped into gold; re-stamped whole every pass |

The enrichment pass deletes every row it owns — which is every row —
and re-asserts them, rather than scoping the delete to the current
population: an account fenced out of scope, or a transaction a
re-projection dropped, would otherwise leave rows the pass can no
longer reach and therefore no longer clean up. `manual` rows are
owned too: the decision behind one is not in gold but in the pins
ledger, which is config, and a preserved manual row would outlive the
pin that made it.

### Undoing a merchant verdict

The model is sometimes wrong at merchant scope, and the store has no
tier above it: a per-transaction pin outranks a verdict for one row,
not for the merchant. `wealthdb categorizations --forget SIGNATURE`
(repeatable) is the undo. It deletes the rows whose signature matches
exactly — the `merchant_signature` column of the dump, quoted — in one
transaction, prints each removal with its name and category, reports a
signature it did not find without failing, and needs write access;
`--dry-run` prints what would go and writes nothing. Nothing else
changes: the next `categorize` run re-asks a forgotten merchant
naturally, because the backlog is whatever the store does not cover.

Forgetting is also how a verdict is kept out of a signature-version
re-key (§4). The pass carries a verdict only across a one-to-one move
and leaves a split behind, but an artefact whose rows all happen to
move to one creditor would still follow them; forgetting its key before
the bump stops it carrying anywhere. Forgetting it after the bump only
tidies the store — by then the rows have moved on.

---

## 9. Decisions of record

- **Family name `spending`; outflows only.** Income is
  [INCOME.md](INCOME.md); buys and sells remain the future `cashflow`
  feature.
- **Plaid PFC, vendored verbatim**, plus extensions and deltas. A
  revision arrives as a diff. The dimension carries both families, told
  apart by a `family` column (migration 0069); the two vocabularies are
  fenced from each other by predicate, so a spending rule cannot place
  an income value.
- **A rewards credit is income, not an offset against card spend**
  (migration 0070). The `reward` kind left the spending population: a
  statement credit, an account-opening bonus and a referral bonus all
  arrive under it, and only the first has any relationship to spending
  at all. Nothing in gold changed — no adapter emits the kind yet.
- **Capital deployed is not spending** (`investment`, §2). It is an
  own-account move when the destination is tracked and an
  `investment` when it is not; either way it leaves the base.
- **An unpaired card payment is card spend, not an own-account move**
  (`card_spend`, §2). `internal_transfer` is the wrong verdict for an
  unpaired bill: a household pays bills for cards no collector exists
  for, and every such bill — the only trace of that card's spending —
  would be deleted from the totals, silently and at whatever size it
  was. The rule places `card_spend`, a placeholder primary kept in the
  base and visible as its own line. The matcher still outranks it, so a
  bill whose card *is* collected nets out exactly as before, and
  collecting a card replaces its bills with its purchases on the next
  pass. The rule's issuer table names issuers only — a store card that
  names its merchant is that merchant's spend — and the issuer it
  matched is what the line carries as its merchant (§7).
- **A cash gift is spending, visible as what it is** (`gift`, §2).
  The vendored vocabulary means a shop by gifts-and-novelties and a
  non-profit by donations, and left to itself the model files cash gifts
  and family support under other general services, which is false.
  `gift` is a primary in its own right, kept in the base by the same
  construction as `card_spend`, and placed only by a config rule or a
  pin — no narrative says a person is family, and the fence keeps
  person-shaped rows from the model regardless.
- **No merchant on a delta line** (migration 0048, §7). The merchant
  column is the store's name for the signature only where the resolved
  category is vendored. A delta line — an own-account move, capital
  deployed, a card bill, a gift, cash out — is not a merchant
  transaction and shows none, whatever the store holds: the holder's
  own name on a card-bill payment, a relative's on a gift, a company's
  on a subscription are not merchants and do not rank among them.
  Decided per line from the resolved value, and read off the
  dimension's own marker (a delta is primary-level) rather than a
  second list, so a new delta is covered by being seeded.
- **…except a card bill, which names its issuer** (migration 0052, §7).
  A card bill has no purchases to name, and the issuer it was paid to
  is the only handle on which card the money went to; without it the
  `card_spend` line is one undifferentiated bucket. The name comes from
  the card-payment rule's issuer table rather than from the merchant
  store — the store holds whatever a model made of a narrative naming
  the payer's own bank — and is written by that rule alone, cleared by
  any tier that overrules it. It is a handle, not a merchant: a report
  that ranks merchants excludes the delta categories rather than
  reading a blank merchant as "not a delta".
- **A line with no store row falls back to its own signature**
  (migration 0054, §7). The store is the model tier's, and the model
  tier is the weakest scope: every line a stronger tier placed, and
  every line nothing placed, is outside it and was blank. The
  signature is the key that store row would have hung on and is a
  merchant identity already, so the column publishes it — verbatim,
  because the cell is then the value of the `merchant_signature`
  column beside it, the fold's upper case tells a derived name from a
  written one, and identity is the only rendering that is stable by
  construction. The candidacy gates gate the store and not the
  column, so a fold that is only a booking tag surfaces under it.
- **Merchant-keyed categorisation**, stored globally. A merchant is
  the same merchant whichever card met it.
- **The description outranks a counterparty that names no merchant**
  (`SignatureVersion` 3, §4). The counterparty is the merchant field
  proper and wins by default, but an adapter's counterparty may be a
  promotion of the narrative's first segment, and that segment can be
  the bank's notice or the bank's own name with the creditor behind
  it in the description. When the counterparty reduces to no word, or
  is a truncation of the description, the signature comes from the
  description; and the built-in rules read both narrative fields as
  well as the signature, so a creditor named only in the description
  is found whatever the key holds.
- **A rail marker is never a merchant** (`SignatureVersion` 4, §4).
  An e-bill's promoted counterparty is the bank's marker for the
  rail, with words in it and the creditor behind it in the
  description; the signature is built from the description past the
  marker, less the payment order's own lines, so a utility and an
  insurer are two merchants rather than one.
- **An address is not part of the merchant's name**
  (`SignatureVersion` 9, §4). A statement narrative writes the payee
  first and its address behind, and an MT940 `:86:` narrative leads
  with the structured-field tag the payee went into. Both are the
  bank's filing, and both vary across bookings of one merchant — a
  town in full or abbreviated, a postcode before the name or after it,
  a different field tag — so left in the key one merchant gets a
  signature per spelling and a model reads the town as part of the
  name. The key is trimmed to the narrative's head; the transfer fence
  still reads the whole narrative, so nothing a later segment says can
  slip through on the trim.
- **Only a STRUCTURED narrative is trimmed to its head** (§4). The
  field tag is what says the first segment is the payee. The export
  feed composes its description the other way round — the booking type
  leads and the merchant follows — so trimming an untagged narrative
  would file every direct debit under one key and lose the merchant
  behind it.
- **The bank's booking type is not a payee** (§4). A row the bank
  filed without one leads its narrative with the booking type, which
  the ubs adapter's promotion then reads as the counterparty and the
  signature prefers over the description. Refused at the promotion,
  the description names the merchant instead — and for the bookings
  the MT940 feed also carries, that description is the one narrative
  with a payee in it.
- **The model may never emit a delta.** Enforced by a stricter
  predicate in `canonical`, not by a hardcoded list in the command.
- **The provider tier may place a delta.** The no-deltas restriction
  guards a merchant-keyed verdict the model produces from a name; a provider verdict is per transaction, placed from the
  provider's own structured filing of the row, and where that filing
  names the movement — an ATM withdrawal, an FX conversion between the
  holder's own currency accounts, a bill paid to a card — the delta is
  the only honest verdict, and the tiers above still overrule it (§3).
- **An unmapped booking type is a rail, not drift.** A card issuer's
  vocabulary is categorical, so a value the map lacks is drift and is
  counted — bar the values the map marks `untranslatable`, the issuer's
  own residual bucket and a category naming a money movement rather than
  a line of business (§3); a bank's booking types mostly name payment
  rails, so a value
  the map lacks is the normal case and is not. The shape is a flag on
  the map, which keeps the misses canary meaningful for the issuers it
  was built for (§3).
- **A payer's memo never enters the signature** (`SignatureVersion` 4,
  §4). An adapter emits the payer's own message apart, as the change's
  memo, and the gold writer stores it after the memo separator, at the
  end of the description, folding any separator the narrative carried
  — so the payee keeps leading the narrative and the separator means
  one thing; the signature stops at it, so a payment is keyed the same
  whatever was written on it; the memo is never sent as a descriptor;
  and a signature that is nothing but the bank's booking type, which
  is what a message-led row without a caption reduces to, is refused
  at candidacy (§5).
- **Built-in rules read the narrative only; config rules read
  narrative and memo** (§3). The payer's words can name a mortgage or
  a cash machine on a row that is neither, and a built-in that read
  them would place a delta with the rule's provenance — invisibly, for
  `internal_transfer`. A config rule is the holder's own input and may
  key on what the holder wrote a payment was for.
- **A built-in may read the provider's filing to DECLINE a row, never
  to place one** (§3). The card-payment rule stands down where the
  booking type names a cash withdrawal, because cash taken at a machine
  carries the masked card number the rule reads as a bill. Placing a
  category from the filing would be the provider tier wearing the rule
  tier's provenance and outranking it; declining on it hands the row
  down to the tier that owns the filing.
- **Card-rule order is a refusal, not a re-order.** The card rule
  still leads the table — its narratives are the most specific — and
  the one row type both it and the `atm` rule match is settled by the
  refusal rather than by moving either rule.
- **The built-in rules are engine constants.** A per-deployment
  override of them would let a wrong local rule quietly break the
  arithmetic the three rules protect; a merchant needing special
  handling belongs in the merchant store, where a verdict is
  per-merchant and visible. `spending.rules` sits *below* the built-ins,
  so a local rule can add a verdict but never override one, which is
  what keeps this true.
- **Deterministic enrichment is folded into `load`**; the model tier
  is only ever explicit.
- **The matcher's window defaults to 5 days**, the same as the returns
  matcher: one matching core, one banding, so the same movement is not
  internal in one report and external in the other.
- **The spending matcher pairs legs on the same account**
  (`AllowSameOwner`); the returns matcher does not. Otherwise
  a same-day, equal-and-opposite withdrawal and deposit on one account
  — a round trip netting to zero — counts as spending. The knob
  defaults off in the shared core so returns is byte-identical; the
  false-pair surface it opens is bounded by the eligible-kind set
  (`purchase` / `refund` never reach the matcher) and by a tie-break
  that prefers a partner on another account.
- **The catch-all kinds (`other`, `journal`) are excluded from the
  population.** The UBS adapter deliberately demotes internal conduit
  legs to `other`, and `journal` is a bookkeeping entry; neither kind
  carries a reliable sign, so admitting them would import noise with no
  way to orient it. `wealthdb status -v` counts the rows excluded for
  either kind so the drift is loud rather than silently uncounted.
- **Context defaults to `merchant`**, the most private level.
- **The read CLI carries no row filters** and defaults to the trailing
  twelve months. Filtering is jq / SQL / the dashboard's job; the
  window default follows what a spending report is read for.
- **A narrow correction surface.** Three populations settle what it must
  cover, each not spending and each counted as it without a correction:
  own-money movement to destinations whose receiving side is booked
  nowhere in gold, so no matcher can ever pair it; outright securities
  purchases from a cash account, booked as plain withdrawals, for which
  the taxonomy has no honest value; and rows with no descriptor and no
  counter-leg, which nothing but a per-transaction pin can classify. The
  surface is exactly two mechanisms and no more: `spending.rules` (§3),
  keyed on narrative text for a counterparty that recurs, and the
  `spending.pins` ledger (§3, DESIGN.md §13.11), the override proper,
  per transaction. Both are config, both are re-stamped whole on every
  pass so removal removes, and both hold the holder's own identifiers —
  which is why they live in the user's config and never in the
  repository.
- **A config rule may place a consumption category.** Confining rules to
  the three delta verdicts was rejected. The fence (§5) keeps person-
  and IBAN-shaped narratives away from the model, correctly; but a
  household that pays its lawyer, contractor and tax office by wire has
  every such row fenced, and with consumption forbidden to rules those
  rows could be categorised by nothing but a per-transaction pin, one
  row at a time, for a counterparty that recurs. A rule is the holder's
  own local input and the model never sees it, so the privacy argument
  behind the restriction never applied to it; the model's output stays
  delta-free, and that is a different restriction with a different
  reason. `spending.rules` validates against `ValidSpendDetailed`, as
  the pins do.

## 10. Open follow-ups

- **`TODO(cashflow)`: the interest-versus-principal split.** Part of a
  mortgage payment — the interest — really is consumed, and only the
  principal share is the own-account move. The transaction does not
  carry the split, and deriving it needs an amortisation view the
  product has no place for yet. Recorded in `rules.go` and in the
  taxonomy header beside the `LOAN_PAYMENTS` drop.
- **Model-tier measurements.** The `categorize` loop is covered
  against a scripted client, and batching was set from live runs: a
  backlog sent whole times out, and a local model answers a batch of
  40 well inside the transport's ceiling, in the shape the gauntlet
  expects (§6, DESIGN.md §4.11). Two things have not been measured:
  whether the rolling anchor set converges the vocabulary across a
  full backlog — only samples of verdicts have been read — and
  whether 40 is tuned rather than merely safe, since no larger batch
  has been tried against the ceiling.
