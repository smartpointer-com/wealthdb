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
| `cash_balances` | `cash_balances` | Direct one-to-one; UBS `balance_kind` enum carries over. |
| `pending_securities` | — | Most rows are "no activity" markers (`ACTI//N`). Deferred. |
| `fx_rates` | `fx_rates` | Base currency is CHF in the current PSN setup. |
| `portfolio_performance` | — | Monthly TDPOPF analytics. Deferred. |
| `cash_account_pricing` | — | TDCAPI interest-rate config. Deferred. |
| `forward_contracts` | `positions` — `(foreign_exchange, forward)` | `contract_external_id` is `position_key`. |
| `option_contracts` | `positions` — `(foreign_exchange, option)` | Same. |
| `money_market_contracts` | `positions` — `(cash, time_deposit)` | Same. |
| `otc_contracts` | `positions` — `(foreign_exchange, forward)`, or `(other, other)` for a non-FX underlying | Same. |
| `events` | `transactions` | See `kind` mapping in §5. |

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
| `cash_movement` | `deposit` / `withdrawal` / `fee` / `interest` / `tax` / `dividend` | from MT940 `:86:` narrative (adapter splits — see §6); a deposit/withdrawal leg whose same-day mirror books on another own account is demoted to `other` (same-day offset veto, `buildSameDayOffsetVeto`) |
| `securities_movement` | `transfer_in` / `transfer_out` | sign-driven |
| `trade_confirmation` | `buy` or `sell` | from MT515 payload `side` |
| `fx_confirmation` | `fx` | MT300 |
| `fx_option_confirmation` | `fx` | MT305 (no dedicated option kind; the settlement is an FX cash effect) |
| `loan_deposit_confirmation` | `other` | MT320/MT330/MT350 |
| `corporate_action_notification` | `corporate_action` | MT564 |
| `corporate_action_confirmation` | `corporate_action` | MT566 |
| `corporate_action_narrative` | `corporate_action` | MT568 (narrative; may collapse with the MT566 row) |
| `securities_settlement_advice` | `transfer_in` / `transfer_out` | MT544–548 |
| `precious_metal_trade` | `buy` or `sell` | MT600/MT601 |
| `charges_advice` | `fee` | MT590/MT990 |
| `debit_credit_confirmation` | `deposit` / `withdrawal` | MT900/MT910 |

### `ubs-web` booking types

The web silver's `description_kind` — the statement PDF's printed
booking type, the CSV feed's `Description2` label — is classified
by `webKind` (`web_reader.go`), which matches the whole type
case-insensitively so the two eras' spellings (`CREDIT` /
`credit`) land together: the income and cost types (`DIVIDEND`,
`COUPON`, `CUSTODY PRICE`, `INTEREST CALCULATION BALANCE`, …) to
`dividend` / `coupon` / `interest` / `fee`, the FX types to the
`fx` kinds, the securities settlements to `buy` / `sell` by cash
direction, and the mobile-payment types to the money-moving kinds:

| Booking type (any case) | Gold `kind` |
| --- | --- |
| `PAYMENT UBS TWINT`, `DEBIT UBS TWINT` | `withdrawal` |
| `CREDIT UBS TWINT`, `REVERSAL UBS TWINT` | `deposit` |
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
The narrative parser is conservative — unmapped narratives fall
through to `deposit` or `withdrawal` based on the amount sign,
with the raw narrative preserved in payload.

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
| Web CSV feed — `ubs-web.transactions` rows without a `payload.source` marker (`Description1/2/3` in the payload) | the `counterparty` column verbatim: the first `;`-segment of `Description1` — the payee, or the security caption on a securities row | the booking type: `description_kind` (`Description2`, the bank's booking-kind label — `Dividend`, `e-banking payment order`, …; a `;Reversal` suffix stays) less the payer's message. The export puts a message typed on the order *ahead* of the type (`THANKS; e-banking payment order`), so the column is split at its last `; ` and the trailing part is the type; a column without the separator is the type verbatim | the `Description1` caption with its ISIN tail stripped — unchanged, gold's name lookups key on it. Only when there is no caption: the booking type, then `Description3`. The payer's message, when there is one, is emitted apart as the change's memo, which gold stores last, behind the memo separator |
| Web PDF backfill — `payload.source = "account_statement_pdf"` | the `counterparty` column verbatim: the first statement continuation line — none when that line is the statement's turnover-total line (below) | `description_kind` verbatim: the printed booking type (`E-BANKING PAYMENT ORDER`, `FEES`, …) | the booking type, then every `payload.continuation` line in order, less the turnover-total line |
| PSN MT940 — `ubs-psn.events` of kind `cash_movement` | never: the `:86:` narrative carries no structured payee and none is parsed out of the free text | `payload.txn_type` verbatim: the `:61:` transaction type code (`NTRF`, `NMSC`, …) | the `:86:` narrative (`payload.narrative`), line by line |
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
diagnostics. When the same booking types carry a non-zero figure the
row is a real fee or interest booking and is kept, but its narrative
is still the period's totals, not the booking's own text: the
description is the booking type alone, the counterparty is left
empty, and the provider tier places the row from the type
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

Per column, and only downward: a column that is empty or a bare code
(no separator, at most a few alphanumerics — `isCodeOnly`) takes the
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

The counterparty is silver's promoted column rather than a fresh
read of the payload because it is the collector's stated extraction
(`Description1`'s first segment; the first continuation line),
populated on essentially every row, and recomputing it here would
create a second source of truth. It feeds gold's merchant signature
verbatim, so its format is part of this contract: a change to what
the collector promotes re-keys merchants. The booking-kind label is
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

## 10. Open questions

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
