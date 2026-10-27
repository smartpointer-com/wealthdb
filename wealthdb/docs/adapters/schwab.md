# Schwab adapter

The adapter projects two Schwab silver SQLite databases into the
canonical gold schema:

- `schwab-api`: Trader API JSON. Live position and cash snapshots, and
  about two years of transactions.
- `schwab-web`: the netbanking scrape. The account list, and the
  positions, cash balances, transactions and tax lots read from
  statements, tax forms and the transaction-history export.

It implements the `silver.Adapter` / `silver.Connection` interface of
[../DESIGN.md](../DESIGN.md) §6. With both subsources configured, the
orchestrator (`merge.go`) splices them:

- The web feed backfills transactions from before the api's coverage.
- It adds statement-period positions and cash balances, which the api
  does not have.
- It adds the tax lots (§8).

§7 has the merge contract and §7.1 the account bridge. A single `path`
(`"kind": "schwab", "path": ...`, no subsources) reads an api silver
alone, without the merge layer. A source whose only subsource is
`schwab-web` reads the web silver without the api (§7.4). The web
silver names an account by the last digits of its number. With the api
beside it, the api silver maps those to an account (§7.1). Alone, the
account's full number is its id.

## 1. Silver sources

- API: [`schwab-api`](../../../collectors/schwab-api/).
  Silver schema: the collector's [migrations](../../../collectors/schwab-api/migrations).
- Web: [`schwab-web`](../../../collectors/schwab-web/).
  Silver schema: the collector's [migrations](../../../collectors/schwab-web/migrations).
- Cross-collector interop notes: [schwab-web/INTEROP.md](../../../collectors/schwab-web/INTEROP.md).

## 2. Identifier conventions

| Field | Source | Notes |
| --- | --- | --- |
| `account_external_id` | Schwab `hashValue`; with the web silver alone, the full account number (§7.4) | Opaque account hash, stable per Schwab developer app. The account number is the `display_name` and stays in the payload. |
| `instrument_external_id` | CUSIP if present, else symbol | Silver's `instrument_key`. A web ticker maps to the api's CUSIP where the api states one (§8.3). |
| `transaction_external_id` | Schwab `activityId` | Unique within Schwab. A web row's id is a synthetic SHA-256 prefix (§7.2). |
| `realized_lot_external_id` | web `closed_lots` key | A hash of `(logical_doc_key, document_kind, lot_index)`: stable across loads, one per copy of a lot. |
| `position_lots.lot_key` | web `open_lots.lot_index` | The lot's place in print order under its holding. |

## 3. Coverage matrix

| Silver table | Gold target | Notes |
| --- | --- | --- |
| `schema_meta` / `dump_runs` | meta only | Read by `Status` and `ChangeWindow` (§6). |
| `accounts` | `accounts` (kind `brokerage`) | Account hash as `account_external_id`. The optional `nickname` column becomes the gold `nickname`. |
| `user_preference` | — | Schwab-internal UI and streamer preferences. Not projected. |
| `account_balances` | `cash_balances` | The `cashBalance` of each `balance_kind` (`initial`, `current`, `projected`, `aggregated`), in USD. A row without one, or of another kind, emits nothing. |
| `positions` | `positions`; `CASH_EQUIVALENT` and `CURRENCY` rows → `cash_balances` | §4; the mark guard in §4.1; book value in §8. |
| `instruments` | `instruments.name`, and a bond's instrument | The latest description per symbol, where a position states none (§4). A bond it describes is an instrument with its name, `maturity_date` and `interest_rate`, whether or not a position holds it. |
| `open_orders` | — | Operational state, not portfolio state. Not projected. |
| `transactions` | `transactions` | §5 maps the kinds. |

The web silver's tables are in §7 and §8.

## 4. Position routing

Schwab models cash as positions with `assetType` `CASH_EQUIVALENT` or
`CURRENCY`. Gold keeps cash in `cash_balances`, so the adapter emits
such a row as a `current` `CashBalanceChange` in USD, at its
`marketValue`. A purchased money-market fund stays a position,
`(cash, fund)` (below).

Every other position lands in `positions`. `taxonomyFor` derives the
`(asset_class, vehicle)` pair from `instrument.assetType`, plus
`instrument.type` and the security name for collective vehicles:

| Schwab `assetType` | Gold `(asset_class, vehicle)` |
| --- | --- |
| `EQUITY` | `(public_equity, stock)` |
| `COLLECTIVE_INVESTMENT` with `instrument.type = EXCHANGE_TRADED_FUND` | `(etfExposure(name), etf)`. This is how the Trader API types ETFs. |
| `COLLECTIVE_INVESTMENT`, other | `(fundExposure(name), fund)` |
| `ETF` | `(etfExposure(name), etf)`. In the API's enum; position dumps use `COLLECTIVE_INVESTMENT`. |
| `MUTUAL_FUND` | `(fundExposure(name), fund)` |
| `BOND` / `FIXED_INCOME` | `(fixed_income, bond)` |
| `OPTION` | `(public_equity, option)` (equity underlying) |
| `FUTURE` | `(public_equity, future)` (equity underlying) |
| (unrecognised) | `(other, other)`; the `assetType` stays in the payload |

- `etfExposure` reads the exposure from the name with
  `silver.RefineETFExposure`: crypto → `crypto`, bullion → `metal`
  (miners funds stay `public_equity`: they hold stocks), bond and
  fixed-income → `fixed_income`, else `public_equity`. It also reads
  the bond words Schwab abbreviates (`BD`, `BND`, `TREAS`, `GOVT`,
  `INFL PROT`) as `fixed_income`.
- `fundExposure` first routes a money-market fund's name to `cash`
  (`silver.FundExposure`), then reads the rest the same way.
- Gold's class pass reads OpenFIGI's type of the security over this
  pair (§4.2).
- Config `instrument_overrides` pins a product whose name hides its
  exposure ([../DESIGN.md](../DESIGN.md) §13.8). It wins over both.

### Instrument names

`instruments.name` is the position's `instrument.description`. The
Trader API states one for `COLLECTIVE_INVESTMENT`, `MUTUAL_FUND` and
`BOND` positions, but not for `EQUITY` ones. An equity block carries
only `{assetType, cusip, symbol, netChange}`.

So the adapter falls back to the silver `instruments` table: the latest
description per symbol (`latestKnownInstrumentNames`). `schwab-api`
fills it from `/marketdata/v1/instruments` on every download unless
`--no-instruments` is given. A silver without the table leaves an
equity's name empty; its `symbol` is still set.

### 4.1. Mark guard

The api positions carry the api feed's overnight marks. A thinly traded
security's mark can be far off one night or two and right again after.
The adapter reads such a spike as a bad quote (`markGuard`,
`marks.go`):

- A position's mark is its value per unit. A mark that moves more than
  `mark_spike_pct` (10%) from the previous mark, and comes back within
  `mark_revert_pct` (2%) of that previous mark at one of the next two
  observations, is a spike.
- The observations in between take the previous mark: each keeps its
  quantity, and its `market_value` is that quantity at the previous
  mark. Its `valued_at` is the snapshot the previous mark is from. Its
  payload keeps Schwab's value as `stated_market_value`. Its book value
  is Schwab's value less the open P/L, as on any row (§8.1).
- A move that does not come back is real and stands, whatever its size
  and the vehicle. A split moves quantity and mark together and never
  comes back, so it stands too.

At the edges:

- A position's observations count in a run: the snapshots in a row that
  state it. A snapshot that leaves it out ends the run, and a spike is
  read inside one run only. An observation without a value or a
  quantity has no mark and is not counted.
- The newest observations, with nothing after them yet, stand as
  stated. The api feed's change window reaches back two snapshots before
  the first new one, so the next load corrects them if they revert.
- Options and futures are not guarded. Their marks move that far as a
  matter of course.
- The web subsource's statement positions are not guarded. They are
  period-end closes a month apart.

The guard reads every api position, so an incremental load corrects the
same rows as a reload.

The two thresholds are settings of the source, here at their defaults:

```json
{"id": "schwab", "kind": "schwab", "path": "...",
 "schwab": {"mark_spike_pct": 10, "mark_revert_pct": 2}}
```

Both are above 0, and `mark_revert_pct` is below `mark_spike_pct`. A
source of another kind refuses the object.

### 4.2. Class precedence

Three signals give an instrument its `(asset_class, vehicle)` pair. The
first that speaks wins:

1. OpenFIGI's security type, where gold holds one (`security_identities`,
   from the refdata source).
2. Schwab's own instrument type: `assetType` and `instrument.type` (§4).
3. The security name (`etfExposure`, `fundExposure`).

The adapter reads 2 and 3 at load time. OpenFIGI's type reaches gold
through another source, so gold's class pass reads it after the loads
(`gold.RefineClasses`, the adapter's `RefineClass`). The pass runs first
among the passes a load or a reload ends with. It reads the type so:

- A bond's type (`Govt`, `Corp`, `Muni`, `Mtge`, `Pool`) gives
  `(fixed_income, bond)`.
- A share's type (`Common Stock`, `Depositary Receipt`, `REIT`, …) gives
  `(public_equity, stock)`.
- A fund's type (`Mutual Fund`, `Closed-End Fund`, `ETP`, …) keeps an
  ETF or a fund as Schwab types it. `ETP` makes a fund an ETF. Any other
  instrument becomes a fund, or an ETF for `ETP`, with its exposure from
  the name. A crypto trust Schwab types as an equity reads as
  `(crypto, fund)`.
- Any other type, such as an option's or a warrant's, leaves the pair.

OpenFIGI types an ETF as `Mutual Fund`, which names no exposure. A bond
ETF's exposure still comes from its name, abbreviations included.

The pass looks the type up under the instrument's ISIN, the US ISIN of
its CUSIP, its CUSIP and its symbol, in that order. It rewrites the
instrument row and every position row of the instrument. An instrument
`instrument_overrides` pins keeps the config's pair.

## 5. `transactions.kind` mapping

The api silver's `transactions.kind` (Schwab's transaction `type`) maps
to gold's `kind` (`kindFor`):

| Schwab | Gold | Adapter notes |
| --- | --- | --- |
| `TRADE` | `buy`, `sell` or `other` | sign of `netAmount` (negative = buy); zero is `other` (see below) |
| `JOURNAL` | `tax`, `fee`, `corporate_action` or `journal` | from the description (see below) |
| `DIVIDEND_OR_INTEREST` | `dividend` or `interest` | from the description: a cash-sweep, margin or bond interest prefix, or a Treasury note or bond, is `interest` |
| `WIRE_IN` / `CASH_RECEIPT` / `ELECTRONIC_FUND`(+) | `deposit` | |
| `WIRE_OUT` / `CASH_DISBURSEMENT` / `ELECTRONIC_FUND`(−) | `withdrawal` | |
| `RECEIVE_AND_DELIVER` | `corporate_action`, `journal`, `transfer_in` or `transfer_out` | see below |
| `SMA_ADJUSTMENT`, `MEMORANDUM`, `MONEY_MARKET`, any other | `other` | the raw type stays in the payload ([../DESIGN.md](../DESIGN.md) §6.8) |

A `TRADE` with a zero `netAmount` buys and sells nothing. Schwab books
a "System transfer" this way. It restates a holding at its cost, and the
shares stay in the account. The row is `other`, and the lot engine
skips it.

A `JOURNAL` moves cash or shares inside the holder's accounts. Schwab
books some cash rows as journals too, and the adapter types them by the
words of their description (`journalShapes`):

- tax withheld from a non-resident's dividend (`NRA Tax Adj`), its
  adjustment for a prior year (`Pr Yr NRA Tax`) and a foreign tax
  (`Foreign Tax Paid`) are `tax`;
- a depositary's fee on an ADR (`ADR Mgmt Fee`) is a `fee`;
- cash in lieu of a fractional share is a `corporate_action`, as the web
  feed books it. The lot engine reads it as cash in lieu
  (`CorpCashInLieu`) in both feeds. A cash-only row without an instrument moves no
  lot.

The API often books the withholding and the depositary's fee with no
such words: the description names only the security, and the row moves
cash alone. A cash journal with an amount, whose description names no
move of cash (a sweep, a transfer of funds, a journal to or from an
account, an account number behind `TO` or `FR`), is read as one of the
two: a `fee` when the security is a
depositary receipt, a `tax` otherwise. A receipt's foreign tax then
reads as a fee; both are charges, and a refund keeps its positive sign.

Any other journal stays a `journal`.

`RECEIVE_AND_DELIVER` carries a zero `netAmount`, so the adapter types
it from the description and the security leg. Schwab books the legs of
one event on the same account at the same instant, and the adapter
reads them as a group:

- A row whose description names a split, merger, expiration, spin-off
  or name change is a `corporate_action`. So is every row booked with
  it, such as the new-share leg of a reverse split.
- Legs of one instrument whose quantities cancel move shares between
  the account's cash and margin sub-accounts. They are a `journal`.
- Any other row is a delivery: `transfer_in` for a positive quantity,
  `transfer_out` for a negative one.

**Signs.** The API signs `netAmount` from the account's side, and the
adapter keeps that sign on every kind. A row signed against its kind is
a correction, such as a dividend clawed back, and nets against the
booking it corrects.

**Instruments.** A `DIVIDEND_OR_INTEREST` row's legs are often cash
alone, so the row names its security by its description. The adapter
links it in this order:

- to the position or instrument whose description matches it, after
  both are normalised;
- for an `interest` row, to the bond the account holds or held whose
  coupon and maturity the description names, as a Treasury's coupon
  reads (`US TREASU NT 9.875%09/99UST NOTE DUE 09/09/99`). A bond's
  coupon is its `variableRate`, else its description's; its maturity
  its `maturityDate`, else its description's. A row two bonds could
  pay, or none, stays unlinked;
- else to nothing. A cash balance's interest (`SCHWAB1 INT 01/01-02/01`,
  `BANK INT 010100-020100 SCHWAB BANK`) names no bond.

A bond's instrument states that `maturityDate` and `variableRate` as
`maturity_date` and `interest_rate`, the keys the yield pass reads a
payload's terms by. Gold matches an unlinked row only to a bond the
account holds on the report's day, and reads one it cannot match as
the account's coupons (YIELD.md §4).

### 5.1 Web transactions

The web silver's `kind` is the word Schwab prints on statements and in
the transaction-history export. `webKind` (`web_reader.go`) maps it:

| Web `kind` | Gold |
| --- | --- |
| `Buy`, `Buy to Open`, `Buy to Close`, `Purchase`, `Reinvest`, `Reinvest Shares` | `buy` |
| `Sell`, `Sell to Open`, `Sell to Close`, `Sale`, `Redemption` | `sell` |
| dividend words (`Cash Dividend`, `Qualified Dividend`, `Reinvest Dividend`, …) | `dividend` |
| interest words (`Bond Interest`, `Credit Interest`, `Margin Interest`, …) | `interest` |
| `Fee`, `Service Fee`, `ADR Mgmt Fee` | `fee` |
| `Foreign Tax Paid`, `NRA Tax Adj`, `Pr Yr NRA Tax`, `Tax` | `tax` |
| `Deposit`, `MoneyLink Deposit`, `Funds Received`, `Wire Received` | `deposit` |
| `Withdrawal`, `Wire Sent` | `withdrawal` |
| `Transfer Out` | `withdrawal` for a cash distribution, else `transfer_out` |
| `Transfer In` | `transfer_in` |
| `MoneyLink Transfer`, `Transfer` | `deposit` / `withdrawal` by the amount's sign; `journal` without one |
| `Security Transfer`, `Journal`, `Journaled Shares` | `journal` |
| corporate-action words (`Split`, `Reverse Split`, `Spin-off`, `Exchange`, `Cash In Lieu`, …) | `corporate_action` |
| `Unknown` | see below |
| anything else | `other` |

- A reinvested dividend is two rows: `Reinvest Dividend` (a dividend)
  and `Reinvest Shares` (a buy). They net to zero cash.
- The undirected transfer words take their direction from the amount.
  The API books the same movements as deposits and withdrawals, so the
  kinds stay continuous across the two feeds.
- `Unknown` is the statement parser's catch-all. A funds journal in it
  (`Journaled Funds …`) takes its direction from the amount. Cash shapes
  are read by the name the statement prints first: a short sale (`sell`)
  and its cover (`buy`), a pass-through or service fee (`fee`), a
  withholding or its reclaim (`tax`), a fund's capital-gain payout or a
  dividend correction (`dividend`). Anything else in it (a share
  journal, an in-kind transfer, a corporate action, an expiry) stays
  `other`.

**Signs.** The web feeds print most figures as magnitudes, so the kind
orients the figure. A minus printed on an inflow kind is kept: the
statements print one only on a correction. The `Unknown` bucket keeps
every figure as printed, a debit in parentheses.

**Narrative.** A web row's description is the movement, then the
security (`webNarrative`). The movement is the export's `Action`, or a
statement row's `action` or `category`; `Unknown` names none. The
security alone cannot tell a dividend from the withholding taken from
it.

## 6. Change number

Each feed has its own change number. The source's is the later of the
two.

- **api:** `MAX(dump_runs.snapshot_at)`, or `-1` when `dump_runs` is
  empty.
- **web:** the later of `MAX(dump_runs.snapshot_at)` and
  `MAX(transactions.timestamp)` without the `form_1099b` rows, or `-1`
  when both are empty.

The web feed's statement-period dates (`historical_position_snapshots`,
`historical_cash_balances`) never count. They widen `Status`'s snapshot
range, and the load window once a load triggers. But `ChangeWindow`
never advances the watermark to them. A latest period end past the
latest dump and transaction would otherwise keep the source from ever
reading as up to date.

- A load triggers on a dump past the watermark, or on a transaction
  past it (web: without the `form_1099b` rows).
- The web window then widens to every statement-period date, so the
  loader's window-DELETE covers the historical gold rows before they
  are re-inserted.

Each feed is observed through its latest run. The source is observed
through the later of the api and web feeds: each reads every account
([../DESIGN.md](../DESIGN.md) §6.4).

## 7. Web subsource (schwab-web)

With the `schwab-web` subsource configured, the adapter adds what the
api silver does not have. §8 has the tax lots, open and realized.

- **Statement positions.** `historical_position_snapshots` holds the
  holdings of each statement period, one row per (`as_of_date`,
  account, `instrument_key`). The `instrument_key` is both the
  instrument id and the `position_key`. Statements carry no
  `assetType`, so the pair comes from the statement section
  (`taxonomyHistorical`):
  - `Equities` → `(public_equity, stock)`;
  - `Exchange Traded Funds` → `(etfExposure(description), etf)`;
  - `Fixed Income` → `(fixed_income, bond)`;
  - `Options` → `(public_equity, option)`;
  - any other section falls back to the key and description shapes:
    OCC symbol or CALL/PUT → `(public_equity, option)`; money-market
    name or money-fund ticker → `(cash, fund)`; CUSIP or coupon →
    `(fixed_income, bond)`; the word ETF → etf; a four-letter-plus-X
    ticker → fund; an ETF-only issuer name (iShares, SPDR, Vanguard,
    …) → etf; else `(public_equity, stock)`. An ETF or a fund reads
    Schwab's abbreviated bond words as `etfExposure` does.

  The api's classification wins on the instrument row when the same
  key appears there (per-column upsert). A statement prints a holding's
  accrued interest or declared dividend apart from its market value.
  Gold's `market_value` includes it, and `accrued_interest` says how
  much it is ([../DESIGN.md](../DESIGN.md) §7.1). The printed value
  stays in the payload as `printed_market_value`. The api positions
  state none.
- **Statement cash balances.** `historical_cash_balances` holds the
  opening and closing balance of each statement period. Opening lands
  at `period_start`, closing at `period_end`. A NULL side is skipped,
  not read as 0.
- **Transaction backfill.** The web transactions reach further back
  than the api's. The adapter emits the web rows from before each
  account's api coverage start (§7.2). The `form_1099b` rows stay out
  (§8.3).
- **Web sub-feeds.** The statements (`statement_pdf`) and the
  transaction-history export (`tx_history_json`) overlap. The
  third-party distribution letters (`third_party_distribution`) add
  transfers out. The adapter reconciles them before emitting
  ([INTEROP §8](../../../collectors/schwab-web/INTEROP.md#8-gold-layer-hand-off-the-1099-b-and-distribution-sources)):
  1. Across the export's span per account, the export is the record
     for every row that is not an external flow. A statement row in
     that span is dropped. Older statement rows stay as the deep
     backfill.
  2. A cash distribution names the counterparty, so it outranks a
     matching statement or export debit. A match is within 3 days, and
     within the larger of $1 and 0.5% of the amount. Every such debit
     is dropped.
  3. A statement external flow (deposit, withdrawal, transfer,
     journal) that matches an export one within the same tolerance is
     dropped, 1:1. A flow only one sub-feed records stays.

  A securities distribution is new data and always passes through.
- **Accounts.** The web `accounts` row adds two columns to the api's
  account by per-column upsert:
  - `nickname`, the label Schwab shows in the UI;
  - `account_registration`, the statement's registration label. It
    becomes the `account_category` and sets the tax wrapper (IRA,
    Coverdell ESA, 529, …). An unknown label leaves the wrapper unset.

### 7.1. Suffix ↔ hashValue bridge

The web silver keys an account by the 3-to-5-digit suffix Schwab shows
in the UI. The api keys it by Schwab's opaque `hashValue`. The
orchestrator builds the suffix → hashValue bridge on the first
`Snapshots` or `Transactions` call (`buildAccountBridge`).

- The api side reads each account's latest row, so a closed account
  still bridges. It compares against `accounts.account_number`, which
  the api silver promotes from `/accounts/accountNumbers`.
- The web side reads every account. The newest non-empty
  `account_number_full` (the number a statement prints, e.g.
  `1234-5678`) wins.

Each web account resolves in two tiers:

1. **Exact.** With an `account_number_full`, the full number and every
   api account number are reduced to their digits and compared.
   - The suffix is the trailing digits of the account number. A full
     number that does not end in the suffix can only be a mis-parsed
     statement header, and the bridge fails loudly.
   - Two api accounts matching one full number also fail the bridge.
   - A full number that matches no api account falls through to the
     suffix tier: the api roster may lag the statements.
2. **Suffix.** The web suffix is matched against the trailing digits of
   every api account number. Two matches fail the bridge loudly.

A web account that bridges to no api account is skipped, with all its
rows. Emitted under the raw suffix, they would be orphaned gold rows.

### 7.2. Transaction splice — hard cut, not overlap merge

The two silvers' id spaces are disjoint: the api uses Schwab's
`activityId`, the web a synthetic SHA-256 prefix
([INTEROP §2](../../../collectors/schwab-web/INTEROP.md#2-transaction-identifier-mismatch)).
A per-row match across them would be heuristic and risk counting a
transaction twice. So each account's history splits at its api
coverage start:

- the web emits the rows strictly before it;
- the api emits the rows from it on, with its real `activityId` and no
  parser approximation;
- an account with no api transactions takes every web row.

The coverage start is the account's first api row, unless that row is
a stray. The api can return one trade from months before its history
proper, then fall silent. A leading api row followed by more than 30
days of api silence, during which the web books the account as active,
is set aside (`apiCoverageStart`). The history then starts at the next
row. The web's activity is the evidence: a quiet account keeps its
first row as its start.

### 7.3. PDF sha256 churn

Schwab regenerates a statement PDF on every download: a new sha256,
the same content (INTEROP §3). The `schwab-web` silver handles it:

- every load gate keys on the logical document, not its bytes;
- the statement tables keep one row per account and period end, owned
  by the first statement that prints it (schwab-web DESIGN.md §4.4,
  §4.5).

The adapter does not dedupe further.

### 7.4. The web silver alone

A source can name the `schwab-web` subsource alone. Each account is then
keyed by its full account number, digits only, which the web silver
states for every account. That number is also its display name, and its
currency is USD, as the api would state them. An account the web silver
states no full number for has no id, and the load fails naming it.

Without the api, the source reads:

- the statement-date positions, cash balances and open lots;
- every web transaction, with no cut at an api start;
- the year-end documents' realized lots.

A ticker stays the instrument's key, since no api CUSIP resolves it, and
the statement's security name names the instrument. The source lacks the
api's daily snapshots between statements, and its ledger ends at the web
feed's last run.

A web row of an account the silver's `accounts` table does not list is
dropped, as with the api beside it. The two modes key an account
differently: the full number alone, the api's `hashValue` with the api.
Adding the api to a web-only source re-keys every account, so the source
is reloaded (`wealthdb reload`), and a config entry keyed by account id
is rewritten for the new id.

## 8. Cost basis and tax lots

Schwab's basis is the sum of a holding's tax lots, commissions
included. Every book value carries its stamp (DESIGN.md §7.4).

### 8.1. Positions

| Source | `book_value` | Stamp |
| --- | --- | --- |
| api `positions` | market value − `unrealized_gain_loss` | derived, lots, included |
| web statement holdings | `cost_basis` as printed | stated, lots, included |
| web statement holdings without `cost_basis` | market value − `unrealized_gain_loss` | derived, lots, included |

- The api's open P/L is the long side's, or the short side's when
  the position is short. A short position's basis is what the short
  sale raised. It is negative, like the position and like the basis
  the statements print for a short holding.
- An api row that is long and short at once has no basis: its P/L
  covers one side only.
- A silver from before api migration 0005 has no
  `unrealized_gain_loss`. Its positions have no basis.
- A statement holding's `acquisition_date` is its earliest lot's
  acquired date. Holdings without lots have none.

### 8.2. Open lots

`open_lots` holds the lots the statements print under each holding.
Each lot lands in `position_lots` beside its holding's position row:
same period end, bridged account and position key (the holding's
`instrument_key`).

- `lot_key` is the lot's place in print order (`lot_index`).
- `quantity` and `book_value` are as printed. A short lot prints
  both negative.
- A lot whose basis Schwab does not know ("N/A") has no book value
  and no `basis_origin`.
- `term` is `short` or `long` where the statement prints it.
  `covered` stays NULL: statements do not say.
- The payload keeps the endnote markers, the cost per share, the
  unrealized gain, the holding days and the raw line.
- A lot whose holding row is missing has no position to sit beside.
  It is dropped and counted in the load log.

### 8.3. Realized lots

`closed_lots` holds the realized lots of three year-end documents.
The adapter returns every one of them as a realized lot. They are
not cut at the api's coverage start, so every tax year reaches gold.
Three kinds of lot are dropped and counted in the load log:

- a lot of an account the bridge does not resolve (§7.1);
- a lot of a document kind gold does not know;
- a lot that states neither a tax year nor a disposal date.

The same sale appears in several documents, so one set is primary
per account and tax year:

1. The best document kind present: the 1099-B, then the Year-End
   Summary, then the Gain/Loss Report.
2. Within that kind, the latest document. A corrected 1099 repeats
   the original. A Year-End Summary can arrive twice, as its own PDF
   and inside the 1099 Composite. The document date comes from
   `logical_doc_key`. Two copies dated alike go to the greater key.
   A key whose date does not read counts as the oldest.

The figures map as printed:

- Quantity, proceeds and cost basis are magnitudes. A NULL cost
  basis stays NULL.
- The gain is signed. The 1099-B prints none, so it is NULL there.
- `Various` sets `acquired_various` and leaves the date NULL.
- The tax year is the document's, else the disposal year.
- The payload is the silver row's own, including a bond's adjusted
  basis on the Year-End Summary.

Instruments resolve the way the web transactions do: a ticker goes
through the symbol→CUSIP bridge, anything else stays as stored.

- The Year-End Summary keys a lot by CUSIP, the api's own key.
- The Gain/Loss Report keys a lot by ticker.
- Both key an option by its contract, as the statements print it.
- A 1099-B lot states only `security_name`. A plain ticker or an
  option contract is read as that key (`securityNameKey`). An option
  keeps its contract and does not resolve to its underlying: the lot
  and its gain are the contract's.
- A name of neither shape resolves to nothing. It becomes the
  `instrument_hint`, which a config link can close.

The `form_1099b` rows of the web `transactions` table do not enter
gold's transactions. A 1099-B lot is a tax record, not a cash
movement. It prints a closed short's opening premium as proceeds on
the closing date, and an order filled in several lots as several
sales. The statement and history feeds book each sale as cash once.
The `form_1099b` rows also stay out of the transaction range that
`Status` and `ChangeWindow` report, so they widen no load window.

## 9. Open questions

- **Tax withholding on dividends.** A `DIVIDEND_OR_INTEREST` row
  carries only its cash leg. The adapter books its `netAmount`, leaves
  `gross_amount` NULL, and generates no separate `tax` row for a
  withholding. Tax-lot work may need the withholding as an event of
  its own.
- **`open_orders` projection.** Not projected. A `wealthdb orders`
  subcommand would need it.
- **Cost-basis methods.** A Gain/Loss Report prints the account's
  relief method per asset class (`cost_basis_methods`, schwab-web
  DESIGN.md §9.3). Gold does not map it.
- **Explicit suffix→hashValue override.** The bridge is automatic
  only. The exact tier (§7.1) resolves a suffix collision wherever
  `account_number_full` is present. A config field such as
  `account_bridge: {<api_hash>: <web_suffix>}` would be needed only for
  a suffix collision on an account whose payload carries no full
  number. The bridge's error message suggests an override, which no
  config field provides.
