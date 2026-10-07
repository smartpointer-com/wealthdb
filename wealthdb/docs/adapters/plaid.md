# plaid adapter

Adapter that projects a `plaid` silver SQLite database into the
canonical gold schema. Implements the `silver.Adapter` /
`silver.Connection` interface defined in
[../DESIGN.md](../DESIGN.md) §6.

Plaid is a data aggregator. One login at one institution is one Plaid
**Item**, and one silver database holds one Item. The collector
[`collectors/plaid`](../../../collectors/plaid) writes it, and a
deployment configures each Item's database as its own source of kind
`plaid`. Reports therefore name the Item's own source, not "plaid".

A bank, a broker and a card issuer all arrive in one shape. The adapter
decides each account's kind from Plaid's account type and subtype, and
each instrument's pair from Plaid's security type.

Link only an institution that no other collector reads into the same
gold. Gold cannot tell two copies of one account apart. Its balances
would count twice in net worth, and its ledger twice in spending, income
and cash flow.

## 1. Silver source

- Silver schema: the collector's
  [migrations](../../../collectors/plaid/migrations). The adapter tests
  keep a copy in
  [testdata/silver_schema.sql](../../internal/silver/plaid/testdata/silver_schema.sql).
  A collector test fails when the two differ in a column or an index.
- Conventions the schema fixes:
  - Money, quantities and prices are decimal strings, never REAL.
  - Timestamps are INTEGER unix seconds UTC. A date is stamped at its
    UTC midnight.
  - Both ledgers are in the fleet's sign: money into the account is
    positive. Balances keep Plaid's sign: a card or a loan states what
    is owed, positive.
  - Every snapshot a run stores carries the run's start,
    `dump_runs.snapshot_at`. Each run restates every account. Each run
    that read the holdings restates every holding.
  - `run_products` records what each run read, product by product.

## 2. Identifier conventions

Account and transaction ids pass through unchanged.

| Gold field | Source | Example |
| --- | --- | --- |
| `account_external_id` | `accounts.account_id` | Plaid's account id |
| `instrument_external_id` | CUSIP, else ISIN, else ticker, else `plaid:` + `security_id`; a mortgage's account id | `EXA` |
| `position_key` | the instrument id; a mortgage's account id | `EXA` |
| `transaction_external_id` | `transaction_id` or `investment_transaction_id` | Plaid's id |

Plaid leaves CUSIP and ISIN empty for most customers, and gives some
funds no ticker. Such an instrument is known by its ticker, else by
`plaid:` and Plaid's security id. Config `instrument_overrides` names it
by that id. The FIGI stays in the instrument's payload. Holdings and the
investment ledger key their instruments through one function, so a
trade lands on the instrument its holding made.

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `dump_runs` | none | Drives `Status` and `ChangeWindow` — see §8. |
| `run_products` | none | Decides which runs carry snapshots — see §5. |
| `item_states` | none | Plaid's update times, kept for audit. |
| `accounts` | `accounts`, `cash_balances`, `positions` | By kind — see §4 and §5. |
| `securities` | `instruments` | Through the holdings and trades that name them. |
| `holdings` | `positions`, `cash_balances` | Cash holdings are a balance. |
| `liabilities` | `cash_balances` | A card's statement closes. |
| `transactions` | `transactions` | Cash and card accounts only. |
| `investment_transactions` | `transactions` | Investment accounts only. |

**Not projected:** any loan but one on a home, and an account of type
`other`. A loan on a home is a mortgage (§4). Gold has no kind for any
other loan, such as an auto, student or personal loan or a line of
credit. As `other` its pay-down would read as a gain, and as `mortgage`
it would show as property. Left out, it is a lender gold does not
track. A payment to it from a tracked account usually places as
`debt_repayment` (§10). Plaid's catch-all `LOAN_PAYMENTS_OTHER_PAYMENT`
is left to a rule or a pin. A loan's own ledger is not booked either.
The payment is booked once, on the account it was paid from.

## 4. Account taxonomy

| Plaid type / subtype | `account_kind` | `tax_wrapper` |
| --- | --- | --- |
| depository (checking, savings, cd, money market, …) | `cash` | `taxable_personal`; `hsa` for an HSA |
| credit (credit card, paypal) | `card` | `taxable_personal` |
| loan: mortgage, home equity, home equity loan, construction | `mortgage` | `taxable_personal` |
| investment: crypto exchange, non-custodial wallet | `crypto` | `taxable_personal` |
| investment: any other subtype | `brokerage` | by subtype, below |

`home equity` is a credit line on the home, and `home equity loan` its
closed-end sibling. A `construction` loan pays for a home being built.
All four are secured by the property.

Investment wrappers by subtype:

- brokerage, cash management, mutual fund, stock plan: `taxable_personal`;
- ira: `traditional_ira`; roth: `roth_ira`;
- sep ira, sarsep: `sep_ira`; simple ira: `simple_ira`;
- 401k, 401a, roth 401k, keogh, and profit sharing and thrift savings
  plans with their Roth forms: `401k`;
- 403B and roth 403B: `403b`; 457b and roth 457b: `457b`;
- 529: `529`; education savings account: `coverdell_esa`; hsa: `hsa`;
- ugma, utma: `custodial_ugma`, `custodial_utma`.

Any other subtype leaves the wrapper unset, never `other`. An unset
wrapper reads as household, as `other` does, but the load and
`status -v` report it, and config `account_overrides` fills it in.
`trust` is one of them: it covers revocable trusts, which are the
holder's own, and irrevocable ones, which are not.

The wrapper follows the subtype as Plaid reports it. Where that gives
the wrong wrapper, config `account_overrides` sets the right one.

`management_style` is `self_directed` on cash, card and mortgage
accounts, and unset on an investment account, where Plaid does not say
who trades.

Names:

- The display name is the institution's official name for the account,
  else the account's own name, followed by its mask: `Official Savings
  …0000`.
- The nickname is the account's own name, set by the holder or the
  institution, where an official name stands beside it.
- `account_category` is Plaid's `type / subtype`, followed by the
  official name: `depository / savings · Official Savings`.

## 5. Positions and balances

Each run is one snapshot instant: the run's start. A run carries
snapshots only when it read the accounts and settled the holdings
(read in full, or nothing to read). A run whose holdings failed adds
nothing at its own instant. Its balances alone, newer than the last
positions, would hide every position from the current view and zero
nothing.

| Account kind | Fact at the run's instant |
| --- | --- |
| `cash` | a `current` balance: Plaid's balance as it stands; an `available` one where the institution states only that |
| `card` | a `current` balance: Plaid's balance negated, so what is owed is negative cash |
| `mortgage` | a `(real_estate, mortgage)` position keyed by the account: the balance negated |
| `brokerage`, `crypto` | a position per instrument held, and a `current` balance per currency for the cash among the holdings |

Plaid's balance of an investment account is its total value, so it is
never read as cash. An Item linked without investments still lists its
investment accounts. They carry no value in gold, because only holdings
say what such an account holds.

- **Positions.** `market_value` is Plaid's `institution_value`,
  `book_value` its `cost_basis` (a total). Two holdings of one
  instrument in one account are one position, summed; its payload
  lists both. The book value is left out when a holding states no
  cost. Tax lots stay in the payload, and `acquisition_date` is unset.
- **Vesting.** Shares not yet vested are not the holder's. Where Plaid
  states a vested quantity below the whole, the position holds the
  vested quantity. Its value is Plaid's vested value, else the price
  times the vested quantity, else the whole value pro rata. The payload
  notes the `unvested_quantity`. Plaid states the cost of the whole
  holding only, so the book value is that cost pro rata to the vested
  quantity.
- **Cash.** A security is cash when its type is `cash` and it has no
  ticker, a currency code as its ticker, or a `CUR:` ticker. A
  cash-type security with a ticker of its own is a money market fund,
  and stays a `(cash, fund)` position. The payload of a balance read off
  the holdings says `"basis": "holdings"`. One read off Plaid's account
  list says `"basis": "roster"`.
- **Cash that has a price.** Cash is worth one per unit. Plaid can type
  a bond as cash, with no ticker. Such a security is not cash when a
  holding of it, or a buy or sell of it, states a price other than 0
  or 1. It is then a position, and its type says nothing about its
  class (§6). Any other row's price is a placeholder, and does not
  count.
- **A balance Plaid leaves empty.** Gold reads a source's current state
  from its latest instant, so every run restates every account. A run
  may list a cash, card or mortgage account with no balance, or with no
  currency. The last figure the account stated is then carried into the
  run, in its own currency. That figure may come from a run whose
  holdings failed. The payload says `"basis": "carried"` and the run it
  was stated at.
- **Closure.** An account that held positions at the last run and
  holds none now gets its last positions replayed at zero
  (`silver.ClosureMarkerBatch`). It left the Item, or sold out. Without
  the zero, gold's history would carry the positions forward. Cash
  closes the same way, key by key. A balance the last run stated and
  this run does not reads zero once, with `closure_marker` in the
  payload. That is an account Plaid no longer lists, or cash an
  investment account no longer holds. Plaid lists no line for either.
- **Card statements.** Each statement's closing balance is a `closing`
  balance at its issue date, negated. Every run restates the last
  statement, and the newest run that states a figure stands. Only a
  statement issued before the newest run that carries snapshots counts.
  A later one would be the source's latest instant on its own, and hide
  every other balance. It counts once a later run carries snapshots. A
  card whose newest listing names no currency closes in the currency of
  its last stated balance.

## 6. Instruments

| Plaid security type | `(asset_class, vehicle)` |
| --- | --- |
| equity | `(public_equity, stock)` |
| etf | `(RefineETFExposure(name), etf)` |
| mutual fund | `(cash, fund)` for a money market name, else `(RefineETFExposure(name), fund)` |
| cash, with a ticker of its own | `(cash, fund)` |
| fixed income | `(fixed_income, bond)` |
| derivative | `(public_equity, option)` |
| cryptocurrency | `(crypto, physical)` |
| loan | `(private_debt, loan)`: a loan the holder owns |
| cash, with no ticker and a price | by the CFI code, below; else `(other, other)` |
| other, or anything else | by the CFI code, below; else `(other, other)` |

Plaid marks bitcoin as a cash equivalent. It is still crypto.

A security of type `other`, or of a type no row above names, is read by
its CFI code (ISO 10962). Category C is a collective investment vehicle.
In group E it is `(RefineETFExposure(name), etf)`. In any other group
it is classed as a mutual fund. A security classed by its CFI code, or
not at all, keeps Plaid's type as `source_type` in the payload. Config
`instrument_overrides` states the class of one that lands wrong.

## 7. Transactions

Both ledgers go out in one batch, ordered by date and id. `occurred_at`
is the posting date, except on a trade. Plaid dates a trade at its
settlement and states its trade date apart, as a time:

- A buy or a sell books at the UTC day of its trade date, where Plaid
  states one no later than the posting date. Plaid's own `date` stays in
  the payload.
- No trade books before the oldest date the investment ledger was read
  from. A reload then always reaches it (§8).

**The bank and card ledger** (cash and card accounts). Silver holds it
in the fleet's sign, and it still goes through `ApplyCanonicalSign`.

| Account | Row | Kind |
| --- | --- | --- |
| cash | `INCOME_INTEREST_EARNED`, or a debit `BANK_FEES_INTEREST_CHARGE` | `interest` |
| cash | another debit under `BANK_FEES`, or under Plaid's older category "Bank Fees" | `fee` |
| cash | any other debit / credit | `withdrawal` / `deposit` |
| card | a debit: `BANK_FEES_INTEREST_CHARGE` / other `BANK_FEES` or older "Bank Fees" / anything else | `interest` / `fee` / `purchase` |
| card | a credit under `LOAN_PAYMENTS`, `TRANSFER_IN` or `LOAN_DISBURSEMENTS`, or Plaid's older category "Payment > Credit Card" | `card_payment` |
| card | any other credit | `refund` |

A card payment can arrive filed as a loan disbursement. A credit on a
card cannot be a loan disbursement, since a card lends with a debit.
Plaid still sends its older category beside the newer one. The older
one can name a payment or a fee where the newer one does not.

A cash account's row is never a transfer kind, in either direction. A
transfer kind would leave the spending and the income population.
`description` is the institution's own text, else Plaid's name.
`counterparty` is Plaid's merchant name, verbatim: it is the input to
the merchant signature. `provider_category` is Plaid's detailed
category, verbatim. A cheque number is kept on an outflow only. A
pending row goes out like any other, with Plaid's `pending` in the
payload. When the charge posts under a new id, the next read of the
ledger drops the pending row from silver.

**The investment ledger** (investment accounts). Each row keeps its own
sign: a row that disagrees with its kind is a correction, and forcing
the sign would book it twice.

| Plaid subtype | Kind |
| --- | --- |
| buy, buy to cover, any reinvestment | `buy` |
| sell, sell short | `sell` |
| dividend, qualified and non-qualified dividend | `dividend` |
| interest, interest receivable, margin expense | `interest` |
| long- and short-term capital gain, unqualified gain | `capital_gain` |
| the fee subtypes, and an adjustment of type `fee` | `fee` |
| tax, tax withheld, non-resident tax | `tax` |
| deposit, contribution, withdrawal, distribution, transfer, send, request, pending credit and debit | cash: `deposit` / `withdrawal` by sign; a security: `transfer_in` / `transfer_out` |
| exercise, assignment | the side of a trade under `buy` or `sell`; else `corporate_action` |
| merger, spin off, split, stock distribution, expire, return of principal, any other adjustment | `corporate_action` |
| trade: one cryptocurrency for another | `other` |
| a `cancel` row | the kind of the row it cancels, and that row's amount negated; `other` when silver does not hold that row |
| anything else | `buy`, `sell` or `fee` by Plaid's type, else `other` |

- A row of Plaid type `cash` moves the account's cash, whatever
  security it names. A row on the cash security (a contribution,
  interest) names no instrument.
- A row of type `cash` or `fee` states no quantity or price. Plaid's
  figures there are placeholders.
- A cash deposit or withdrawal can be a coupon, a fund distribution,
  a fee or a tax. Then only its text says so, and the row keeps its
  amount and the security it names. A text that names a security puts
  the action after ` - `, and only the action is read. The first of
  these that the action names decides:
  - interest: `INTEREST`, or `INT` as a word of its own;
  - a capital gain: `CAP GAIN` or `CAPITAL GAIN`, singular or plural;
  - a fee: `FEE` or `FEES`;
  - a tax: `TAX`, `TAXES` or `TAXPYMT`.
- Otherwise a row of type `cash` or `fee` that names a security that is
  not cash is read by what it names:
  - an adjustment of type `fee`, or a cash withdrawal, is `tax` where
    the security paid a dividend into the account on the same posting
    date, else a `fee`;
  - a cash deposit is the cash paid in lieu of a fractional share: a
    `sell` of the fraction, with no quantity.
- A movement of a security is valued at the securities' worth: the
  row's amount, else its quantity times its price.
- A movement at no worth is a `corporate_action` in two cases. The
  security is a derivative, such as an option that expired. Or the
  security has a corporate action in the account that day, such as the
  other leg of a merger. Any other is marked `"unvalued": true` in the
  payload. Gold cannot value it.
- A trade's gross amount is its net amount before Plaid's fees.
- A cancel nets against the row it cancels, inside one kind. A
  cancelled deposit is a negative `deposit`.
- Nothing maps to `contribution`, which is a capital call.
- A row of kind `other`, and a row kinded by Plaid's type alone, keep
  Plaid's type and subtype in `payload.source_kind`. Kinding by type is
  a departure from DESIGN.md §6.8. `status -v` counts a row kinded by
  type as `kind guessed`, and a row of kind `other` under
  `kind='other'`.
- `provider_category` is unset on this ledger: Plaid files no category
  here.

Dimensions travel only on the snapshot stream. The accounts and
instruments the window's transactions name go out on the last snapshot
batch, seen over the dates those transactions book at.

## 8. Change number and the window

The shared load clock (`silver.LoadClockStatus`): a loaded run is the
trigger, and a full re-emit is the response.

- The change number is `MAX(dump_runs.snapshot_at)`.
- The window spans every date the projection touches: the runs, both
  ledgers' posting dates, the card statements' issue dates, and the
  start of every ledger window a run read. Silver deletes the rows Plaid
  stopped listing, and such a row can be the oldest one. Every row
  silver held was stored by a run whose window reached its date, so
  with every window in the span, a reload never leaves it standing in
  gold. A trade books no earlier than the oldest window start, so the
  span covers it too.
- `Status` reports the transaction range over both ledgers, at the dates
  their rows book at. An Item linked for investments alone has no bank
  ledger.

## 9. Returns policy

`DefaultReturnsPolicy(BankFlowPolicy())`, as for the synthetic kind:
flow_complete on the shared bank sets, every other knob at its default.
One Item may be a bank, a broker or a card issuer. The engine already
treats each account by its kind:

- a card never enters returns;
- a cash account shows no row of its own;
- a mortgage goes to the liability line.

A source-wide hidden accounts grain would also hide a broker's
investment accounts. A deployment states an Item's own regime with
config `returns_policy_overrides` (DESIGN.md §5.6).

## 10. Provider vocabulary

`plaid` is a categorical vocabulary of Plaid's personal finance
categories, version 2, under one key for every account kind.

- **The reviewed list** is all 127 version 2 values. Each side
  translates a value or leaves it untranslated. Only a value outside
  the list is drift. A version 1 spelling that version 2 renamed counts
  as drift too: the collector asks for version 2.
- **Identity.** The taxonomy vendors version 2 (SPENDING.md §2), so
  each of its 95 vendored values translates to itself. The 82 spending
  values do so on the spending side, and the 13 income values on the
  income side.
- **Loans.** Twelve loan values translate to a delta (SPENDING.md §2):
  - a card bill, `LOAN_PAYMENTS_CREDIT_CARD_PAYMENT`, is `card_spend`;
  - a mortgage instalment is `mortgage_transfer`;
  - a student, personal, cash-advance or car payment is
    `debt_repayment`;
  - an auto, cash-advance, personal, student or other loan disbursed
    is `loan_proceeds`;
  - a mortgage tranche, `LOAN_DISBURSEMENTS_MORTGAGE`, is
    `mortgage_transfer`.
- **A car payment can be a lease.** Plaid files loans and leases under
  one value. A config rule on the lessor's name, scoped to the paying
  account, puts a lease back in the spending base.
- **Fees.** Late fees and cash-advance fees have values of their own,
  so a filing under either claims the row. The built-in cash rule
  stands down on a row filed under any bank fee, so a cash-advance fee
  or an ATM fee stays a fee (SPENDING.md §3).
- **Pensions.** Plaid files a payout from a plan such as a 401(k)
  under `INCOME_RETIREMENT_PENSION`, beside pensions. The provider tier
  places it as pension income. A payout from a plan whose balance is
  the holder's own is `retirement_transfer` (INCOME.md §2). A config
  rule on the plan's name places it there.
- **Left untranslated:**
  - the transfers, whose far side is the matcher's to find;
  - buy-now-pay-later instalments;
  - `LOAN_PAYMENTS_OTHER_PAYMENT`, which can be a card bill or a loan
    payment;
  - a wage advance, `LOAN_DISBURSEMENTS_EWA`, and its repayment,
    `LOAN_PAYMENTS_EWA`. An advance is usually wages paid early and
    taken back from the next pay, and sometimes a loan. It stays in the
    income base as a visible receipt, and its repayment in the spending
    base;
  - `OTHER_OTHER`, and each side's values for the other direction.
- **Catch-alls.** A catch-all is recorded and declined, so the model
  reads the merchant or the payer name. `INCOME_OTHER` is the income
  side's one catch-all.
- The investment ledger carries no category.
- **The card rule.** The built-in card rule stands down where this
  vocabulary translates the row to something other than a card bill
  (SPENDING.md §3).

## 11. Open questions

- **Account ids.** Whether Plaid keeps an account's id from one run to
  the next is open (collector DESIGN.md §8). A new id would read as one
  account closed and another opened.
- **Cancel rows.** Which institutions send a `cancel` row, and for what,
  is open.
- **Mortgage instalments.** A payment to the Item's own mortgage is not
  linked to that mortgage. Plaid states no counter account, and the
  loan's own ledger is not booked. Cash flow therefore draws each
  instalment whole as `Mortgage interest`. Gold does hold the balance
  series that the interest and principal split reads.
