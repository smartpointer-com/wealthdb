# UBS adapter

The adapter projects two UBS silver SQLite databases into the
canonical gold schema:

- `ubs-psn`: the daily SDFI and SWIFT MT report feed, delivered over
  SFTP.
- `ubs-web`: the netbanking scrape. It holds live snapshots, the
  account-statement export, statements reconstructed from the PDF
  archive, cards, mortgages and the portfolio transaction list.

It implements the `silver.Adapter` / `silver.Connection` interface of
[../DESIGN.md](../DESIGN.md) §6. With both subsources configured, the
orchestrator (`merge.go`) splices them:

- Statements reconstructed from PDFs fill the dates before PSN starts
  (§9).
- The live web feed emits dimensions before PSN starts.
- PSN emits dimensions and facts from its start on.
- Cards, mortgages and the portfolio transaction list come from the web
  feed on every day. PSN carries none of them.
- The transaction stream emits each booking once across the feeds
  (§7).

## 1. Silver sources

- Upstream feeds:
  - PSN: [`ubs-psn`](../../../collectors/ubs-psn/).
    Silver schema: the collector's [migrations](../../../collectors/ubs-psn/migrations).
  - Web: [`ubs-web`](../../../collectors/ubs-web/).
    Silver schema: the collector's [migrations](../../../collectors/ubs-web/migrations).

## 2. Identifier conventions

| Field | Source | Notes |
| --- | --- | --- |
| `relationship_id` | UBS server id (`SFTPCHxx`, `SFTPCHyy`, ...) | One per banking relationship under the SFTP login. Stored on the gold `accounts` row. The config's `relationships` pairs it with the web feed's `banking_relationship_id` ([../DESIGN.md](../DESIGN.md) §5.1). |
| `account_external_id` | IBAN for a cash account; PSN's safekeeping id for a safekeeping account | A portfolio's overlay account is `<portfolio>:overlay` (§3, §9). |
| `instrument_external_id` | ISIN | SDFI states one for every instrument. |
| `transaction_external_id` | the silver row's id | PSN: `mt515:<SEME>` for a trade confirmation, `mt940:<account>:…` for a statement line. Web: `<Transaction no.>@<account>` for an export row, `stmt:<hash>` for a statement reconstruction, `ptx:<ref>` for the portfolio transaction list. Rows the adapter derives: `mirror:<event id>` and `<event id>:cash` (§7). |

## 3. Coverage matrix

PSN silver:

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `schema_meta` / `dump_runs` | meta only | Read by `Status` and `ChangeWindow` (§8). |
| `account_holders` | — | Not an account. The holders' names tell a conversion between own accounts from a payment abroad (§7). |
| `cash_accounts` | `accounts` (kind `cash`) | IBAN as `account_external_id`. |
| `cash_accounts.payload.BookBalAmt` | `cash_balances` (kind `closing`) | See below. |
| `safekeeping_accounts` | `accounts` (kind `safekeeping`) | PSN's safekeeping id as `account_external_id`. |
| `portfolios` | `portfolios` | `PrtflId` as `portfolio_external_id`. |
| `instruments` | `instruments` | §4 derives the `(asset_class, vehicle)` pair. |
| `holdings` | `positions` | One securities holding per row. The book value is the MT535 BOOK amount (§12). |
| `cash_balances` | `cash_balances` | One to one; UBS `balance_kind` carries over. From the MT940 statement feed. |
| `fx_rates` | `fx_rates` | Base and quote currency as silver states them. The mid rate is the first period's `MiddleRate`. |
| `forward_contracts` | `positions` `(foreign_exchange, forward)` | On the portfolio's overlay account. `contract_external_id` is the `position_key`. |
| `events` | `transactions` | §5 maps the kinds. §7 adds two derived rows. |
| `option_contracts`, `money_market_contracts`, `otc_contracts` | — | Not projected. |
| `pending_securities` | — | Mostly "no activity" markers (`ACTI//N`). Not projected. |
| `portfolio_performance` | — | Monthly TDPOPF analytics. Not projected. |
| `cash_account_pricing` | — | TDCAPI interest-rate configuration. Not projected. |

Web silver:

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `accounts`, `portfolios`, `positions` | `accounts`, `portfolios`, `instruments` | Accounts and portfolios before PSN starts. The web feed's instrument descriptions read better than PSN's names; the latest snapshot's wins. Live positions reach gold as `payload.web` on PSN's rows. |
| `transactions` | `transactions` | §5, §7. |
| `mortgages`, `historical_mortgages` | `accounts` (kind `mortgage`) + a negative `mortgage` position | No PSN cut: PSN carries no mortgages. |
| `historical_position_snapshots`, `historical_cash_balances` | `positions`, `cash_balances` | §9. |
| `card_accounts`, `card_invoices`, `card_transactions` | `accounts`, `cash_balances`, `transactions` | §10. |
| `portfolio_transactions` | `transactions`; `realized_lots` | A managed portfolio's securities settlements, booked on the cash account that paid (§11). Its sales with a realized P/L are also realized lots (§12). |
| `statement_trades` | `realized_lots` | The trade list a Statement of Assets prints (ubs-web DESIGN.md §3.10). Each sale is a realized lot with the cost it drew on (§12). |
| `advices` | `positions.book_value` | The capital calls are the paid-in basis of a private-markets fund's units (§12). Contract notes are not read. |

**The MT940 feed is delivered per account.** An account is in scope
for statements or it is not, and one that is not receives none on any
day. §7 describes what follows for transactions.

**The book balance fills the rest.** The account master data states a
daily book balance for every cash account (`appendBookBalances`).

- It is the closing balance. Wherever the two feeds overlap it equals
  the MT940 closing figure to the cent. It matches neither the opening
  nor the available one.
- It is signed at the source, so no credit/debit indicator applies.
- An (account, currency, day) the statement feed already emitted is
  skipped, so the pass only adds.

## 4. `(asset_class, vehicle)` derivation for `holdings`

`silver.holdings` has no taxonomy column. The adapter looks up the
holding's ISIN in `silver.instruments` and reads the ISO 10962 CFI code
(`InstrCtgyCFI` in the SDFI payload). Where the CFI is empty, it falls
back to UBS's internal `UacAsstClsCd` bucket: the non-listed custody
items (metal-deposit receipts, private-market fund interests) carry no
CFI but do carry a UAC code. `taxonomyPairForInstrument` derives the
pair ([../TAXONOMY.md](../TAXONOMY.md)):

- the CFI's first character picks the vehicle;
- the exposure comes from the CFI, the `UacAsstClsCd` bucket and the
  instrument name together.

| CFI category (first char) | Gold `(asset_class, vehicle)` |
| --- | --- |
| `E` Equities | `(public_equity, stock)`. Group `EY` (tracker and actively managed certificates) is `(public_equity, structured_product)`, or `(foreign_exchange, structured_product)` when the name reads currency-linked (`currencyLinkedRe`: "currency", "FX", "forex", "dual currency"). |
| `C` Collective investment | Vehicle `etf` for group `CE` (ISO 10962:2015 ETFs), else `fund`. The exposure is `silver.RefineETFExposure(name)`: crypto → `crypto`, bullion → `metal`, bond keywords → `fixed_income`, else `public_equity`. A fund's `UacAsstClsCd` overrides the name read: `0100` (Liquidity) → `(cash, fund)`; `0400` (Hedge funds & private markets) → `privateMarketsPair` ("infrastructure" → `(infrastructure, fund)`, "hedge" → `(hedge_fund, fund)`, else `(private_equity, fund)`). |
| `D` Debt | `(fixed_income, bond)` |
| `R` Entitlements (rights) | `(public_equity, right)` |
| `O` Listed / `H` non-listed and complex options | `(public_equity, option)` |
| `F` Futures | `(public_equity, future)` |
| `J` Forwards | `(foreign_exchange, forward)`. The forward wrapper pairs only with FX in the taxonomy. |
| `S` swaps · `I` spot · `K` strategies · `L` financing · `T` referential · `M` others · unrecognised | `(other, other)`. These are not custody holdings. `T` is reference data (UBS's per-currency reference rows carry `TC…` codes). |
| empty CFI | The `UacAsstClsCd` fallback (`taxonomyPairForUAC`): `0100` → `(cash, fund)`, `0300` (Equities) → `(public_equity, stock)`, `0400` → `privateMarketsPair`, `0600` (Precious metals & commodities) → `(metal, physical)`, anything else → `(other, other)`. |

### Latest-known-instruments lookup

The UBS silver loader writes an instruments row only when the SDFI
payload changes. So most snapshots carry no instruments row for a given
ISIN, and a same-snapshot lookup would miss. The adapter builds one
`map[isin]meta` from the most recent instrument row in the whole silver
and uses it for every holding. Instrument metadata (name, CFI category)
does not change in practice, so one snapshot of staleness is harmless.

### MT535 SWIFT-tag parsing

Each `holdings.payload` carries the raw MT535 fields under
`payload.fields.{"19A","93B","35B",...}`. Each tag is an array of raw
SWIFT subfield strings. `mt535.go` decodes two of them:

- **`93B`**, the quantity: `:<qualifier>//<format>/<value>`.
  - The qualifier is `AGGR` (aggregate), `AVAI` (available), `NAVL`
    (not available), `AWAS` (awaiting settlement) and others.
  - The format is `UNIT` (shares, contracts) or `FAMT` (face amount,
    for bonds).
  - The adapter takes `AGGR`, else `AVAI`.
- **`19A`**, a money amount: `:<qualifier>//<CCY><value>`.
  - The qualifier is `HOLD` (market value), `BOOK` (book cost), `ACRU`
    (accrued interest) and others.
  - A holding usually states `HOLD` twice: in the position's currency
    and in the relationship's reference currency (CHF).
  - The adapter takes the `HOLD` entry in the instrument's currency,
    else the first `HOLD` entry.

A SWIFT amount uses a comma as the decimal separator. A trailing comma
ends the amount: `1500000,` is `1500000` and `150,123456` is
`150.123456` (`parseSwiftDecimal`).

A holding whose fields are empty or malformed leaves those gold columns
NULL. One bad payload does not fail the load.

## 5. `events.kind` mapping

UBS's `silver.events.kind` maps to gold's `kind`:

| UBS | Gold | Adapter notes |
| --- | --- | --- |
| `cash_movement` | `deposit` / `withdrawal` / `fee` / `interest` / `tax` / `dividend` / `buy` / `sell` / `fx` | From the MT940 `:86:` narrative, with the `:61:` type code as a floor (§6). A deposit or withdrawal whose same-day mirror books on another own account carries the conduit verdict in its payload (same-day offset veto, `buildSameDayOffsetVeto`). A converted movement (`/OCMT/`) whose other account the feed does not speak for gets a counter-leg, `mirror:<event id>` (§7). |
| `securities_movement` | `transfer_in` / `transfer_out` | By the credit/debit mark. |
| `trade_confirmation` | `buy` or `sell` | MT515 payload `side`, which the collector reads off the order's business function (`:22H::BUSE//`). A market trade names the holder's role (`BUYI` / `SELL`, folded to `BUY` / `SELL` at load). A fund order names the operation (`SUBS` subscribes, `REDM` redeems, kept verbatim). Dated to the trade day, not the settlement day. Booked against the settlement's cash account, resolved to its IBAN (`cashAccountIBANs`). The MT940 line for the same settlement folds away (§7, *The settlement fold*). |
| `fx_confirmation` | `fx` | MT300. |
| `fx_option_confirmation` | `fx` | MT305. No dedicated option kind: the settlement is an FX cash effect. |
| `loan_deposit_confirmation` | `other` | MT320/MT330/MT350. |
| `corporate_action_notification` | `corporate_action` | MT564. |
| `corporate_action_confirmation` | `corporate_action` | MT566. A cash dividend (`DVCA`) or interest (`INTR`) paid into a cash account the MT940 feed does not speak for is also booked on that account as `<event id>:cash` (§7). |
| `corporate_action_narrative` | `corporate_action` | MT568. |
| `securities_settlement_advice` | `transfer_in` / `transfer_out` | MT544–548. |
| `precious_metal_trade` | `buy` or `sell` | MT600/MT601. A credit is a sale, anything else a purchase. |
| `charges_advice` | `fee` | MT590/MT990. |
| `debit_credit_confirmation` | `deposit` / `withdrawal` | MT900/MT910. |

### `ubs-web` booking types

`webKind` (`web_reader.go`) classifies the web silver's
`description_kind`. That is the statement PDF's printed booking type,
or the export's `Description2` label.

- It matches the whole type, case-insensitive, so the two eras'
  spellings (`CREDIT` / `credit`) land together.
- A `<base>;Reversal` suffix is stripped, and the row takes the kind of
  the booking it reverses.
- On an export row whose booking-type column is empty or holds a
  reference, the first segment of the promoted counterparty stands in
  (`webKindHint`). Such rows carry the product there.

| Booking type (any case) | Gold `kind` |
| --- | --- |
| Income and cost types (`DIVIDEND`, `COUPON`, `INTEREST CALCULATION BALANCE`, `CUSTODY PRICE`, `UBS ADVICE`, …) | `dividend` / `coupon` / `interest` / `fee` |
| `UBS MANAGE`, `CAN UBS MANAGE`, `REC UBS MANAGE` | `fee` |
| FX types (`FOREX SALE`, `PURCHASE FX SPOT`, `SALE FX FORWARD`, `SALE FROM FX SWAP`, …) | `fx` / `fx_forward` / `fx_swap` |
| `BUY`, `SECURITIES PURCHASE` / `SELL`, `SECURITIES SALE` | `buy` / `sell` |
| Securities settlements (`SHARE`, `MUTUAL FUNDS`, `STRUCTURED PRODUCTS`, `CAPITAL CALL`, `CASH DISTRIBUTION`, …) | `buy` / `sell` by cash direction |
| `PAYMENT UBS TWINT`, `DEBIT UBS TWINT` | `withdrawal` |
| `CREDIT UBS TWINT`, `REVERSAL UBS TWINT` | `deposit` |
| (other, `ORDER` among them) | `deposit` / `withdrawal` by the column the figure sits in; `other` when neither or both are set |

Two types need a note:

- **`UBS MANAGE`** is the discretionary mandate's periodic management
  charge. The bank bills it to the mandate's cash account at each period
  end. Like the other charges it names the product, with no instrument,
  quantity or price. `CAN UBS MANAGE` cancels a charge and `REC UBS
  MANAGE` re-bills the corrected figure. The cancellation's inflow
  survives because the statement era prints it as a negative in the
  debit column, which `webReversal` reads.
- **`ORDER`** is a payment order, not a securities order. Its rows have
  no quantity, no price and no instrument, and the narrative names the
  party paid. It often pays another account of the same relationship,
  possibly in another currency, whose side books a plain `CREDIT`. It
  is the statement era's form of the export's `payment order` /
  `e-banking payment order` / `special payment order`, and it takes the
  direction as those do.

The kind follows the type, not the column. The silver row carries an
unsigned figure in a debit or a credit column. A trailing-minus figure
on a statement stays negative. `ApplyCanonicalSign` orients the net
amount by the kind, so a reversal keeps its inflow whichever column
printed it.

A statement-era deposit or withdrawal then passes the era-gated
external-flow test (`pdfCashIsExternal`):

- In the MT940 era, the TWINT credit and reversal count as arrivals,
  beside `CREDIT`, `E-BANKING CREDIT` and `SALARY PAYMENT`. The TWINT
  payment and debit count as outbound rail payments.
- In the deep era, both directions stay internal, as every rail booking
  does there.

## 6. MT940 `:86:` narrative parsing

A `cash_movement` event (an MT940 `:61:` line) carries a free-text
narrative in the `:86:` continuation. The adapter reads three things,
in order:

1. **The narrative.** It decides wherever it says something a reader
   can place. It is the only witness that tells a transaction tax from a
   custody price.
2. **The `:61:` type code**, a floor read only where the narrative says
   nothing placeable. An entry the bank wrote no narrative for arrives
   as a bare booking code. Without the floor, the cash leg of a trade
   settling that way would read as a plain withdrawal: spending would
   count it as money leaving, and returns as external capital.
3. **The direction**: `deposit` or `withdrawal`. The raw narrative stays
   in the payload.

| Narrative | Gold `kind` |
| --- | --- |
| contains `TIMBRE`, `UMSATZABGABE`, `STEMPEL` or `STAMP` anywhere | `tax` |
| first word `INT` / `INTERESTS` / `INTERETS` / `ZINSEN` | `interest` |
| first word `COMM` / `COMMISSION` / `FRAIS` / `GEBUEHREN` / `FEE` / `FEES` | `fee` |
| first word `IMP` / `IMPOT` / `STEUER` / `TAX` | `tax` |
| first word `DIV` / `DIVIDEND` / `DIVIDENDE` | `dividend` |

| `:61:` type code | Gold `kind` |
| --- | --- |
| `NSEC` | `buy` (debit) or `sell` (credit) |
| `NFEX` | `fx` |
| `NDIV` | `dividend` |
| `NINT` | `interest` |
| `NCHG`, `NCOM` | `fee` |
| `NTAX` | `tax` |

- `NRTI` is a returned item, not interest, and is not mapped.
- A reversal, marked `RC` or `RD` in the credit/debit field, skips the
  floor. Its sign is not yet flipped there, so a kind read off it would
  state the wrong direction.
- An `NSEC` line whose trade the MT515 rail also confirms never reaches
  the floor. The settlement fold (§7) drops it in favour of the
  confirmation, which names the security.

## 7. Transactions across the feeds

Three eras record the cash ledger:

| Era | Silver rows | Ids |
| --- | --- | --- |
| Statement reconstruction | `ubs-web.transactions`, `payload.source = "account_statement_pdf"` | `stmt:`, a content hash of the printed row |
| Account-statement export | `ubs-web.transactions`, no `payload.source` | UBS's "Transaction no." |
| PSN | `ubs-psn.events` | `mt940:`, `mt515:`, … |

Their time ranges overlap and their id spaces are disjoint. The adapter
emits each booking once:

- **The hard cut** gives a web row on or after PSN start to PSN, per
  account (*The accounts the feed does not speak for*).
- **The seam** drops the web copy of a booking the first PSN dump
  reaches back over.
- **The era fold** drops a statement copy that the export or the feed
  also holds.
- **The settlement fold** drops the MT940 cash leg of a trade an MT515
  confirmation states.

The folds remove rows; they never change an amount, a date, a kind or
an id. Text moves only downward: a richer copy fills a column the
surviving row left empty or bare.

### Transaction text columns

Gold's `transactions.description`, `counterparty` and
`provider_category` (gold migration 0038) are a projection of text
only. Each emitter settles ids, dates, accounts, instruments, kinds,
signs and amounts first, and never reads the text it then attaches. So
the text cannot move a row's kind or amount (`text_columns_test.go`
pins the other columns byte for byte).

**Export rows** (`Description1/2/3` in the payload):

- `counterparty`: silver's `counterparty` column, the first
  `;`-segment of `Description1`. That is the payee, or the security
  caption on a securities row. Two exceptions:
  - A booking type the bank filed without a payee is refused.
  - On a charge the bank bills for itself, the counterparty is the
    bank. These are custody, advice, safe box, the service-price close,
    an interest calculation, and the mandate management charge with the
    pair that corrects it. The booking type names the charge. Where that
    column is empty or holds a reference, the narrative's first segment
    does. A depositary's pass-through and a third-party charge keep the
    promoted text.
- `provider_category`: the booking type, `description_kind`
  (`Description2`: `Dividend`, `e-banking payment order`, …). A
  `;Reversal` suffix stays.
  - A message typed on the order comes before the type
    (`THANKS; e-banking payment order`). So the column splits at its
    last `; `, and the trailing part is the type.
  - A card-booked entry puts the card's number and expiry in that slot
    (`<number>-<check> MM/YY; ATM Withdrawal`). It is recognised whole
    and dropped (§10.7).
- `description`: the `Description1` caption without its ISIN tail.
  Gold's name lookups key on it. Without a caption: the booking type,
  then `Description3`.

**Statement rows** (`payload.source = "account_statement_pdf"`):

- `counterparty`: silver's `counterparty` column, the first
  continuation line. None when that line is the turnover-total line
  (below). The bank on the booking types it bills for itself.
- `provider_category`: `description_kind` verbatim, the printed booking
  type (`E-BANKING PAYMENT ORDER`, `FEES`, …).
- `description`: the booking type, then every `payload.continuation`
  line in order, without the turnover-total line.

**PSN rows:**

- MT940 `cash_movement`:
  - `counterparty`: the bank when the `:61:` type is `NCHG` or `NCOM`
    (a charge it levies for itself), never on a reversal. Otherwise
    none: the `:86:` narrative has no structured payee, and none is
    parsed from the free text.
  - `provider_category`: `payload.txn_type` verbatim, the `:61:` type
    code (`NTRF`, `NMSC`, …).
  - `description`: the `:86:` narrative (`payload.narrative`), line by
    line.
- MT515 `trade_confirmation`: `description` is
  `payload.security_name`.
- MT564/566/568 `corporate_action_*`: `description` is `payload.caev`,
  the ISO 15022 event code.

**Composition.** `silver.JoinText` joins the parts in the order listed.
Each part is trimmed, empty parts drop, and the rest join with `"; "`,
the separator UBS puts between the lines of a `Description1`. A row
whose only text is a bare booking code reaches gold as that code. A row
with no text stays NULL. Nothing is inferred or paraphrased.

**The memo.** A payer's message is not a part. The adapter emits it as
the change's `Memo`. The gold writer stores it after the composed
narrative, behind `canonical.DescriptionMemoSeparator` (a spaced em
dash). So the payee leads, and the merchant signature stops at the
separator ([../SPENDING.md](../SPENDING.md) §4): a payment keys the
same with or without a message. A reference-led order
(`<reference>; order`) splits the same way: the reference is the memo
and `order` the type. Such rows without a caption all share the key
`ORDER`, which candidacy refuses as the bank's own filing
([../SPENDING.md](../SPENDING.md) §5).

**The statement's period summary.** Before its closing balance an
Account Statement prints `Turnover total <debits> <credits>`. The line
has no date, so the collector's parser attaches it to the booking
before it. At a period close that is the service-price or interest
line (`BALANCE CLOSING OF SERVICE PRICES`, `INTEREST CALCULATION
BALANCE`), which reaches silver with a figure of `0.00` and the totals
as its only narrative.

- The adapter drops a row with an amount of exactly zero and no
  narrative but the turnover-total line (`isStatementSummary`).
- The export prints the same marker as a zero-amount `Balance closing
  of service prices` row without a booking type. It is dropped the same
  way (`isServicePriceClose`).
- The adapter logs both counts. The adapter interface carries no
  per-source diagnostics.
- With a non-zero figure the same booking types are a real fee or
  interest booking, and the row stays. Its description is the booking
  type alone and its counterparty is the bank. The provider tier places
  it from the type (`BANK_FEES_*`).
- The line is stripped from every statement narrative it attaches to
  (`bookingLines`). Behind a payment order it trails the payee, and the
  payee stays.
- `isTurnoverTotalLine` matches the two words, any case, followed by
  figures and nothing else.

**TWINT rows** compose like any statement booking: `PAYMENT UBS TWINT;
<payee>; <street>; <town>` and `DEBIT UBS TWINT; <person>; <phone
number>; <reference>`. The counterparty is the payee, and the
description leads with the booking type. So the merchant signature
never reaches the phone-number segment.

**Why these sources.** The counterparty is silver's promoted column,
not a fresh read of the payload. It is the collector's stated
extraction and fills nearly every row; a second extraction here would
be a second source of truth. It feeds gold's merchant signature as
promoted, except where it is not a party: a booking type is refused
(`SignatureVersion` 9), and the bank's own charge names the bank
(`SignatureVersion` 11). So its format is part of this contract: a
change to what the collector promotes re-keys merchants.

The booking type is the provider category because it is the bank's own
filing of the entry. That is the nearest thing a bank statement has to
a card issuer's category. It stays verbatim: the export and the
statements differ in case, and PSN's is a code. The spending provider
tier translates the types whose meaning is clear in every spelling:
fees and charges, ATM cash, FX between own currency accounts, and card
bills. It treats every other type as a payment rail that says nothing
about what was bought ([../SPENDING.md](../SPENDING.md) §3,
`internal/spending/providermap.go`). The web eras' FX types are `fx`
kinds, which the spending population excludes by kind. Of the FX codes,
only an MT940 `NFEX` reversal, which the type-code floor does not read
(§6), reaches that tier.

### The bank reference, across both feeds

UBS states its own number for a booking in both feeds:

- The MT940 feed writes it as the `:61:` account-servicing-institution
  reference. The collector keeps it as `payload.bank_ref`.
- The export uses it as the row's silver `transaction_external_id`: the
  "Transaction no." the statement prints. UBS stamps the same number on
  both legs of a move between two accounts of one relationship. So the
  web silver keys a transaction by (number, account).

Gold's id for an export row is `<number>@<account>`. The adapter also
writes the number into `payload.bank_ref`, the key the MT940 feed uses,
by the same rule as the counter account below: one key for both feeds,
and a derived value never displaces a parsed one. Statement rows stay
unstamped. Their `stmt:` ids are the collector's content hash, not a
number the bank wrote.

Three passes read the reference:

- The era text fold and the seam (below) match an export row to its
  MT940 copy on (account, number). The account is part of the key
  because the two legs of a transfer share the number.
- The internal-transfer matcher pairs on it ahead of amounts
  ([../SPENDING.md](../SPENDING.md) §3, *The reference road*). That
  reaches a conversion between two own accounts, whose legs carry
  different figures in different currencies. UBS books a transfer's
  charge under the transfer's own reference, so a third row under one
  number is common, and the matcher refuses it.

### The era text fold

The cut gives an overlapping booking to the MT940 row, and that row
often says less than the export's:

- the `:86:` narrative can reduce to the bank's own code;
- the `:61:` type code is the whole provider filing;
- MT940 has no structured payee.

The export's row for the same entry names the payee, the printed
booking type and the detail line. So the PSN transaction stream is
wrapped in a fold (`psnWebTextFoldStream`). It fills such a column from
the export's record of the same entry, matched on (account, bank
reference).

- It works per column and only downward. A column that is empty or a
  bare code takes the export's value, unless that is a bare code too.
  A bare code has no separator and at most a few alphanumerics, a
  trailing MT940 subfield marker discounted (`isCodeOnly`). A column
  that already says something keeps it.
- Nothing else moves. Amount, value date, kind and id stay the MT940
  row's, and no row is added or dropped. A false match costs one wrong
  narrative, never a duplicated or lost booking.
- The payer's message does not travel. It is the payer's words, not the
  bank's record of whom it paid.

The fold changes what `Normalize` sees, so the rows it reaches are
re-keyed (`SignatureVersion` 7, [../SPENDING.md](../SPENDING.md) §4).

### The era fold

A statement row and its export or MT940 copy share no id. Nothing in
silver pairs them: the statement loader dedups only within the archive,
and the hard cut arbitrates web against PSN, not one web era against
another.

The era fold (`buildEraFold`, web side) is that match. Its key is four
facts of the booking. Each comes from the adapter's own projection of
the row, never from the raw silver columns:

| Key component | Web eras (`webProjectedNet`) | Feed era (`buildTransaction`) |
| --- | --- | --- |
| Account | `transactions.account_external_id` | `payload.account`, else `events.account_external_id` |
| Value day | `value_date`, floored to UTC midnight | `events.timestamp`, floored to UTC midnight |
| Signed amount | the projected `net_amount`, rounded to the minor unit | the projected `net_amount`, rounded the same way |
| Currency | `currency_iso`, trimmed and upper-cased | `payload.funds`, else `events.currency_iso`, with the builder's `XXX` fallback |

**Why the projection.** The two web eras write the amount columns to
different conventions. A statement prints a debit as a positive figure
in its debit column. The export carries the sheet's own cell, whose
sign already states the direction. So `amount_credit − amount_debit`
has opposite signs for one booking in the two eras. Both eras agree on
which column holds the figure, and `webKind` reads the direction from
that. The projection takes the direction from the kind and the
magnitude from the figure (`canonical.ApplyCanonicalSign`), so both
eras land on one signed number.

- The key is signed. A booking and the bank's correction of it are
  equal and opposite on one day, and folding them would delete a real
  entry. A reversal row (`<base>;Reversal`) keeps its own sign for the
  same reason.
- A kind with no pinned direction (interest, `fx`, `other`) passes the
  source's sign through. There the eras can disagree, and the pair does
  not fold. The fold never guesses.
- No narrative enters the key: each era words the entry differently.
- A zero amount never folds. It names no sum, and a day can carry
  several unrelated zero-amount period-close lines.
- All three eras store a UTC-midnight value date. The flooring is a
  safety net.

**The rules:**

- **The statement copy is always the one dropped.** The export and the
  feed are the bank's machine-readable record. A statement row comes
  from a layout parser, one step further from the bank. The rule is per
  era, not per row. The survivor's id, account, amount, value date and
  kind stay as they are.
- **Two rows of one era never fold.** Two identical payments on one day
  are ordinary, and within one era they carry distinct ids. Pairing is
  1:1 and deterministic: ids sorted, export rows offered before feed
  rows. A day with two statement copies and one export row folds exactly
  one of them.
- **The dropped copy's text carries over**, by the same `richerText`
  rule as the era text fold. An export survivor takes it as it is
  emitted. An MT940 survivor takes it in `psnStatementCarryStream`,
  applied outside `psnWebTextFoldStream`, so the export's text fills a
  bare column first.

**What follows:**

- A folded row is out of the same-day offset veto's universe
  (`buildSameDayOffsetVeto`). Otherwise one booking could consume two
  mirrors.
- Where a folded row carried an external deposit or withdrawal, returns
  count that flow once. The fold adds no flow and changes no amount.
- Survivors that gain a payee are re-keyed (`SignatureVersion` 8,
  [../SPENDING.md](../SPENDING.md) §4).
- The adapter logs how many statement rows it folded on each load,
  beside the dropped period summaries.

### The settlement fold

Two PSN rails record one securities trade: the MT515 confirmation and
the MT940 `:61:` line for its cash leg, typed `NSEC` with a bare
booking code. They share no id. Left alone, one trade would be two
rows, and every count, turnover and per-instrument total would double.

`buildSettlementFold` (`transactions.go`) matches them on the
settlement both rails state:

- the cash account;
- the currency;
- the unsigned figure: the confirmation states direction in its side,
  the line in its debit/credit mark;
- the settlement day: `:98A::SETT//` on the confirmation, the value
  date on the line.

It reads the whole silver, unwindowed. Whether a booking is recorded
twice depends on silver's contents, never on the slice a load covers.

- **The confirmation survives.** It carries the ISIN, the quantity, the
  price and the side. The line names no security.
- Only a line the bank typed `NSEC` is eligible. An ordinary payment
  that matches a trade's account, day and figure stays.
- A trade settling on a cash account outside the MT940 delivery keeps
  its confirmation alone.

The confirmation names its cash account in the bank's internal form
(`:97A::CASH//`), not the IBAN every other rail keys by.
`cashAccountIBANs` resolves it from the master-data feed, which states
both. Unresolved, the trade would attach to an account nothing else
records.

### The seam

The hard cut sits at the first PSN dump. That is the day PSN's coverage
becomes complete: before it, the MT940 feed holds only part of the
ledger. But the first dump's statements reach back over the days before
it. For a short window both feeds hold an entry and neither side's
window excludes it.

`buildSeamBankRefs` closes that window on (account, bank reference).
That is an exact identity, not a signature over amounts. The web copy
is dropped, as the cut does on every later day: the MT940 row reaches
gold and the export's text folds onto it.

### The accounts the feed does not speak for

The MT940 feed is delivered per account, and a relationship's delivery
need not reach every account. A current account is typically in scope.
The cash accounts behind a managed portfolio, one per currency, often
are not. `psnCashCoverage` records, per account, the first value day
the feed carried a movement. Three decisions read it.

**The hard cut is per account** (`psnCut`). The cut gives a web row to
the feed only where the feed holds that account's bookings by that day.

- On an account the feed never reaches, the export is the record on
  every day. Otherwise a transfer from a covered account into such an
  account would reach gold with one leg: an unpaired outflow in the cash
  flow statement, and owner capital leaving in the returns.
- Where the feed reaches an account later than the relationship's PSN
  start, the account's first statement is its cut. The export keeps the
  days before, the feed the days from it on, and the seam drops what the
  first statement reaches back over.

**A conversion the feed describes gets its counter-leg**
(`conversionMirrors`). A currency conversion between two own accounts
is one booking on each. Where the feed speaks for one side only, that
side's `:86:` narrative still states the whole movement:

- the beneficiary is the holder (`account_holders`);
- the `/OCMT/` subfield names the currency and the figure the other
  account booked.

The other account follows from that currency. A managed portfolio holds
one cash account per currency, and the paying account names the
portfolio (`portfolioCashAccounts`, the index §11 also uses). The
adapter books the other leg on that account: the stated figure and
currency, the same value day, the opposite direction, id
`mirror:<event id>`.

- The paying row gets the counter account and the stated leg, under the
  keys the export era writes for the same facts. The mirror gets the
  paying account as its counter account, and the bank's reference. So
  the two pair by every road the cash flow statement has
  ([../CASHFLOW.md](../CASHFLOW.md) §4). `vetoConversions` pairs them on
  the stated leg, as it pairs a statement's `CCY amount Rate` line, and
  demotes both for returns.
- Where the feed speaks for the other account that day, nothing is
  booked: its own statement carries the leg.
- Where the export carries the booking, the export's row is the record
  and the mirror is withheld (`buildSameDayOffsetVeto`). The feed row
  still names the account.
- A statement copy from a later annual statement folds onto the mirror
  as onto any feed row (the era fold).
- A payment abroad in a foreign currency carries the same subfield but
  names a payee other than the holder. It books nothing.
- Two accounts of one currency in one portfolio are refused, not
  guessed at.

**The cash a corporate action paid is booked from its confirmation**
(`corporateActionCashLegs`). A dividend reaches PSN twice: the MT566
confirmation on the custody account, and the MT940 line crediting the
cash account. Where the feed speaks for the cash account, the line is
the row. The confirmation stands beside it as a `corporate_action`
marker with no amount. Where the feed does not, the confirmation is the
only record. Its `CASHMOVE` block states:

- the cash account (`:97A::CASH//`, internal form, resolved through
  `cashAccountIBANs`);
- the amount posted (`:19B::PSTA//`), the gross and the tax withheld;
- the payment day (`:98A::PAYD//`).

The adapter books that leg as `<event id>:cash`: a `dividend` for
`DVCA`, `interest` for `INTR`, with the gross and the tax in its
payload.

- It books only a cash option credited (`CAOP CASH`, `CRDB CRED`).
- A capital call confirmed as `OTHR`, a dividend taken in shares, an
  exchange or any other event is left to the rails that carry it.
- Without this leg, a mandate's foreign-currency income would show only
  in its balances, or as "other income" when later converted into a
  covered account.

A conversion between two accounts the feed does speak for stays open
(§13).

### The counter account, across both eras

Two readers ask whether a row's other side belongs to the relationship.
Both web eras state the counter account, in the payload key
`counter_account`:

- **Statement era.** The collector's PDF parser writes it, from the
  booking's continuation lines: a full CH IBAN, or a UBS mortgage
  account stamp.
- **Export era.** The CSV writes a three-part narrative into
  `Description3`:

  ```
  Reason for payment: <purpose>; Account no. IBAN: <iban>; Transaction no. <n>
  ```

  `counterAccountFromNarrative` lifts the IBAN alone, never the purpose
  or the payee. Those name a person, and this value is compared against
  account ids.

The readers:

- **Returns.** In either era, a counter account the relationship owns
  makes the row internal (`buildOwnIBANSet`; also rule 2 of
  `pdfCashIsExternal`'s decision order). This only demotes, as the conduit model requires: a known own
  counter can take a row out of the flow series, never put one in.
- **The far account.** Gold keeps the payload verbatim. The enrichment
  pass resolves the stated counter account against gold's accounts and
  records where an own-account move went, even where the far side has
  no transactions to pair with ([../CASHFLOW.md](../CASHFLOW.md) §4). It
  sets the destination, never a verdict.

### The other leg, where the statement describes it

On one leg of a conversion, a statement writes what the other leg
holds:

```
CCY 1 234.56 Rate 1.234567
```

`counterLegFromNarrative` lifts the currency and the figure. The match
is anchored on the whole line and on the closing rate, because the
amount separates thousands with a space and running text must not read
as one. Both values go into the payload (`counter_currency`,
`counter_amount`), or neither does.

A description is weaker than a reference. It must be matched, and two
conversions of one size on one day answer it equally. So both readers
demand exactly one row that answers the description and exactly one
that makes it, and refuse otherwise:

- the matcher's stated-counter phase ([../SPENDING.md](../SPENDING.md)
  §3);
- the offset veto's conversion phase (`vetoConversions`).

It reaches the one shape nothing else does: a conversion between two
accounts of one relationship, whose legs carry different figures in
different currencies and, in the statement era, share no reference.

**The veto's conversion phase.** The veto's other phases bucket by
(day, currency) and match equal, opposite amounts, so they cannot see
such a pair. `pdfCashIsExternal` reads a statement-era arrival credit
as external on its booking type alone, because the veto peels the
intra-relationship shapes off first. A conversion between own accounts
is such a shape. Without this phase, the paying leg would be internal
by the gate's default and the receiving leg external. That one-sided
demotion would fabricate an arrival of owner capital in the returns.
`vetoConversions` demotes both legs or neither.

### The instrument, across both eras

The export era states an ISIN in `Description1`, and
`projectWebTxText` puts it on `instrument_external_id`.

The statement era names the security in free text, which closes with
the Swiss valor. A valor identifies one security line, so the match is
an identity, not a name guess. The collector's PDF parser promotes it
beside `counter_account` (`security_valor`). The adapter resolves it
against the valor index (`buildValorIndex`), built from three roads:

- `payload.InstrIdtfr.Valor`, where PSN's instrument carries the
  identifier object;
- the ISIN itself for a Swiss line: a `CH` ISIN is `CH`, the valor
  zero-padded to nine digits, and a check digit;
- the portfolio transaction list, which prints the valor and the ISIN
  of a security on the same row. This reaches securities PSN never
  described.

A valor naming two instruments is dropped, not resolved. A stated
identifier outranks a looked-up one, so the export era never consults
the index.

Where the lookup finds nothing, two weaker claims follow, in order:

- The valor goes on `instrument_hint` ([../DESIGN.md](../DESIGN.md)
  §10.8), which a `transaction_instruments` config entry is keyed by.
- `unlinkedSecurityTaxonomy` reads what kind of thing the row traded
  from the booking type, else the narrative
  ([../TAXONOMY.md](../TAXONOMY.md) §6). Only on buys and sells: a cash
  movement whose narrative happens to carry such a token traded no
  security. Nothing is stamped beside a resolved instrument; the
  instrument's own row answers.

## 8. Change number

Each feed has its own change number. The source's is the later of the
two.

- **PSN:** `MAX(dump_runs.snapshot_at)`, or `-1` when `dump_runs` is
  empty.
- **Web:** the later of `MAX(dump_runs.snapshot_at)` and
  `MAX(transactions.value_date)`, or `-1` when both are empty.

Other web dates never count toward the change number:

- the statement reconstructions and historical mortgages widen
  `Status`'s snapshot range;
- they, the card ledger and statements, and the portfolio transaction
  list widen the load window (§9, §10.3, §11).

`ChangeWindow` never advances the watermark to those dates. A
future-dated maturity notice would otherwise keep the source from ever
reading as up to date.

**The load window.**

- PSN: a new `dump_runs` row or event past the watermark triggers a
  load. PSN delivers a report after the business day it covers, so a
  dump's holdings, balances and events are dated before the dump, and
  before the previous dump's time as well. So the window reaches seven
  days before the previous change number for those business-dated rows
  (`psnBusinessDayLookback`). Rows in that stretch that gold already
  holds are re-emitted unchanged. A report older than seven days, such
  as an archive backfill after a long outage, needs `wealthdb reload`.
- Web: a dump or a transaction value date past the watermark triggers a
  load. The new change number is the latest of them. The window then
  widens to every historical, card and portfolio-transaction date.

**Observed through.** The report feed is observed through its latest
report. The web feed is observed through its latest run for the
holdings. For the ledger it is observed through the end of the first
gapless stretch of the runs' windows (`window_since`, `window_until`).
The source as a whole is observed through the earlier of the two feeds:
the reports state the securities, the web feed the cash accounts
([../DESIGN.md](../DESIGN.md) §6.4).

## 9. Historical (PDF-reconstructed) data

`ubs-web` reconstructs these tables from the eDocuments PDF archive:

| Silver table | Source PDF | Cadence | Gold target |
| --- | --- | --- | --- |
| `historical_position_snapshots` | "Statement of Assets" | Quarterly | `positions` (securities); cash rows skipped, see below |
| `historical_cash_balances` | "Account Statement" | Monthly | `cash_balances` (opening + closing) |
| `historical_mortgages` | "Maturity Notice" | Quarterly | `accounts` (kind `mortgage`) + a negative `mortgage` position |

They cover the dates before PSN starts. A row on or after its
portfolio's or account's PSN start is dropped, so PSN owns those days
(`buildHistoricalCutoffs`). The adapter emits them as a separate stream
(`webReader.snapshotsHistorical`) ahead of the live web and PSN
streams, so gold fills oldest-first.

### Security positions

These are the `historical_position_snapshots` rows with an
`instrument_isin`. The PDFs do not state the safekeeping account
reliably, so silver leaves `account_external_id` empty. The adapter
attaches each position to the PSN safekeeping account its portfolio
maps to, where exactly one can hold it (`statementSecuritiesAccount`).
Otherwise it uses the portfolio's overlay account
(`<portfolio>:overlay`, kind `overlay`), which PSN also uses for
forward contracts. Either way every gold position has an `accounts`
row.

| Silver column | Gold mapping |
| --- | --- |
| `as_of_date` | `positions.snapshot_at` |
| `portfolio_external_id` | `accounts.portfolio_external_id` (PSN's `PrtflId` form, not the live web feed's short code) |
| `instrument_isin` | `positions.instrument_external_id`, `instruments.isin` |
| `currency_iso` | `instruments.currency` |
| `units` | `positions.quantity` |
| `market_value` | `positions.market_value` (in `market_value_currency`, usually the portfolio's base), plus `accrued_interest` ([../DESIGN.md](../DESIGN.md) §7.1); the printed value stays in the payload as `printed_market_value` |
| `cost_basis` | `positions.book_value` (§12). Without it, or without `market_value_currency`, the book value is NULL and the cost figures travel in the payload |
| `accrued_interest` | `positions.accrued_interest`, where `market_value_currency` is set; a printed 0 adds nothing |
| `description` | `instruments.name` |
| `sector` | payload only |

A year-end Statement of Assets prints a gold-bar detail line for the
precious-metals portfolio that the quarterly statements omit. The
adapter already emits one overview precious-metals position
(`PM-<portfolio>`) for that portfolio. Where the overview exists for the
same date and portfolio, the detail line is suppressed, so the metal
counts once.

The PDFs carry no CFI or UAC code, so the pair comes from the
description (`taxonomyPairForWebDescription`). UBS generates
descriptions from a fixed vocabulary per instrument type, so the
leading words are a reliable signal:

- "Reg.shs …", "Shs …", depository receipts, participation
  certificates → `(public_equity, stock)`;
- ETF umbrellas → `(RefineETFExposure, etf)`;
- SICAVs and funds → `(…, fund)`; money-market, infrastructure and
  private-equity names sharpen the exposure;
- "Actively Managed Certificate" → `structured_product`;
- precious-metals and gold-bar lines → `(metal, physical)`;
- anything else → `(other, other)`.

Where PSN describes the ISIN, its CFI-derived pair wins. The
description decides only for instruments PSN never described.

### Cash balances

`historical_cash_balances` is the richer, monthly source. The cash rows
of `historical_position_snapshots` (no `instrument_isin`) are skipped.
Their quarter-end dates would land on the same `(account, currency,
balance_kind)` as the matching month-end row and collide on the
`cash_balances` key.

| Silver column | Gold mapping |
| --- | --- |
| `period_start`, `opening_balance` | `cash_balances` row, `balance_kind=opening`, `snapshot_at=period_start` |
| `period_end`, `closing_balance` | `cash_balances` row, `balance_kind=closing`, `snapshot_at=period_end` |
| `account_external_id` | `cash_balances.account_external_id`; also an `accounts` row (kind `cash`) once per period |
| `total_debits`, `total_credits` | payload only |

A row writes zero, one or two balances, one per non-NULL side. A NULL
side (a closed account's statement, a period not yet closed) is skipped,
not read as 0. Silver keeps NULL apart from a real zero.

### Mortgages

A Maturity Notice states a mortgage's balance "as at" a date. The
adapter emits it on the portfolio snapshot of that date, as the same
account, instrument and position triple as a live mortgage. It only
joins a snapshot that already exists. UBS issues the next quarter's
notice ahead of time, and a mortgage-only snapshot at that future date
would become the source's latest snapshot, hiding every other position.

### Window semantics

Once a load triggers (§8), `ChangeWindow` widens `Start` back to the
earliest historical date and `End` to the latest. The loader's
window-DELETE then covers every historical gold row before it is
re-inserted, so a reload never collides on the gold key. Historical
dates never move the change number.

## 10. Credit cards (ubs-web migration 0007)

Cards reach gold from the `ubs-web` silver's `card_*` tables. They are
**web-only and cutoff-free**: the PSN cut exists to stop the web feed
restating what PSN says better, and PSN says nothing about cards at all.
Same treatment as mortgages, for the same reason.

| Silver table | Gold target |
| --- | --- |
| `card_accounts` | `accounts` (kind=`card`) + a CURRENT `cash_balances` row |
| `card_invoices` (reconciling ones) | `cash_balances` (CLOSING) at `period_end` |
| `card_transactions` | `transactions` |
| `card_statements` | — (the document index; no figure is parsed from a PDF) |

### 10.1 The sign is NOT flipped here

This is the one place a reader of the chase adapter will guess wrong.

Chase's silver stores a card provider-verbatim as a **positive amount
owed**, and `signedBalance` negates it. UBS reports the opposite already:
a purchase is negative, a payment positive, and a balance is negative
while the card carries debt. That is `AccountKindCard`'s own convention —
a revolving-credit liability held as negative cash — so the figures pass
through unnegated. Gold migration 0037's note about "the adapter negating
the provider's owed-positive figure" describes chase's silver, not this
one.

Amounts still pass through `canonical.ApplyCanonicalSign`, because the
card kinds have a fixed direction: a `purchase` comes out negative and a
`refund` / `card_payment` positive whatever the row said.

### 10.2 `transactions.kind`

A card ledger has no booking-type column — the cash surface's
`description_kind` has no card equivalent — so the kind comes from the
sign, plus the settlement descriptors for the one distinction the sign
cannot make.

| Silver row | Gold `kind` |
| --- | --- |
| amount < 0, descriptor is `TRANSFER TO ACCOUNT` | `withdrawal` |
| amount < 0, anything else | `purchase` |
| amount > 0, descriptor is `DIRECT DEBIT` / `DIRECT DEBIT (SWIFT)` / `TRANSFER FROM ACCOUNT` | `card_payment` |
| amount > 0, anything else | `refund` |
| amount == 0 | `other` |

On a card, direction *is* the classification: money off the card is
spend, and money onto it is either the bill being settled or a merchant
giving some back. Only the descriptor tells those two apart, and it is
matched **whole** — a merchant whose name merely contains the words is
still a refund. The one debit that is not spend is the transfer rail's
mirror: a credit balance the card returns to an account. It is the
card's half of an own-account move, booked as the `withdrawal` the
matcher pairs with the account's deposit. As a purchase it would pair
with nothing and read as consumption.

`reward` has no producer: UBS books a rewards credit as an ordinary
credit with no descriptor that distinguishes it, so the kind is left
unproduced rather than guessed at.

### 10.3 Balances

Two sources, disjoint by construction:

| Source | Kind | Stamped at | `payload.basis` |
| --- | --- | --- | --- |
| the roster's live figure, as reported | `current` | the dump time | `roster` |
| a billing period's closing figure | `closing` | `period_end` | `statement_closing` |

The roster figure is emitted unchanged. It **already includes** the
account's authorised-but-unposted spend: an account's balance equals the
sum of its cards' `balanceIncludingReserved`, not of their `balance`. So
`card_accounts.reserved_amount` records how much of the balance has not
yet booked — a part of it, never an addition to it, and adding it back
would count that spend twice.

The statement series is the **only** history. Neither the ledger nor the
roster carries a running balance, so without it a card would have exactly
one balance in gold — today's. Only periods whose own figures reconcile
(`card_invoices.reconciles = 1`) are emitted: a wrong balance is worse
than a missing one, because gold's carry-forward rule fills a gap from
the neighbouring observation but nothing corrects a figure that is
present and wrong.

A period end is a billing date and need not fall on any dump time, so
`ChangeWindow` is widened by `cardRange` to cover both the ledger's dates
and the period ends. That is a correctness requirement, not tidiness:
gold deletes the window before re-inserting it, so a record emitted
outside it would be inserted again each load without its predecessor
being removed.

### 10.4 Returns and allocation

A card is returns-invisible **engine-wide**, keyed on the account kind
itself rather than on any per-source policy
(`returnsInvisibleKind`, `internal/gold/returns.go`; RETURNS-NOTES.md,
"Credit cards are returns-invisible"). The UBS adapter needs no policy
change to get this. It only emits `AccountKindCard`, as the table in
§10 shows. A card emits no positions either, so nothing reaches
the instrument-taxonomy rollups; its balance surfaces through
`report_cash` exactly as chase's does, which is what keeps it in net
worth.

### 10.5 Text columns

Extending §7's text columns, for card rows:

| `counterparty` | `provider_category` | `description` |
| --- | --- | --- |
| the terminal descriptor (silver `merchant`), verbatim — the input to gold's merchant signature | the MCC description (silver `merchant_category`), verbatim | the same descriptor |

**The API's field names are the wrong way round**, and the collector's
promotion is where that is corrected: UBS's `merchantName` holds an ISO
18245 category description (`Grocery stores`, `Taxicabs`) while `details`
holds the actual merchant. Reading them as named would key every merchant
signature on a category and hand the provider tier a payee.

The card vocabulary is **categorical** — every row carries one — whereas
this source's booking types are payment rails where a miss is the normal
case. Both belong to the same `ubs` silver kind, so the spending
provider map is keyed by **product**: `ubs/card` for the MCC
descriptions, `ubs` for the booking types
(`internal/spending/providermap.go`, SPENDING.md §3). One entry is
deliberately left untranslated — the bank's own catch-all for a card row
that moved money rather than bought something names no line of business,
and placing it would file person-to-person transfers as shopping.

### 10.6 The card bill, and where it still double-counts

Gold files a card bill paid from a cash account as `card_spend`. The
internal-transfer matcher replaces that verdict once the card's own
settlement leg is in gold ([../SPENDING.md](../SPENDING.md) §2, §3). With
UBS cards collected, the bill leaves the spending base. The card's own
purchases carry the spending instead, placed by the provider tier from
their MCC descriptions.

Both UBS bill shapes pair:

- the payment order, whose counterparty is the bank's own name, with
  `C/O UBS CARD CENTER` behind it in the description;
- the LSV direct debit, whose counterparty is the mandate notice.

The built-in rule reads the raw narrative fields as well as the
signature, which finds the creditor in either.

A card billed in one currency and settled from an account in another
does not pair, and there the spending counts twice. The card ledger
mints its own ids, so no reference joins the two legs, and the amount
pass does not cross currencies. [../SPENDING.md](../SPENDING.md) §3
(*The reference road*) explains why the rule is not narrowed and how
`wealthdb categorize` surfaces such near-pairs. A pin, a config rule or
a `match` line in the transfer-override ledger states the pair.

### 10.7 The card reference on a cash-account row

A card transaction booked on the *cash* account — a cash-machine
withdrawal, a debit-card purchase — carries the card's number and expiry
ahead of its booking type in the same slot a payer's typed message uses:

    <number>-<check> MM/YY; ATM Withdrawal

The split at the last `; ` reads the type correctly. The leading part is
not a memo, though: a memo is the payer's own words, shown as such, and
a config rule may key on it ([../SPENDING.md](../SPENDING.md) §3).
`cardReferenceRe` recognises the reference whole and yields no memo for
it.

- The match is narrow: the check-digit suffix and the expiry together.
  A typed message that merely opens with digits is still a message.
- The booking type and the merchant signature do not change. A
  signature reads the description only up to the memo separator.
- The raw column stays whole in the row's payload. The reference is
  dropped from a projection, not from the record.

## 11. The portfolio transaction list (ubs-web migration 0012)

A managed portfolio's securities trades reach gold on three rails, and
the two older ones leave a hole between them.

| Rail | Ids | Covers |
| --- | --- | --- |
| Annual Account Statement PDF | `stmt:` | A calendar year, published the following January — so the current year is unreadable for as long as twelve months, and a year whose statement was never published for an account is never covered at all. |
| PSN MT515 confirmations | `mt515:` | From wherever that feed was first ingested. Nothing earlier is recoverable from it. |
| The portfolio transaction list | `ptx:` | Back to the surface's own archive floor, which the collector records per export. |

Between the last statement and the first confirmation, a discretionary
mandate's cash account shows money arriving and leaving with nothing to
explain it — which the cash flow statement can only read as cash the
household kept.

**The rows are booked on the CASH ACCOUNT that paid**, not on the
custody account the securities moved in: that is the account whose
balance the trade moved, the one `report_cashflow_coverage`
reconciles, and the one the MT515 rail already books its own
confirmations against, so the two rails are continuous. The list names
the custody account and the settlement currency, never the cash
account; the pair (portfolio, currency) determines it, resolved
through PSN's account master data because the web collector's own
account rows carry the banking relationship rather than the numbered
portfolio. A pair naming more than one account — a dormant account
beside a live one — is refused rather than guessed at.

**The kind is read off the figure, not the booking type.** The export
signs its own values (positive leaves the portfolio, which on the cash
leg means money going out), so a purchase is a `buy` and a disposal a
`sell` without the classifier knowing a vocabulary of several dozen
booking types that grows without notice. The type still travels, as
the row's `provider_category`. Two things say a row settles nothing:
no value stated (corporate actions, in-kind issues, and the currency
conversions — which state a PAIR of figures in one cell and so state
no single settlement amount), and the free-of-payment custody
transfers, which are valued because the securities are worth something
and settle nothing.

**Gross, not net.** The list states a trade's value; the commission is
not in it. The difference to the confirmations is the fee, so a coverage
gap this rail closes closes to within the fees rather than exactly.

### The settled-day fold

One booking, one row, as everywhere else in this adapter — but the
identity has to be weaker here than the [era fold](#the-era-fold)'s,
because the three rails state different AMOUNTS for the same trade:
the gross value here, the net debit on the statement and on the
confirmation. So the key is (cash account, currency, settlement day),
with no amount in it, and it counts rather than sets: a day the other
rails cover only partly — a statement published for some of a year's
trades and not others — keeps its surplus.

What it counts is the EMITTED universe, never silver's contents. The
cash pass hands over its own count, accumulated from the rows it
emitted after its cut, its era fold and its seam; the feed's half is
rebuilt through the same [settlement fold](#the-settlement-fold) the
PSN pass applies, so a trade recorded there twice — as a confirmation
and as the MT940 line for its cash leg — is counted once.

No PSN cutoff applies, and none could: the cut arbitrates between two
records of one booking, and this pass emits only where no rail
recorded the booking at all.

## 12. Cost basis and realized lots

[../DESIGN.md](../DESIGN.md) §7.4 sets the rules for a book value and
its stamp. UBS keeps every holding at its average cost and
states that cost without the purchase fees. So every UBS stamp has the
method `average`, or `paid_in` for a private-markets fund, and the fees
`excluded`. The stamps live in `basis.go`.

### The book value of a position

| Source | Book value | Stamp |
| --- | --- | --- |
| PSN `holdings`, BOOK in the position's currency | `cost_basis`, the MT535 BOOK amount | stated, average, excluded |
| PSN `holdings`, BOOK in another currency | `cost_basis` × `acquisition_fx_rate` (AEXR), when AEXR runs from BOOK's currency to the position's | derived, average, excluded |
| PSN `holdings`, any other | NULL. `cost_basis`, `cost_currency` and the AEXR fields travel in the payload. | — |
| Statement holding (`historical_position_snapshots`) | `cost_basis`, the statement's cost value, in `market_value_currency` | stated, average, excluded |
| Statement holding without a cost value, or without `market_value_currency` | NULL. A cost value travels in the payload as `cost_basis`, and a cost price as `cost_price` with `cost_currency`, the instrument's currency. | — |
| A private-markets fund's units | the capital calls in `advices` up to the snapshot (below) | derived, paid_in, excluded |

- **AEXR.** One unit of the first currency is `rate` units of the
  second. A PSN position's currency is the currency of its chosen HOLD
  leg, which is usually BOOK's own, so the conversion is the exception.
- **The cost price is never multiplied out.** A bond's cost price is a
  percent of the nominal, so units × cost price would be a hundred
  times its cost. The cost value is the statement's own figure.
- **A silver older than the cost columns** states no book value. Both
  feeds still load.

**Capital calls.** Neither feed states a cost for a private-markets
fund's units. MT535 carries no BOOK, and the statement prints a NAV in
place of a cost. The capital calls are what was paid in
(`paid_in.go`). A call names the fund's ISIN and no account, so it
joins a position only when:

- every call on the fund has a date and all share one currency;
- one account holds the fund across both feeds: the PSN safekeeping
  account, or the account a statement portfolio maps to;
- the position is in the calls' currency and states no book value of
  its own.

The book value at a snapshot is the sum of the called `amount` of every
call whose value date is on or before it. A snapshot before the first
call has no book value. The placement fee and the equalisation
interest are separate figures, and they stay out. A distribution paid
back does not reduce the figure.

Silver keys a notice by its document, so a notice that arrives twice
is two rows. A call is therefore read once per fund, value date,
currency and amount.

### Realized lots

Each sale is one realized lot. UBS states no tax lots.

| Source | `document_kind` | Currency | Proceeds | Book value | Gain |
| --- | --- | --- | --- | --- | --- |
| `statement_trades` | `statement` | `reporting_currency_iso` | \|`transaction_value`\| | `cost_basis` (stated, average, excluded) | NULL. The list prints only percentages, and they travel in the payload. |
| `portfolio_transactions` | `trade` | `valuation_currency_iso` | \|`trans_value`\| | proceeds − `realized_pl` (derived, average, excluded) | `realized_pl` |

- **A statement sale** has a negative quantity, a sale's price
  (`transaction_price`) and a settlement amount, and its booking text
  names no reversal. Corporate actions and write-offs print no price.
  A delivery free of payment settles nothing. A reversed purchase
  sends units out but is no sale.
- **An export sale** has a negative quantity and a realized P/L, and
  its booking type names no reversal. Its value and P/L are in the
  export's valuation currency. The book value relies on the export
  computing the P/L against the same average cost the statement
  prints, so the value less the P/L is that cost.
- **A reversal** is a booking whose text holds a reversal word, in
  any case: in front of the text of the booking it reverses, or behind
  a separator after it. The words are "Reversal" and its German, French
  and Italian counterparts ("Storno", "Stornierung", "Extourne",
  "Annulation", "Annullamento"). Its sign is not what marks it.
- **A missing currency.** A sale printed without its reporting or
  valuation currency is no lot: a figure without its currency states
  nothing.
- **Dates.** The disposal date is the trade date and the settlement
  date the value date. The tax year is the trade date's year, else
  the value date's, else the statement date's.
- **Account.** The custody account the list prints, in PSN's form: the
  branch padded to four digits and the account body to ten
  (`custodyAccountCanonical`). A custody account PSN does not report
  falls back to the account the statement era uses for the portfolio.
- **Instrument.** The ISIN, else the valor through the valor index
  (§7, *The instrument, across both eras*). An unresolved valor is the
  hint a config link closes.

**Primary rows.** Within one account and tax year, the primary rows
count each sale once:

- A booking recurs in every statement whose period covers it. The
  portfolio and the settlement number name it across statements. A
  booking without a number is named by its trade date, security,
  quantity, value and booking text. The earliest statement that prints
  it with a currency keeps it, and later copies are not primary.
- A reversed sale is not primary. The list prints the reversal as a
  booking of its own, with the sale's figures and the units coming
  back. It cancels one sale with the same portfolio, security, trade
  date, quantity and value, once however many statements print it.
  That sale's payload says `reversed`. The export's reversals cancel
  its own sales the same way, matched on the custody account.
- An export sale is primary only on a day no statement's transaction
  list covers for its account. So on one account and day only one kind
  counts, and a sale both kinds state counts once where both book it on
  the same account. A sale on a covered day that no list prints counts
  nowhere. Both kinds rank alike, so a tax year a statement covers in
  part still counts the export's sales after it.

## 13. Open questions

- **A conversion between two covered accounts.** Both legs are feed
  rows, the `/OCMT/` subfield on the paying one states the other, and
  the veto's conversion phase pairs them for returns — but neither row
  names the other's account, so the cash flow statement's far account
  for the pair rests on the matcher, whose amount join cannot cross
  currencies. Stamping both rows with the counter account the stated
  leg identifies (the receiving row is the one booking the stated
  figure that day) would close it by the same road the mirror uses.
- **MT568 vs MT566 collapsing.** Both carry corporate-action info;
  MT568 is narrative supplementing MT566. The adapter emits both as
  separate `corporate_action` events. Consider
  merging when MT566 and MT568 share an `event_external_id`.
- **`pending_securities` projection.** If a use case for T+2
  visibility lands, project into a new gold table
  `pending_transactions` rather than mixing with settled
  `transactions`.
- **Historical taxonomy pair.** Historical security positions are
  classified by the description-template read (see the mapping
  section above); a description outside UBS's known templates
  stays `(other, other)`. Consider a per-ISIN taxonomy lookup
  populated from an external catalogue if a residual unmatched
  instrument ever matters.
