# carta adapter

Projects the **carta** silver (private-market holdings on carta.com —
see `collectors/carta/`) into canonical gold change records.
`internal/silver/carta/`.

Carta is the first wealthdb source for **non-public-market securities**:
direct private-company equity + equity-comp (shares, option grants with
strike + vesting, RSUs/RSAs, SAFEs/notes, warrants) and LP interests in
venture/PE funds. None has an ISIN/CUSIP/symbol or a quotable market price, so
the adapter introduces two private asset classes and values the fund at its
NAV and cap-table equity at the holder's per-date fair-market-value (from the
collector's valuation series), rather than inventing prices.

Single-source. The silver is a per-position **change delta** series (collector
`DESIGN.md` §5.1) that the adapter forward-fills into a complete portfolio at
every event date (§5), plus a `cash_flows` ledger projected as double-entry
transaction pairs on the custody account (§7).

## 1. Silver source

One SQLite DB (`$XDG_DATA_HOME/wealthdb/carta/carta.db`). The relevant tables:

- `entities` — one row per (event date, held entity). An entity is either a
  cap-table corporation (`is_fund_investment = 0`) or a fund investment
  (`is_fund_investment = 1`), both under one Carta "individual portfolio"
  (the `individual_id` / `firm_id` columns name it).
- `securities` — one **change-delta** row per cap-table security line per
  event date: `security_type`, `quantity`, `exercise_price` (strike), `cost`,
  `market_value` (the holder's per-date valuation — held shares × the FMV in
  effect at that snapshot; 0 for unexercised options and exited lines),
  `position_status` (`held` | `exited`), `currency`. A share certificate
  born from an option exercise also states `exercise_type`,
  `exercise_date` and `exercise_fmv` (collector migration 0004; an older
  silver lacks the columns and states none).
- `fund_metrics` — the LP capital account per fund entity, **one row per
  quarterly statement** (a NAV time series): `net_asset_value`, `commitment`,
  `called_capital`, `capital_contributed`, `distributions`, `vintage_year`
  (decimal **strings**, parsed exactly).
- `k1_capital_accounts` — one row per K-1 document of a fund (collector
  migrations 0004 and 0005). The adapter reads its box 19 code C, the
  property distributed in kind, and the period it covers (§5). An older
  silver lacks the table and states none.
- `vesting_schedules` / `vesting_events` / `documents` / `cap_calls` /
  `capital_events` — silver-only; not projected to gold (no canonical home —
  see §7).

## 2. Identifier conventions

- `account_external_id` = the Carta individual portfolio (`individual_id`,
  falling back to `firm_id`, then the constant `carta`). **One** gold account
  for the whole portfolio — every held company is a position under it.
- `instrument_external_id` = `entity:<corporation_id>`. One instrument per
  company (the issuer / the fund). Carta private securities carry no
  ISIN/CUSIP/symbol, so the instrument is adapter-scoped; cross-source joins
  on these don't apply.
- `position_key` = `entity:<corporation_id>` — one position per held company
  (keyed on its instrument). The company's share certificates / option grants
  are the lots of that position, aggregated, with the per-lot detail in the
  position payload.

## 3. Coverage matrix

| Gold table   | Carta silver source                              | Notes |
|--------------|--------------------------------------------------|-------|
| accounts     | `dump_runs` (`individual_id`)                    | one custody account carrying the positions and the transaction pairs |
| instruments  | `entities`                                       | one per held company |
| positions    | `securities` (cap-table, lots aggregated) + `fund_metrics` (fund) | one per company, forward-filled — see §5 |
| position_lots | `securities` (held share certificates)          | one per certificate, beside its company's position — see §5 |
| transactions | `cash_flows`                                     | double-entry pairs on the custody account — see §7 |
| cash_balances| —                                                | none; each transaction pair nets to 0, so no cash position is implied |
| portfolios   | —                                                | not grouped at source (the engine rolls the accounts up under "(no portfolio)") |
| fx_rates     | —                                                | adapter emits none (holdings are USD) |

## 4. Account taxonomy

**One** account for the whole Carta portfolio — like a brokerage account that
holds many securities. All values are adapter defaults; config-side
`account_overrides` win on overlap.

- **account_kind** = `custody`. Carta safekeeps / administers private
  securities and fund interests; it is not a trading brokerage.
- **tax_wrapper** = `taxable_personal`. The holdings are personally held
  and taxable; no tax-advantaged wrapper.
- **management_style** = `self_directed`. `management_style` is an
  account-level field (a canonical position carries none), and a Carta
  account may hold holder-controlled equity, GP-managed funds, and pre-conversion
  SAFEs — so the GP-managed-fund vs holder-controlled-equity vs
  pre-conversion-SAFE distinction rides on each position's (`asset_class`,
  `vehicle`) pair ((`private_equity`, `fund`) vs (`private_equity`, `stock`) vs
  (`private_debt`, `convertible_note`)), not here. The holder controls what the
  portfolio holds, so the account is self-directed.
- **base_currency** = `USD` (all Carta holdings; the cap-table side reports the
  currency as `$`, normalised to `USD`). Set on the account so the
  base-currency aggregates (`positions_value`) populate.
- **display_name** = `Carta`.

## 5. Positions

The silver holds per-**lot** **change deltas** — a row for a security lot only
when its state changes (acquisition, exercise, exercise-price change,
disposition, quarterly NAV). gold's as-of query reads the latest snapshot per
source, so for every event date the adapter emits a **complete forward-filled**
snapshot: each lot's latest delta on/before that date, **dropping** those whose
latest `position_status` is `exited`, then **aggregating the surviving lots
into one position per company**. An exited holding disappears exactly at its
disposition date, with no full-portfolio re-storage in silver. When the LAST
holding exits, the disposition date instead carries the **exit-day zero
snapshot** (`silver.ClosureMarkerBatch`): the previous snapshot's positions
replayed at zero value, so gold's value spine and as-of holdings register the
closure on the exit day itself rather than carrying the pre-exit marks
forward as phantom value.

**Cap-table** (the held `securities` lots of one company → one position, whose
(`asset_class`, `vehicle`) pair is derived from the aggregated security types —
see §6):
- `quantity` = Σ the **share** lots' quantity (the common-stock count; an
  unexercised option is a different unit and would double-count the
  certificates it became, so it is excluded here and kept as a 0-value lot in
  the payload).
- `market_value` = Σ every held lot's `market_value` — the holder's per-date
  valuation: held shares × the FMV in effect at the snapshot (a side-loaded
  valuation override when present, else the Carta-derived basis — see the
  collector `DESIGN.md` §5.1); options 0.
- `book_value` = Σ every held line's `cost`: the cash paid for each share
  certificate (quantity × strike for an exercise), each convertible's
  principal, and each award's or warrant's cost. An exercise or a
  purchase carries no fee. The stamp says what the sum is (DESIGN.md
  §7.4):
  - every costed line is a share certificate: the sum of its lots,
    `derived` / `lots` / `none`;
  - any other line has a cost: the cash paid, `derived` / `paid_in` /
    `none`.

  A certificate born from an NSO exercise has the fair-market-value at
  exercise as its tax basis. The book value stays the cash paid, and the
  exercise facts ride in the lot's payload.
- `acquisition_date` = the EARLIEST acquisition date of the share lots
  (see below). Carta states it per lot as `original_acquisition_date`,
  which is not the certificate's issue date: a certificate is re-issued
  whenever the holding is restructured — a transfer, a split, a
  conversion — and the new one is dated to the re-issue while the shares
  behind it are the same shares, so the acquisition date can precede the
  platform's own coverage. A later lot's date would claim the oldest
  shares were acquired more recently than they were. A holding without a
  dated share lot takes the earliest date its other lines carry. A
  convertible that states none contributes its issue date: a SAFE or note
  is not re-issued on a split or transfer the way a share certificate is,
  so its issue date is the day it was bought. Other lines that state none
  contribute nothing, and a position whose lines all state none carries
  no date.
- The per-lot detail (label, `security_type`, quantity, cost, market_value,
  issue date, acquisition date, strike) rides in the position payload under
  `lots`.

Each held **share certificate** is also one row in `position_lots`, beside
its company's position:
- `lot_key` = the certificate's security id.
- `quantity` = its shares; `book_value` = its `cost`, stamped `stated`;
  `market_value` = its `market_value`.
- `acquisition_date` = its `original_acquisition_date`, where stated.
- `payload` = `exercise_type`, `exercise_date` and `exercise_fmv`, where
  stated.

The certificates' quantities sum to the position's. An option grant is not
a lot: it holds no shares until it is exercised, and then the certificate
it becomes is one. A convertible is not a lot either until it converts. So
the lots' book values sum to the position's exactly when it is stamped
`lots`.

**Fund LP** (one position per held `fund_metrics` row → (`private_equity`,
`fund`)):
- `market_value` = `net_asset_value` — the NAV of the latest quarterly
  statement on/before the as-of date (a real per-quarter time series).
- `book_value` = `capital_contributed`: the capital paid in, gross of any
  capital paid back. A row parsed from a capital-account statement takes
  the statement's inception-to-date contributions. Stamped `stated` /
  `paid_in` / `included`: the management fees are drawn from that capital.
  An in-kind distribution reduces it (see below).
- `acquisition_date` = the fund's first capital call.
- `quantity` = NULL (an LP interest has no unit count).
- `commitment` / `called_capital` / `distributions` / `vintage_year` ride
  in the payload.

Before a fund's first NAV — its first statement can follow its first call
by years — the interest has no valuation of its own. Leaving it out would
book each call as a loss in the period it was paid and the first NAV as a
gain, so from the first call until the first NAV the position is carried at
the capital paid in less any capital paid back (`market_value`). Its
`book_value` is the capital paid in alone, as on a NAV row, summed from the
call notices and stamped `derived` / `paid_in` / `included`. Its payload
carries `valuation_basis: called_capital`. Each fund cash event before the
first NAV is a snapshot day.

A K-1's box 19 code C (`k1_capital_accounts.property_distributions`) is
property the fund distributed in kind. It moves basis out with the asset,
so from the K-1's period end on the fund's book value is the paid-in
figure less every such distribution so far:
- the period end is the K-1's `period_end` for a fiscal year, else Dec 31
  of its `tax_year`, and it is a snapshot day;
- two documents of one fund and tax year are copies of one K-1, so the
  later document counts;
- the book value never goes below zero; the load counts the funds held
  at zero;
- a reduced book value is stamped `derived` / `paid_in` / `included`, and
  its payload carries `paid_in` and `property_distributed`.

Cash paid back (code A) does not reduce the book value.

So an as-of query sees the held cap-table equity valued at its basis and the
fund at the right quarter's NAV; after a cap-table exit, only the
surviving positions remain.

The currency on every position is normalised to ISO (`$` → `USD`).

## 6. Instruments

One instrument per entity (`entity:<id>`), carrying the same (`asset_class`,
`vehicle`) pair its position does: (`private_equity`, `fund`) for the fund, and
for a cap-table company the pair its security types derive — (`private_equity`,
`stock`), (`private_equity`, `option`), or (`private_debt`, `convertible_note`)
for a purely-convertible holding (see below). `name` = the entity legal name;
ISIN/CUSIP/symbol/currency are NULL (private securities have none). The
company's single position references its instrument; its lots (e.g. several
share certificates + option grants) are aggregated into that one position,
not separate instruments or positions.

### Taxonomy (`asset_class` × `vehicle`)

Every Carta holding derives a two-dimensional (`asset_class`, `vehicle`) pair —
the exposure (what moves the value) and the wrapper (how it is held), per
[`docs/TAXONOMY.md`](../TAXONOMY.md). `capTableTaxonomy` (`classmap.go`)
classifies a cap-table position from the security types it aggregates:

- any real share-settled unit (share / RSU / RSA / PIU / equity grant) →
  (`private_equity`, `stock`);
- equity that is only option-shaped (option / warrant / SAR) →
  (`private_equity`, `option`);
- a purely-convertible holding still pre-conversion (a SAFE / note) →
  (`private_debt`, `convertible_note`) — carried at principal, kept distinct
  from equity until it converts.

A fund LP interest is (`private_equity`, `fund`) (`appendFundAt` in
`snapshots.go`; a Carta fund investment is read as a venture/PE feeder). The illiquid
private exposures sit beside the public-market ones so portfolio queries can
separate non-quotable private holdings from listed securities; the security
type stays queryable in the payload (a private ESO is not lumped with an
exchange-traded `option`, nor a private share with public equity).

Neither `asset_class` nor `vehicle` carries a SQL CHECK — both are
Go-validated (`AssetClass.Valid()` / `Vehicle.Valid()`, and the combination
via `ValidTaxonomyPair`, in `internal/canonical/taxonomy.go`), so no gold
migration was needed for the enums — only migration `0013` widening the
`silver_sources.silver_kind` whitelist to admit `carta`.

## 7. Transactions

The silver `cash_flows` ledger (collector DESIGN.md §5.2) is projected as
**balanced double-entry pairs** on the custody account — the account carrying
the value spine, so the returns engine sees the flows. Carta exposes no real
cash balance (a capital call is wired from an external bank straight into the
SPV/fund, an exercise is paid externally, proceeds leave to an external
account), so each event splits into an external-bank leg
(`deposit`/`withdrawal`: the boundary flows the returns policy counts) and a
holding leg that net to zero — no cash position is implied (the same shape as
a brokerage's same-day deposit + buy). Every leg links to the company's
instrument; the buy / sell legs carry the share lot + price.

| `cash_flows.kind` | gold pair (signed via `ApplyCanonicalSign`) |
|---|---|
| `exercise`     | `deposit` (+) + `buy` (−, with lot) |
| `convertible_purchase` | `deposit` (+) + `buy` (−, no lot — a SAFE or note buys no shares yet) |
| `capital_call` | `deposit` (+) + `contribution` (−) |
| `exit`         | `sell` (+, with lot) + `withdrawal` (−); a $0 exit emits the $0 `sell` and omits the meaningless $0 `withdrawal` |
| `distribution` | `distribution` (+) + `withdrawal` (−) |

A company's side-loaded `<account_id>-transactions.csv` (collector DESIGN.md
§5.2) instead names canonical kinds directly (`sell` / `withdrawal` /
`deposit` / `buy` / `contribution`), which the adapter emits **1:1** — the CSV
supplies both halves of an exit (a sale plus the withdrawals it splits
into), so they net to 0 without auto-pairing and override the
synthesized $0 exit. A fund's side-loaded file can itemise calls made before
Carta's coverage; they reach silver as `capital_call` / `distribution` rows
and pair like the fund's own.

`Status` reports the `cash_flows` date range as the transaction extrema.

**Vesting schedules** (`vesting_schedules` / `vesting_events`),
**documents**, **cap_calls**, and the `capital_events` audit timeline stay
silver-only — gold has no canonical home for a vesting timeline or a document
archive, and the LP capital account already carries the called/uncalled
figures in the fund position payload.

## 8. Change number & window

`ChangeWindow` triggers on any `dump_run` past the watermark (a new download),
and `NewChangeNumber` is `MAX(dump_runs.snapshot_at)`, so an idle reload is a
no-op. The window itself spans the **event-dated content** (MIN/MAX
`snapshot_at` across `entities` / `securities` / `fund_metrics`) and every
`cash_flows` date, **not** the download time: the reconstructed deltas can
sit years before the download (an exercise, an acquisition, a quarterly NAV) and
must fall in-window to reach gold, and a fund's calls can precede its first
statement. `Status` reports the content span widened by the fund carry days
as the observable range, with `LatestChangeNumber` = the newest dump.

## Open questions / future work

- **Cap-table valuation.** Held shares are valued per date — share count from
  the certificate issue dates × the FMV in effect at each snapshot. When a
  company exits, Carta purges its historical 409A timeline, so an exact
  per-date FMV comes from a side-loaded valuation override
  (`<account_id>-valuations.csv` in the bronze root); absent one, the collector falls
  back to the FMV-at-last-exercise — exact from the last exercise onward, but
  over-stating earlier dates (collector `DESIGN.md` §5.1).
- **Per-company aggregation (done).** Each company is one gold position; its
  share certs / option grants are aggregated into it (the per-lot detail
  rides in the position payload), and its share certificates are its
  `position_lots`, mirroring how a public brokerage account holds one
  position per security with tax lots underneath.
- **Exercise basis.** For a certificate born from an NSO exercise, the tax
  basis is the fair-market-value at exercise, not the cash paid. The lot
  payload carries it; whether the book value should is an open decision.
- **Vesting in gold.** Deliberately silver-only. If a future need arises, a
  dedicated gold table (not the positions/transactions facts) would be the
  place.
