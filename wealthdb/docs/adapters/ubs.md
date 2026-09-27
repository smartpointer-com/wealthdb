# UBS adapter

Adapter that projects two UBS silver SQLite databases into the
canonical gold schema:

- `ubs-psn` — daily SDFI/MT-message feed delivered via SFTP.
- `ubs-web` — netbanking scrape (live snapshots + reconstructed
  historical PDFs).

Implements the `silver.Adapter` / `silver.Connection` interface
defined in [../DESIGN.md](../DESIGN.md) §6. When both subsources
are configured the orchestrator (`merge.go`) splices them: web
emits dimensions before PSN-start, PSN takes over from then on,
and PDF-reconstructed historical fills the pre-PSN-start range.

## 1. Silver sources

- Upstream feeds:
  - PSN: [`ubs-psn`](../../../collectors/ubs-psn/).
    Silver schema: [migrations/0001_initial.sql](../../../collectors/ubs-psn/migrations/0001_initial.sql).
  - Web: [`ubs-web`](../../../collectors/ubs-web/).
    Silver schema: [migrations/0001_initial.sql](../../../collectors/ubs-web/migrations/0001_initial.sql)
    + [migrations/0002_historical_snapshots.sql](../../../collectors/ubs-web/migrations/0002_historical_snapshots.sql).

## 2. Identifier conventions

UBS is the most dimensionally rich of the silvers. Two
identifier dimensions show up in every gold row:

| Field | Source | Notes |
| --- | --- | --- |
| `relationship_id` | UBS Server ID (`SFTPCHxx`, `SFTPCHyy`, ...) | One per banking relationship under the single SFTP login. Stored on the gold `accounts` row as a discriminator. |
| `account_external_id` | IBAN for cash accounts; UBS 28-char safekeeping code for safekeeping accounts | Whichever identifies the account uniquely within the relationship. |
| `instrument_external_id` | ISIN | UBS always supplies ISINs in SDFI. |
| `transaction_external_id` | UBS `:20C::SEME//` or MT940 `:61:` ref | Whatever the source MT message uses as its stable event reference. |

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `schema_meta` / `dump_runs` | meta only | Used by `Status` / `ChangeWindow`. |
| `account_holders` | — | Client/legal-owner metadata; not an account. Deferred. |
| `cash_accounts` | `accounts` (kind=`cash`) | IBAN as `account_external_id`. |
| `safekeeping_accounts` | `accounts` (kind=`safekeeping`) | UBS safekeeping code as `account_external_id`. |
| `portfolios` | `portfolios` | `PrtflId` as `portfolio_external_id` (the dedicated gold table, migration 0004). |
| `instruments` | `instruments` | See §4 for the `(asset_class, vehicle)` derivation. |
| `holdings` | `positions` | One securities holding per row. |
| `cash_balances` | `cash_balances` | Direct one-to-one; UBS `balance_kind` enum carries over. From the MT940 statement feed, whose delivery is per-account: an account is in scope for statements or it is not, and one that is not receives none on any day. |
| `cash_accounts.payload.BookBalAmt` | `cash_balances` (kind=`closing`) | The account master data states a daily book balance for EVERY cash account, so it fills the ones the statement feed never reaches (`appendBookBalances`). It IS the closing balance — measured, not assumed: on every (snapshot, account) the two feeds share it equals the MT940 closing figure to the cent and matches neither the opening nor the available one. Signed at the source, so no credit/debit indicator applies. A key the statement feed already emitted is skipped, so the pass is strictly additive. |
| `pending_securities` | — | Most rows are "no activity" markers (`ACTI//N`). Deferred. |
| `fx_rates` | `fx_rates` | Base currency is CHF in the current PSN setup. |
| `portfolio_performance` | — | Monthly TDPOPF analytics. Deferred. |
| `cash_account_pricing` | — | TDCAPI interest-rate config. Deferred. |
| `forward_contracts` | `positions` — `(foreign_exchange, forward)` | `contract_external_id` is `position_key`. |
| `option_contracts` | `positions` — `(foreign_exchange, option)` | Same. |
| `money_market_contracts` | `positions` — `(cash, time_deposit)` | Same. |
| `otc_contracts` | `positions` — `(foreign_exchange, forward)`, or `(other, other)` for a non-FX underlying | Same. |
| `events` | `transactions` | See `kind` mapping in §5. Two rows the feed does not carry as events are booked from them for the accounts the statement feed never reaches: the other leg of a conversion an MT940 line describes, and the cash an MT566 confirmation paid (§7, *The accounts the feed does not speak for*). |
| `portfolio_transactions` (web) | `transactions` | A managed portfolio's securities settlements, booked on the cash account that paid. See §11. |

## 4. `(asset_class, vehicle)` derivation for `holdings`

`silver.holdings` doesn't carry a taxonomy column directly. The
adapter looks up the holding's ISIN in `silver.instruments` and
reads the ISO 10962 CFI code (`InstrCtgyCFI` in the SDFI payload),
falling back to UBS's internal `UacAsstClsCd` bucket when the CFI
is empty — the non-listed custody items (e.g. metal-deposit
receipts, private-market fund interests) carry no CFI but do
carry a UAC code. `taxonomyPairForInstrument` derives the gold
`(asset_class, vehicle)` pair (exposure = what moves the value,
vehicle = the wrapper — TAXONOMY.md) from those signals: the CFI's
first character picks the vehicle, and the exposure comes from the
CFI, the `UacAsstClsCd` bucket, and the instrument name together.

| CFI category (first char) | Gold `(asset_class, vehicle)` |
| --- | --- |
| `E` Equities | `(public_equity, stock)` — except group `EY` ("structured participation instruments": tracker / actively-managed certificates), which is `(public_equity, structured_product)`, or `(foreign_exchange, structured_product)` when the name reads currency-/FX-linked (`currencyLinkedRe` — "currency", "FX", "forex", "dual currency"). Every other `E` group (`ES` shares, `ED` depository receipts, …) is a stock. |
| `C` Collective investment | The vehicle is `fund`, or `etf` for the `CE` group (ISO 10962:2015 ETFs); every other `C` group stays `fund`. The exposure is read from the instrument name by `silver.RefineETFExposure` — crypto → `crypto`, bullion → `metal`, bond keywords → `fixed_income`, everything else → `public_equity`. A fund's `UacAsstClsCd` sharpens the generic CFI and overrides the name read: `0100` (Liquidity) → `(cash, fund)`, `0400` (Hedge funds & private markets) → the private-markets family via `privateMarketsPair` (name contains "infrastructure" → `(infrastructure, fund)`, "hedge" → `(hedge_fund, fund)`, otherwise → `(private_equity, fund)`). |
| `D` Debt | `(fixed_income, bond)` |
| `R` Entitlements (rights) | `(public_equity, right)` |
| `O` Listed / `H` non-listed & complex options | `(public_equity, option)` |
| `F` Futures | `(public_equity, future)` |
| `J` Forwards | `(foreign_exchange, forward)` — the forward wrapper pairs only with FX in the taxonomy, and these feeds carry FX forwards. |
| `S` swaps · `I` spot · `K` strategies · `L` financing · `T` referential · `M` others · unrecognised | `(other, other)`. Not modelled as custody holdings — `T` in particular is reference data, not a position (UBS's per-currency reference rows carry `TC…` codes), so an honest `other` beats forcing an equity or structured-product guess. |
| empty CFI | Routes to the `UacAsstClsCd` fallback (`taxonomyPairForUAC`): `0100` (Liquidity) → `(cash, fund)`, `0300` (Equities) → `(public_equity, stock)`, `0400` (Hedge funds & private markets) → the private-markets family (see the `C` row), `0600` (Precious metals & commodities) → `(metal, physical)`, anything else → `(other, other)`. |

The emit path uses `silver.RefineETFExposure`, which returns the
*exposure* (asset class) of a collective vehicle from its name; the
vehicle (`etf` / `fund`) is supplied by the CFI group, so the pair
is `(RefineETFExposure(name), etf-or-fund)`.

### Latest-known-instruments lookup

UBS silver's loader applies content-based dedup on instruments —
a fresh row is written only when the SDFI payload changes. Most
snapshots therefore don't carry an instruments row for a given
ISIN, so a same-snapshot lookup misses for most holdings. The
adapter compensates by building a `map[isin]meta` from the
**most-recent** instrument row across the entire silver DB, then
using that for every holding. This is correct in practice
because instrument metadata (name, CFI category) is functionally
immutable — being stale by one snapshot is harmless.

### MT535 SWIFT-tag parsing (implemented)

Each `holdings.payload` carries the raw MT535 fields under
`payload.fields.{"19A","93B","35B",...}`, where each tag is an
array of raw SWIFT subblock strings. `mt535.go` decodes the two
tags we surface in gold:

- **`93B`** — quantity. Subfield format
  `:<qualifier>//<format>/<value>` where qualifier is one of
  `AGGR` (aggregate), `AVAI` (available), `NAVL` (not available),
  `AWAS` (awaiting settlement), etc.; format is `UNIT` (shares /
  contracts) or `FAMT` (face amount, for bonds). The adapter
  prefers `AGGR`, falling back to `AVAI` if missing.

- **`19A`** — monetary amount. Subfield format
  `:<qualifier>//<CCY><value>` where qualifier is `HOLD` (current
  market value), `BOOK` (book / cost basis), `ACRU` (accrued
  interest), and similar. A single holding typically carries
  multiple `19A` entries — the same `HOLD` value in the
  position's trade currency and again in the relationship's
  reference currency (CHF). The adapter prefers the `HOLD` entry
  whose currency matches the instrument's natural currency,
  falling back to the first `HOLD` entry if no exact match
  exists.

SWIFT value convention: comma is the decimal separator, and a
trailing comma is the end-of-amount terminator (so `1500000,`
parses to `1500000` and `150,123456` parses to `150.123456`).
`parseSwiftDecimal` handles both forms.

Holdings whose `payload.fields` is empty or whose 19A/93B
subfields don't match the expected shape leave the gold columns
NULL — the parser degrades gracefully so a single malformed
payload doesn't fail the whole load.

## 5. `events.kind` mapping

UBS's `silver.events.kind` discriminator (per the comment block
in `silver.events`) maps to gold's canonical `kind` taxonomy:

| UBS | Gold | Adapter notes |
| --- | --- | --- |
| `cash_movement` | `deposit` / `withdrawal` / `fee` / `interest` / `tax` / `dividend` / `buy` / `sell` / `fx` | from the MT940 `:86:` narrative, then the `:61:` type code as a floor (adapter splits — see §6); a deposit/withdrawal leg whose same-day mirror books on another own account carries the conduit verdict in its payload (same-day offset veto, `buildSameDayOffsetVeto`). A converted movement (`/OCMT/`) between two own accounts whose other account the feed does not speak for books its counter-leg on that account, as `mirror:<event id>` (§7, *The accounts the feed does not speak for*) |
| `securities_movement` | `transfer_in` / `transfer_out` | sign-driven |
| `trade_confirmation` | `buy` or `sell` | from MT515 payload `side`, which the collector reads off the order's business function (`:22H::BUSE//`). That tag carries two vocabularies: a market trade names the party the holder was (`BUYI`/`SELL`, folded to `BUY`/`SELL` at load), a fund order the operation (`SUBS` subscribes, `REDM` redeems, kept verbatim). Dated to the day the trade was struck, not the day it settled (the collector reads both of ISO's trade-date tags). Books against the settlement's cash account, resolved to its IBAN (`cashAccountIBANs`); the MT940 line for the same settlement folds away (the settlement fold, §7) |
| `fx_confirmation` | `fx` | MT300 |
| `fx_option_confirmation` | `fx` | MT305 (no dedicated option kind; the settlement is an FX cash effect) |
| `loan_deposit_confirmation` | `other` | MT320/MT330/MT350 |
| `corporate_action_notification` | `corporate_action` | MT564 |
| `corporate_action_confirmation` | `corporate_action` | MT566. A cash dividend (`DVCA`) or interest (`INTR`) paid into a cash account the MT940 feed does not speak for is also booked as a `dividend` / `interest` row on that account, `<event id>:cash`, from the confirmation's `CASHMOVE` block (§7, *The accounts the feed does not speak for*) |
| `corporate_action_narrative` | `corporate_action` | MT568 (narrative; may collapse with the MT566 row) |
| `securities_settlement_advice` | `transfer_in` / `transfer_out` | MT544–548 |
| `precious_metal_trade` | `buy` or `sell` | MT600/MT601 |
| `charges_advice` | `fee` | MT590/MT990 |
| `debit_credit_confirmation` | `deposit` / `withdrawal` | MT900/MT910 |

### `ubs-web` booking types

The web silver's `description_kind` — the statement PDF's printed
booking type, the CSV feed's `Description2` label — is classified
by `webKind` (`web_reader.go`) — on a CSV-feed row whose booking-type
column is empty or holds a reference rather than a type, the first
segment of the promoted counterparty stands in for it (`webKindHint`),
since such rows carry the product there —
which matches the whole type case-insensitively so the two eras'
spellings (`CREDIT` / `credit`) land together: the income and cost types (`DIVIDEND`,
`COUPON`, `CUSTODY PRICE`, `INTEREST CALCULATION BALANCE`, …) to
`dividend` / `coupon` / `interest` / `fee`, the FX types to the
`fx` kinds, the securities settlements to `buy` / `sell` by cash
direction, and the mobile-payment types to the money-moving kinds.

`UBS MANAGE` is a cost, not a settlement: it is the discretionary
mandate's periodic management charge, billed to the mandate's own cash
account at each period end, and like the other charges it names the
PRODUCT rather than a security — no instrument, no quantity, no price.
Classified by cash direction it booked a purchase of nothing: a period
charge that left no fee behind, while the cash it took still drained
the portfolio's value. `CAN UBS MANAGE` cancels a charge
already billed and `REC UBS MANAGE` re-bills the corrected figure; all
three take `fee`, and the cancellation's inflow survives because the
statement era prints it as a negative in the debit column, which
`webReversal` reads (below).


`ORDER` is a PAYMENT, not a securities order, and it sat among the
settlements long enough to be worth saying why it does not belong
there. Every other member of that group names an instrument type — a
share, a fund, a structured product — or spells a buy or a sell
outright. This one names neither, and the rows carrying it say so in
every column the bank fills: no quantity, no price, no instrument, and
a narrative naming the party paid rather than anything bought. What it
pays is an account of the same relationship, often in another
currency, and the receiving side books the mirror as a plain `CREDIT`.
It is the statement era's abbreviation of what the export feed spells
out as `payment order` / `e-banking payment order` / `special payment
order`, and it takes the direction exactly as those do. It is named in
the classifier rather than left to fall through, because that case
list is also the adapter's record of which types it knows.

| Booking type (any case) | Gold `kind` |
| --- | --- |
| `PAYMENT UBS TWINT`, `DEBIT UBS TWINT` | `withdrawal` |
| `CREDIT UBS TWINT`, `REVERSAL UBS TWINT` | `deposit` |
| `ORDER` | `deposit` / `withdrawal` by direction — a payment order |
| (other) | `deposit` / `withdrawal` by the column the figure sits in; `other` when neither or both are set |

The kind follows the type, not the column: the silver row carries
an unsigned figure in a debit or a credit column (a trailing-minus
figure on the statement stays negative), and `ApplyCanonicalSign`
orients the net amount by the kind, so a reversal keeps its inflow
whichever column printed it. A PDF-era deposit or withdrawal then
passes the era-gated external-flow classifier (`pdfCashIsExternal`):
in the MT940 era the TWINT credit and reversal count as arrivals
beside `CREDIT`, `E-BANKING CREDIT` and `SALARY PAYMENT`, and the
TWINT payment and debit as outbound rail payments; in the deep era
both directions stay internal, as every rail booking does there.

## 6. MT940 `:86:` narrative parsing

`cash_movement` events (MT940 `:61:` lines) carry a free-text
narrative in the `:86:` continuation. The adapter parses this to
split bare `cash_movement` into more specific canonical kinds.
The narrative parser is conservative — an unmapped narrative falls
through to the `:61:` transaction type code, and then to `deposit` or
`withdrawal` by direction, with the raw narrative preserved in payload.

The type code is a FLOOR, read only where the narrative says nothing a
reader could place, because the narrative is the better witness where
it exists — it is the only one that tells a transaction tax from a
custody price. It matters because an entry the bank wrote no narrative
for arrives as a bare booking code, and the cash leg of a trade
settling that way would otherwise read as a plain withdrawal: spending
would count it as money leaving and returns as external capital.
`NSEC` settles as `buy` or `sell` by direction, `NFEX` as `fx`, `NDIV`
as `dividend`, `NINT` as `interest`, `NCHG`/`NCOM` as `fee`, `NTAX` as
`tax`. An `NSEC` line whose trade the MT515 rail also confirms never
reaches this floor at all — the settlement fold (§7) drops it in favour
of the confirmation, which names the security the floor cannot. `NRTI` is a RETURNED ITEM, not interest, and is deliberately
unmapped. A reversal — marked `RC` or `RD` in the credit/debit field —
bypasses the floor entirely: its sign is not yet flipped at that point,
so any kind read off it would be stated against the wrong direction.

Common narrative prefixes (extend as observed):

| Narrative prefix | Gold `kind` |
| --- | --- |
| `INT` / `INTERETS` / `ZINSEN` | `interest` |
| `COMM` / `FRAIS` / `GEBUEHREN` | `fee` |
| `IMP` / `IMPOT` / `STEUER` | `tax` |
| `DIV` | `dividend` |
| `TIMBRE` / `UMSATZABGABE` / `STEMPEL` / `STAMP` (anywhere in the narrative) | `tax` |
| (other) | `deposit` / `withdrawal` per sign |

## 7. Transaction text columns

Gold's `transactions.description`, `counterparty` and
`provider_category` (gold migration 0038) are a projection of text
only. Each emitter settles ids, dates, accounts, instruments, kinds,
signs and amounts first and never reads the text it then attaches,
so the text columns cannot move a row's kind or amount
(`text_columns_test.go` pins the non-text columns byte-for-byte
against the pre-projection output). Which silver field feeds which
column depends on the era a row comes from:

| Era (silver rows) | `counterparty` | `provider_category` | `description` |
| --- | --- | --- | --- |
| Web CSV feed — `ubs-web.transactions` rows without a `payload.source` marker (`Description1/2/3` in the payload) | the `counterparty` column: the first `;`-segment of `Description1` — the payee, or the security caption on a securities row — kept as promoted except where it is not a party: a booking type the bank filed without a payee is refused, and on a charge the bank bills for itself (custody, advice, safe box, the service-price close, an interest calculation, the mandate management charge and the pair that corrects one — named in the booking type or, on a row whose booking-type column is empty or holds a reference, as the narrative's first segment) the counterparty is the bank; a depositary's pass-through and a third-party charge keep the promoted text | the booking type: `description_kind` (`Description2`, the bank's booking-kind label — `Dividend`, `e-banking payment order`, …; a `;Reversal` suffix stays) less the payer's message. The export puts a message typed on the order *ahead* of the type (`THANKS; e-banking payment order`), so the column is split at its last `; ` and the trailing part is the type; a column without the separator is the type verbatim. A **card-booked** entry puts the card's number and expiry in that leading slot instead (`<number>-<check> MM/YY; ATM Withdrawal`), which is the bank's reference rather than the payer's words — recognised whole and dropped, so it becomes no memo (§10.7) | the `Description1` caption with its ISIN tail stripped — unchanged, gold's name lookups key on it. Only when there is no caption: the booking type, then `Description3`. The payer's message, when there is one, is emitted apart as the change's memo, which gold stores last, behind the memo separator |
| Web PDF backfill — `payload.source = "account_statement_pdf"` | the `counterparty` column: the first statement continuation line — none when that line is the statement's turnover-total line (below), and the bank on the booking types it bills for itself | `description_kind` verbatim: the printed booking type (`E-BANKING PAYMENT ORDER`, `FEES`, …) | the booking type, then every `payload.continuation` line in order, less the turnover-total line |
| PSN MT940 — `ubs-psn.events` of kind `cash_movement` | the bank, when the `:61:` type is `NCHG` or `NCOM` — a charge or commission the bank levies for itself — and never on a reversal; otherwise none: the `:86:` narrative carries no structured payee and none is parsed out of the free text | `payload.txn_type` verbatim: the `:61:` transaction type code (`NTRF`, `NMSC`, …) | the `:86:` narrative (`payload.narrative`), line by line |
| PSN MT515 — `trade_confirmation` | — | — | `payload.security_name` |
| PSN MT564/566/568 — `corporate_action_*` | — | — | `payload.caev`, the ISO 15022 event code |

Composition (`silver.JoinText`): the parts in the order listed,
each trimmed, empty parts dropped, joined with `"; "` — the separator
UBS itself puts between the lines of a `Description1`. A row whose
only text is a bare booking code therefore reaches gold as that code,
and a row with no text at all stays NULL. Nothing is inferred,
expanded or paraphrased. A payer's message is not one of the parts:
the adapter emits it as the change's `Memo`, and the gold writer
stores it after the whole composed narrative behind
`canonical.DescriptionMemoSeparator` (a spaced em dash), so the payee
keeps leading, and the merchant signature stops at the separator
(SPENDING.md §4) — a payment is keyed the same with or without a
message, and a row without one projects byte for byte as it did before
the split. A reference-led order (`<reference>; order`) splits the
same way — the reference is the memo, `order` the type — so every such
row without a caption shares the key `ORDER`, which candidacy refuses
as nothing but the bank's own filing (SPENDING.md §5).

A statement's period summary is not a booking. Before its closing
balance an Account Statement prints `Turnover total <debits>
<credits>`, a line without a date, which the collector's parser
attaches to the booking that precedes it — at a period close the
service-price or interest line (`BALANCE CLOSING OF SERVICE PRICES`,
`INTEREST CALCULATION BALANCE`), so that row reaches silver with the
totals as its only narrative segment and a figure of `0.00`. The
adapter drops such a row — amount exactly zero and no narrative but
the turnover-total line (`isStatementSummary`) — and reports the
count on the log, since the adapter interface carries no per-source
diagnostics; the CSV feed's zero-amount `Balance closing of service
prices` row, the same marker printed without a booking type, is
dropped and counted the same way (`isServicePriceClose`). When the same booking types carry a non-zero figure the
row is a real fee or interest booking and is kept, but its narrative
is still the period's totals, not the booking's own text: the
description is the booking type alone, the counterparty is the
bank (both types are charges for its own services), and the provider
tier places the row from the type
(`BANK_FEES_*`). The line is stripped from every statement-era
narrative it attaches to (`bookingLines`), whichever booking precedes
it — behind an ordinary payment order it is the trailing segment
after the payee, and the payee stays. The test for the line
(`isTurnoverTotalLine`) is the two words, any case, followed by
figures and nothing else. The mobile-payment rail's rows compose
like any other statement-era booking — `PAYMENT UBS TWINT;
<payee>; <street>; <town>` and `DEBIT UBS TWINT; <person>; <phone
number>; <reference>` — so the counterparty is the payee and the
merchant signature (SPENDING.md §4) is built from it: the
description leads with the booking type, not with the
counterparty's tokens, so the signature never reaches the
phone-number segment.

### The era text fold

The two feeds record the same bookings, and the era cut (§1) gives an
overlapping one to the MT940 row. That row often says less than the
export's: the `:86:` narrative reduces to the bank's own code, the
`:61:` type code is the whole provider filing, and MT940 carries no
structured payee at all — a narrative no downstream tier can place,
where the export's row for the same entry names the payee, the printed
booking type and the detail line.

So the PSN transaction stream is wrapped in a fold
(`psnWebTextFoldStream`) that fills such a column from the export's
record of the same entry. The key is the bank's own number for the
booking: the account statement's "Transaction no.", which `ubs-web`
silver uses as its `transaction_external_id`, and which the `:61:`
line repeats as its bank reference (`payload.bank_ref`) — paired with
the account, because UBS stamps both legs of an inter-account transfer
with one number and the two legs have different payees to state.

That one number is also what lets gold re-pair the two legs at all,
which is why the adapter now writes it under `payload.bank_ref` for the
WEB eras too rather than leaving it readable only as half of a
composed id (§7, *The bank reference, across both feeds*).

Per column, and only downward: a column that is empty or a bare code
(no separator, at most a few alphanumerics, a trailing MT940
subfield marker discounted — `isCodeOnly`) takes the
export's value where the export's is not itself one; a column that
already says something keeps it. Nothing else moves — the amount, the
value date, the kind and the id are the MT940 row's, byte for byte,
and no row is added or dropped, so a false match would cost one wrong
narrative rather than a duplicated or vanished booking. The payer's
message does not travel with it: it is the payer's words about the
entry, not the bank's record of whom it paid.

Because the fold changes what `Normalize` is handed, the rows it
reaches are re-keyed, which is what `SignatureVersion` 7 records
(SPENDING.md §4).

### The era fold

Three eras record the cash ledger — the statement reconstructions
(`ubs-web` migration 0002, ids prefixed `stmt:`), the account-statement
export (ids are UBS's own "Transaction no.") and the MT940 feed (ids
prefixed `mt940:`). Their coverage overlaps in time and their id spaces
are disjoint, so an entry the archive printed and the export or the feed
also carried reaches gold **twice** unless something matches the two
copies on the booking itself. Nothing in silver does: the statement
loader dedups only within the archive, and the hard cut above arbitrates
web against PSN, not one web era against the other.

The era fold (`buildEraFold`, web side; the carry onto a surviving MT940
row in `psnStatementCarryStream`) is that match. Its key is the booking's
own facts, four components and nothing else — each taken from **the
adapter's own projection of the row**, never re-read from the silver
columns:

| Key component | Web eras (`webProjectedNet`) | Feed era (`buildTransaction`) |
| --- | --- | --- |
| Account | `transactions.account_external_id` | `payload.account`, else `events.account_external_id` |
| Value day | `value_date`, floored to UTC midnight | `events.timestamp`, floored to UTC midnight |
| Signed amount | the projected `net_amount`, rounded to the minor unit | the projected `net_amount`, rounded the same way |
| Currency | `currency_iso`, trimmed and upper-cased | `payload.funds`, else `events.currency_iso`, with the same `XXX` fallback the builder stamps |

Reading the amount off the projection rather than off the columns is
load-bearing, because **the two web eras do not write those columns to
the same convention.** A statement reconstruction carries the figure a
statement *prints*, and a statement prints a debit as a positive figure in
its debit column; the export carries the sheet's own cell, which already
states the direction in its sign. `amount_credit − amount_debit` therefore
comes out with opposite signs for one booking, and a key built on it pairs
only the rows where the conventions happen to agree.

What both eras do agree on is *which column* carries the figure, and that
is what `webKind` reads for direction. So the projection takes the
direction from the kind and the magnitude from the figure
(`canonical.ApplyCanonicalSign`), and the two eras land on the same signed
number. The magnitude alone would not do: a booking and the bank's
correction of it are equal and opposite on one day, and folding those
would delete a real entry — the signed key is what keeps them apart, and
a reversal row (`<base>;Reversal`) keeps its own record's sign for the
same reason. A kind with no pinned direction (interest, fx, `other`)
passes the source's sign through, so there the two eras can still disagree
and such a pair simply does not fold: the fold never guesses.

Nothing about the narrative enters the key — the same entry is worded
differently in each era by construction — and a zero amount is excluded,
because it names no sum and a day can carry several unrelated
zero-amount period-close lines. The value day survives the same scrutiny:
all three eras store a UTC-midnight value date, so the flooring is a
safety net rather than a correction.

**The statement copy is the one dropped, always.** The export and the
feed are the bank's own machine-readable record of the entry; the
statement row is reconstructed from a printed document, one parse
further from the bank, and it is the era whose amounts, dates and
columns were recovered by a layout parser rather than read from a
field. Which copy survives is therefore an era-level rule, not a
per-row judgement, and the surviving row's id, account, amount, value
date and kind are untouched — the fold removes a row, it never edits
one.

**Two rows of the same era are never folded.** Two identical payments on
one day are an ordinary thing for a ledger to hold, and within one era
they carry distinct ids because they are distinct bookings; only the
cross-era signature says "recorded twice". Pairing is 1:1 and
deterministic (ids sorted, export rows offered before feed rows), so a
day holding two statement copies and one export row folds exactly one of
them and leaves the other standing.

What the dropped copy said is not lost. Per column and only downward, by
the same `richerText` rule as the text fold above, the statement's
reading of the entry fills a column the survivor left empty or as a bare
code; a column that already says something keeps it. Where the survivor
is an export row the carry happens as it is emitted; where it is an MT940
row it rides `psnStatementCarryStream`, applied outside
`psnWebTextFoldStream` so the export's own record of the entry fills a
bare column first.

Two things downstream of the fold follow from it. A folded row is out of
the ledger, so it is also out of the same-day offset veto's universe
(`buildSameDayOffsetVeto`) — otherwise one booking could consume two
mirrors. And returns move where a folded row was itself carrying an
external deposit or withdrawal: that flow was counted twice and is now
counted once, which is the correction, not a side effect. The fold
adds no flow and changes no amount, so nothing else in the returns path
is touched.

Because the fold changes both which rows `Normalize` is handed and what
some of them say, the survivors it gives a payee to are re-keyed, which
is what `SignatureVersion` 8 records (SPENDING.md §4).

The adapter logs the number of statement rows it folded on each load,
alongside the dropped statement summary rows (§7).

The counterparty is silver's promoted column rather than a fresh
read of the payload because it is the collector's stated extraction
(`Description1`'s first segment; the first continuation line),
populated on essentially every row, and recomputing it here would
create a second source of truth. It feeds gold's merchant signature
as promoted — replaced only where the promoted text is not a party: a
booking type (refused, SignatureVersion 9) or the reference or product
name on one of the bank's own charges (the bank, SignatureVersion 11)
— so its format is part of this contract: a change to what the
collector promotes re-keys merchants. The booking-kind label is
the provider category because it is the nearest thing a bank
statement has to a card issuer's category — the bank's own filing of
the entry, verbatim and un-normalised (the CSV and PDF vocabularies
differ in case; PSN's is a code). The spending provider tier
translates the types whose meaning is unambiguous across all three
spellings — fees and charges, ATM cash, FX between the holder's own
currency accounts, bills paid to a card — and treats every other type
as a payment rail that says nothing about what was bought
(SPENDING.md §3, `internal/spending/providermap.go`). Of the FX types
only the MT940 `NFEX` shape ever reaches that tier: this adapter
classifies the web and PDF eras' FX bookings (`FOREX SALE`, `Purchase
FX Spot`, `Sale from FX Swap`, …) as `fx` kinds, which the spending
population excludes by kind, so the map's entries for those spellings
state their meaning without placing anything.

### The settlement fold

Two PSN rails record one securities trade: the MT515 confirmation of the
trade itself and the MT940 `:61:` line for the cash leg settling it, the
latter typed `NSEC` and narrated with a bare booking code. They share no
id, so left alone one trade is two rows and every count, turnover and
per-instrument total over the ledger is doubled.

`buildSettlementFold` (`transactions.go`) is the match, and its key is
the settlement the two rails agree on: the cash account, the currency,
the **unsigned** figure, and the settlement day — `:98A::SETT//` on the
confirmation, the value date on the statement line. Unsigned because the
rails state direction differently, the confirmation in its side and the
statement in its debit/credit mark; a key that took one rail's word on
the sign would pair nothing. It reads the whole silver, unwindowed, for
`buildEraFold`'s reason: whether a booking is recorded twice depends on
silver's contents, never on which slice of time a load covers.

**The confirmation is the copy that survives**, and that is a rail-level
rule rather than a per-row judgement: it carries the ISIN, the quantity,
the price and the side, where the statement line carries a booking code
and names no security at all. Only a line the bank itself typed `NSEC`
is eligible, so an ordinary payment that happens to match a trade's
account, day and figure stays. A trade settling on a cash account the MT940
feed does not deliver keeps its confirmation, which is why the
confirmation is also the rail that reads completely.

The confirmation names its cash account in the bank's INTERNAL form
(`:97A::CASH//`), which is not the IBAN the account registry and every
other rail are keyed by; `cashAccountIBANs` resolves it from the
master-data feed, which states both. Without that the trade reaches
gold attached to an account nothing else records — no kind, no
portfolio, and outside every account-scoped filter.

### The seam

The hard cut is placed at the first PSN **dump**, because that is the
day PSN's coverage becomes complete: before it the MT940 feed holds only
part of the ledger, so a cut placed earlier would drop web bookings PSN
never carried. But that first dump's statements reach back over the days
before it, leaving a short window where both feeds hold an entry and
neither side's window excludes it.

`buildSeamBankRefs` closes it on the bank's own number for the entry —
the export prints it as "Transaction no." and the `:61:` line repeats it
verbatim — paired with the account, because an inter-account transfer's
two legs share the reference and are two bookings. That is an exact
identity rather than a signature over amounts, the same one
`psnWebTextFoldStream` already trusts to carry text. **The web copy is
the one dropped**, matching what the cut does on every later day: the
MT940 row reaches gold and the export's text folds onto it.

### The accounts the feed does not speak for

The MT940 feed is delivered per account, and a relationship's delivery
need not reach every account it holds: a current account is typically
in scope, the cash accounts behind a managed portfolio — one per
currency it trades in — often are not. The adapter reads which
accounts the feed has ever carried a movement for, and from when
(`psnCashCoverage`), and three decisions hang on it.

**The hard cut is per account.** The cut at PSN start (above) yields a
web row to the feed only where the feed holds the account's bookings at
all: on an account the feed never reaches, the export is the record on
every day, before the relationship's PSN start and after it. Without
that, the cut dropped the export's rows for every account the delivery
leaves out the moment the feed began, and every transfer into such an
account from a covered one reached gold one-legged: an unpaired outflow
in the cash flow statement, and owner capital leaving in the returns. Where the feed reached an account
later than the relationship's start, the account's first statement is
its cut instead: the export keeps the days before, the feed the days
from it on, and the seam drops what the first statement reaches back
over.

**A conversion the feed describes gets its counter-leg.** A currency
conversion between two own accounts is one booking on each. Where the
feed speaks for one side alone, that side's `:86:` narrative still
states the whole movement — the beneficiary is the holder, and the
`/OCMT/` subfield names the currency and the figure the other account
booked. The other account follows from that figure's currency, because
a managed portfolio holds one cash account per currency and the paying
account says which portfolio the conversion stayed inside
(`portfolioCashAccounts`, the same index §11 settles trades through).
So the adapter books the other leg itself (`conversionMirrors`): on
that account, for the stated figure in the stated currency, on the
same value day, in the opposite direction, under the id
`mirror:<event id>`. The paying row is stamped with the counter
account and the stated leg under the keys the export era already
writes for the same facts, and the mirror carries the paying account
as its counter account and the bank's reference for the entry, so the
two pair by every road the cash flow statement has (docs/CASHFLOW.md
§4) and the veto's conversion phase demotes them together for the
returns engine — the feed row's stated leg is what `vetoConversions`
pairs on, exactly as a statement-era `CCY amount Rate` line is.

The mirror yields to any record of the booking itself. Where the feed
speaks for the other account on that day, nothing is booked — its own
statement carries the leg. Where the export carries the booking (an
account outside the MT940 delivery but inside the export's), the
export's row is the one gold holds and the mirror is withheld
(`buildSameDayOffsetVeto`), while the feed row still names the account.
A statement copy reconstructed from a later annual statement folds onto
the mirror as onto any feed row (the era fold, above). A payment
abroad in a foreign currency carries the same subfield and names the
payee, not the holder, and books nothing; two accounts of one currency
in one portfolio are refused rather than guessed at.

**The cash a corporate action paid is booked from its confirmation.** A
dividend reaches PSN twice: the MT566 confirmation on the custody
account, and the MT940 line crediting the cash account. Where the feed
speaks for the cash account, the line is the row and the confirmation
stands beside it as a `corporate_action` marker with no amount. Where
it does not, the confirmation is the only record, and its `CASHMOVE`
block states what the line would have — the cash account
(`:97A::CASH//`, in the bank's internal form, resolved through
`cashAccountIBANs`), the amount posted (`:19B::PSTA//`), the gross and
the tax withheld, and the payment day (`:98A::PAYD//`). The adapter
books that leg (`corporateActionCashLegs`) as `<event id>:cash`, a
`dividend` for `DVCA` and `interest` for `INTR`, with the gross and the
tax in its payload. Only a cash option credited (`CAOP CASH`,
`CRDB CRED`); a capital call confirmed as `OTHR`, an optional dividend
taken in shares, an exchange — anything else — is left to the rails
that already carry it. Without this, a mandate's foreign-currency
income existed only in its balances, and reached the income statement,
if at all, as the "other income" its later conversion into the covered
account was read as.

What the three leave: a conversion between two accounts the feed speaks
for is two feed rows that share no reference, and the cash flow
statement's amount join cannot cross currencies; the veto pairs them
for returns, and the far account of each is still the matcher's
affair (§12).

### The counter account, across both eras

Two of the adapter's readers ask the same question — "does this row's
other side belong to this relationship?" — and until both eras could
answer it, only one was asked.

The **statement era** carries the counter account in a field of its
own: the collector's PDF parser writes `counter_account` into the
row's payload, reading it off the booking's continuation lines (a full
CH-IBAN, or a UBS mortgage account stamp). `pdfCashIsExternal` has
always used it — a counter account the relationship owns demotes the
row to internal, which is rule 2 of its decision order and the whole
reason `buildOwnIBANSet` exists.

The **export era** states the same fact in free text. The CSV writes a
three-part narrative into `Description3`:

```
Reason for payment: <purpose>; Account no. IBAN: <iban>; Transaction no. <n>
```

`counterAccountFromNarrative` lifts the IBAN out of it — the IBAN
only, never the purpose or the payee, because those name a person and
this value is compared against account ids. It is written into the
emitted payload under the SAME key the statement era uses, so every
consumer reads one field whichever feed produced the row.

Three consumers follow from that:

- **Returns.** The own-counter demotion now runs on both eras. It is
  DEMOTE-ONLY, which is the direction the conduit model insists on: a
  known own counter can take a row out of the flow series, never put
  one in. Before this, a wire between two of the holder's own accounts
  counted as owner capital leaving the bank whenever it came from the
  export feed.
- **The far account.** gold keeps the payload verbatim, so the
  enrichment pass can resolve the stated counter account against the
  accounts gold holds and record where an own-account move went even
  where the far side contributes no transactions to pair with
  (docs/CASHFLOW.md §4). It places no verdict — only the destination.
- **The far account again, by identity.** See below: the same bargain,
  struck a second time for a different fact.

### The bank reference, across both feeds

The counter account answers "where did it go". The bank's own
transaction number answers something stronger — "these two rows are one
movement" — and UBS states it in both feeds, in two places that look
nothing alike.

The **MT940 feed** writes it into the `:61:` account-servicing-institution
reference, which the collector keeps as `payload.bank_ref`. The **web
feeds** put it in the id itself: a web row's silver
`transaction_external_id` IS the "Transaction no." the statement prints,
which is exactly why that silver's transactions primary key is compound
— UBS stamps the SAME number on both sides of a move between two
accounts of one relationship, so the number alone cannot key a row.

Gold's id for a web row is the per-leg composition (`<number>@<account>`),
so the number reaches gold already fused to something else. The adapter
therefore splices it out into `payload.bank_ref`, under the same key the
MT940 feed uses and by the same rule the counter account follows: one
key, whichever feed wrote the row, and a derived value never displaces a
parsed one.

Statement-era rows are the exception and are left unstamped. Their ids
are the collector's own content hash of a printed row (`stmt:`), not
anything the bank wrote, and a reference that names nothing has no
business being offered as one.

The consumer is the internal-transfer matcher, which pairs on it ahead
of amounts and therefore reaches the one shape amounts can never reach:
a conversion between two of the relationship's own accounts, whose two
legs carry different figures in different currencies (docs/SPENDING.md
§3, *The reference road*). The reference is trusted only where the
source minted it on exactly two rows — UBS books a transfer's charge
under the transfer's own reference, so a third row under one number is
an ordinary thing and refusing it is not a loss.

**This lands on already-loaded rows only after a `reload`.** The value
travels in the payload, and an incremental load re-projects only the
window it touches.

### The other leg, where the statement describes it

A reference names the movement; a counter account names the far
account. There is a third thing a statement says, and it is the only
one that reaches a booking carrying no reference at all: on one of the
two rows it writes what the OTHER row holds.

```
CCY 1 234.56 Rate 1.234567
```

`counterLegFromNarrative` lifts the currency and the figure out of
that line — anchored whole, and on the rate that closes it, because
the amount separates thousands with a space and a line of running text
must not be read as one. Both values go into the payload together
(`counter_currency`, `counter_amount`) or neither does: a currency
with no figure names no leg.

It is a weaker claim than either sibling, and it is used accordingly.
A reference is READ; a description must be MATCHED against a row that
answers to it, and two conversions of the same size on one day would
answer equally. So both consumers demand that exactly one row of the
source answer the description and exactly one row make it, and refuse
outright otherwise — the matcher's stated-counter phase
(docs/SPENDING.md §3) and the offset veto's conversion phase, below.

What it buys is the shape nothing else reaches: a conversion between
two accounts of one relationship, whose two legs carry different
figures in different currencies and, in the statement era, share no
reference. Both roads are blind to it and the description is the only
link the two rows have.

**The veto's conversion phase.** The same-day offset veto buckets by
(day, currency) and matches equal, opposite amounts, so neither of its
phases can see such a pair — its own note about FX legs says as much.
That mattered more than it looked: `pdfCashIsExternal` reads a
statement-era arrival credit as an interbank arrival on its booking
type alone, because the intra-relationship shapes that booking could
smuggle in are supposed to be peeled off by the veto. A conversion
between own accounts is such a shape, and nothing peeled it — the
paying leg was demoted by the gate's conservative default while its
receiving twin was promoted to external. That is a one-sided demotion,
the exact failure the veto exists to prevent, and it fabricated an
arrival of owner capital that the returns engine then counted.
`vetoConversions` closes it, demoting both legs or neither.

### The instrument, across both eras

The same bargain once more, for what a trade traded.

The **export era** states an ISIN in `Description1`, which
`projectWebTxText` puts straight on `instrument_external_id`.

The **statement era** states no identifier at all — it names the
security in free text. But the text closes with the Swiss **valor**,
which identifies one security line, so the match is an identity and
not a name guess. The collector's PDF parser promotes it beside
`counter_account` (`security_valor`), and the adapter resolves it
against `instrumentValorIndex`, built from the instruments PSN
describes by two roads: `payload.InstrIdtfr.Valor` where the feed
carries the identifier object, and **the ISIN itself** for a Swiss
line — a `CH` ISIN is `CH` plus the valor zero-padded to nine digits
plus a check digit — which reaches the lines that omit the object. A
valor naming two instruments is dropped rather than resolved.

A stated identifier outranks a looked-up one, so the export era keeps
what it already resolved and never consults the index.

Where the lookup finds nothing, two weaker claims follow, in order:

- The valor goes on `instrument_hint` (DESIGN.md §10.8), which is
  what a `transaction_instruments` entry is keyed by.
- `unlinkedSecurityTaxonomy` reads what KIND of thing the row traded
  off the booking type, and failing that the narrative — see §4's
  sibling table in docs/TAXONOMY.md §6. On buys and sells only: a cash
  movement whose narrative happens to carry one of those tokens traded
  no security. Nothing is stamped beside a RESOLVED instrument; the
  instrument's own row answers, and a copy would only go stale.

## 8. Change number

`LatestChangeNumber = MAX(dump_runs.snapshot_at)`, or `-1` if
`dump_runs` is empty.

## 9. Historical (PDF-reconstructed) data — ubs-web migration 0002

`ubs-web` migration 0002 added two parallel tables built
from the eDocuments PDF archive:

| Silver table | Source PDF | Cadence | Gold target |
| --- | --- | --- | --- |
| `historical_position_snapshots` | "Statement of Assets" | Quarterly | `positions` (securities) — cash rows skipped, see below |
| `historical_cash_balances` | "Account Statement" | Monthly | `cash_balances` (opening + closing) |

These predate the PSN feed's go-live and complement the intra-day
live web positions. The adapter emits them as a separate stream
(`webReader.snapshotsHistorical`) that runs before the live
overlap stream and PSN stream so chronologically the gold
`positions` and `cash_balances` tables fill in oldest-first.

### Security positions

`historical_position_snapshots` rows where `instrument_isin IS NOT
NULL`. UBS doesn't surface the safekeeping account reliably in
the PDF text, so silver leaves `account_external_id = ''`. The
adapter attaches each security position to the per-portfolio
overlay account (`'<portfolio>:overlay'`, `account_kind=overlay`)
that PSN already uses for forward contracts — preserving the
invariant that every gold `positions` row is owned by an
`accounts` row.

| Silver column | Gold mapping |
| --- | --- |
| `as_of_date` | `positions.snapshot_at` |
| `portfolio_external_id` | `accounts.portfolio_external_id` (BBBBAAAAAAAANN form, 16 chars including the leading-zero branch prefix, matches PSN) |
| `instrument_isin` | `positions.instrument_external_id`, `instruments.isin` |
| `currency_iso` | `instruments.currency` |
| `units` | `positions.quantity` |
| `market_value` | `positions.market_value` (in `market_value_currency`, typically portfolio base) |
| `cost_price * units` | `positions.book_value` |
| `accrued_interest` | `positions.accrued_interest` |
| `description` | `instruments.name` |
| `sector` | (kept in payload only) |

The `(asset_class, vehicle)` pair for historical rows comes from the
description-template classifier (`taxonomyPairForWebDescription`) —
the PDFs carry no CFI/UAC code, but UBS generates their descriptions
from a fixed per-instrument-type vocabulary, so the leading template
words are a reliable signal: "Reg.shs …"/"Shs …" and depository
receipts / participation certificates → `(public_equity, stock)`,
ETF umbrellas → `(RefineETFExposure, etf)`, SICAVs / funds →
`(…, fund)` (money-market / infrastructure / private-equity names
sharpen the exposure), "Actively Managed Certificate" →
`structured_product`, precious-metals / gold-bar lines →
`(metal, physical)`. Unmatched descriptions keep `(other, other)`.
When a later PSN snapshot contains the same ISIN, the per-column
upsert guard overwrites both columns with the CFI-derived pair — the
description read only ever decides for instruments that never made
it into PSN.

### Cash balances

`historical_cash_balances` is the richer monthly source.
`historical_position_snapshots` cash rows (instrument_isin NULL)
are **skipped** to avoid colliding with the cash_balances PK —
their quarter-end timestamps would land on the same
`(account, currency, balance_kind)` tuple as the matching month-
end row from `historical_cash_balances`.

| Silver column | Gold mapping |
| --- | --- |
| `period_start`, `opening_balance` | `cash_balances` row, `balance_kind=opening`, `snapshot_at=period_start` |
| `period_end`, `closing_balance` | `cash_balances` row, `balance_kind=closing`, `snapshot_at=period_end` |
| `account_external_id` | `cash_balances.account_external_id`; also emits an `accounts` row (`kind=cash`) once per period |
| `total_debits`, `total_credits` | kept in payload, not projected |

Each row produces zero, one, or two `cash_balances` writes
depending on which of `opening_balance` / `closing_balance` are
non-NULL. Rows with NULL on a side (closed-account statements,
mid-period exports that haven't crystallised yet) skip that
side rather than coercing to 0.0 — the silver loader leaves NULL
distinguishable from a real zero-flow month.

### Window semantics

The webReader's `ChangeWindow` extends `Start` back to
`MIN(historical times)` whenever there's any new live content (a
new `dump_runs` row). This guarantees the loader's window-DELETE
covers any existing historical gold rows before they're
re-inserted, so a reload never collides on the gold PK. The
`NewChangeNumber` stays a live-time concept — when no new live
content has arrived, the load is a no-op even with historical
present in silver.

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
matcher pairs with the account's deposit; as a purchase it paired with
nothing and stood in the statement as consumption.

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
change to get this — it needs only to emit `AccountKindCard`, which
§10.1's table does. A card emits no positions either, so nothing reaches
the instrument-taxonomy rollups; its balance surfaces through
`report_cash` exactly as chase's does, which is what keeps it in net
worth.

### 10.5 Text columns

Extending §7's table, for card rows:

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

Gold files a card bill paid from a cash account as `card_spend` — a
placeholder meaning "real consumption on a card this deployment does not
itemise" — and the internal-transfer matcher replaces that verdict once
the card's own settlement leg is in gold. Collecting UBS cards is what
makes the second half happen here, and it needs no new rule: the matcher
outranks the rule tier, so the verdict flips on the next pass from
correct projection alone. The bill leaves the spending base and the
card's own purchases carry the spending instead, categorised for free by
the provider tier from their MCC descriptions.

Both bill shapes pair: the payment order, whose counterparty is the
bank's own name with `C/O UBS CARD CENTER` behind it in the description,
and the LSV direct debit, whose counterparty is the mandate notice. The
built-in rule reads the raw narrative fields as well as the signature,
which is what finds the creditor in either.

**It does not pair across currencies, and there it double-counts.** The
matcher's amount pass partitions candidates by native currency and
cannot pair across two — converting them would make the same movement
pair differently per report currency. Its reference pass can cross that
partition, but not here: the card ledger mints its own ids, so the
bank-side payment order and the card's record of being settled share no
transaction number and no reference joins them. A relationship can hold
cards in several currencies, so a card billed in one and settled from an
account in another leaves both legs one-legged: the card's purchases are
itemised *and* its bill stays in the base as `card_spend`, counting the
same spending twice.

The issuer entry is deliberately **not** narrowed to avoid this. The
rule tier reads narratives, not the account graph, and the alternative —
suppressing `card_spend` whenever the source holds any card account —
would delete a genuine bill for a card that is not collected, which is
the failure mode `card_spend` exists to prevent and the one that is
invisible in a report. Over-counting is visible; under-counting is not.

The shape is surfaced rather than left to be discovered: `wealthdb
categorize` lists unmatched opposite-sign legs that differ only in
currency as cross-currency near-pairs. The correction is a pin, a config
rule, or a `match` line in the transfer-override ledger — which asserts
ahead of the currency partition and so can state the pair outright
(SPENDING.md §3).

### 10.7 The card reference on a cash-account row

A card transaction booked on the *cash* account — a cash-machine
withdrawal, a debit-card purchase — carries the card's number and expiry
ahead of its booking type in the same slot a payer's typed message uses:

    <number>-<check> MM/YY; ATM Withdrawal

The split at the last `; ` reads the type correctly either way, so these
rows have always categorised right. What was wrong is where the leading
part went: it became the row's **memo**, which is defined as the payer's
own words — it is shown as such, and a config rule may key on it
(SPENDING.md §3). `cardReferenceRe` recognises the reference whole and
yields no memo for it.

The match is deliberately narrow — the check-digit suffix *and* the
expiry together — so a typed message that merely opens with digits is
still a message. Nothing else moves: the booking type is unchanged, so
the provider tier places the row exactly as before, and the merchant
signature is unchanged too, because a signature reads the description
only up to the memo separator and the part before it is untouched. No
`SignatureVersion` bump.

The raw column survives whole in the row's payload, so the reference is
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
not in it. Measured against the confirmations that carry both, the
difference is a few tenths of a percent, so a coverage gap this rail
closes closes to within the fees rather than exactly.

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

## 12. Open questions

- **A conversion between two covered accounts.** Both legs are feed
  rows, the `/OCMT/` subfield on the paying one states the other, and
  the veto's conversion phase pairs them for returns — but neither row
  names the other's account, so the cash flow statement's far account
  for the pair rests on the matcher, whose amount join cannot cross
  currencies. Stamping both rows with the counter account the stated
  leg identifies (the receiving row is the one booking the stated
  figure that day) would close it by the same road the mirror uses.
- **MT568 vs MT566 collapsing.** Both carry corporate-action info;
  MT568 is narrative supplementing MT566. The adapter currently
  emits both as separate `corporate_action` events. Consider
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
