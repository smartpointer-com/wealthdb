# carta adapter

Projects the **carta** silver (private-market holdings on carta.com —
see `collectors/carta/`) into canonical gold change records.
`internal/silver/carta/`.

Carta is the first wealthdb source for **non-public-market securities**:
direct private-company equity + equity-comp (shares, option grants with
strike + vesting, RSUs/RSAs, SAFEs/notes, warrants) and an LP interest in a
venture/PE fund. None has an ISIN/CUSIP/symbol or a quotable market price, so
the adapter introduces two private asset classes and values the fund at its
NAV and cap-table equity at the holder's per-date fair-market-value (from the
collector's valuation series), rather than inventing prices.

Single-source. The silver is a per-position **change delta** series (collector
`DESIGN.md` §5.1) that the adapter forward-fills into a complete portfolio at
every event date (§5), plus a `cash_flows` ledger projected as double-entry
transaction pairs on a sentinel funding account (§7).

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
  `position_status` (`held` | `exited`), `currency`.
- `fund_metrics` — the LP capital account per fund entity, **one row per
  quarterly statement** (a NAV time series): `net_asset_value`, `commitment`,
  `called_capital`, `capital_contributed`, `distributions`, `vintage_year`
  (decimal **strings**, parsed exactly).
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
| accounts     | `dump_runs` (`individual_id`)                    | the custody account (positions) + the `carta-funding` sentinel (transactions) |
| instruments  | `entities`                                       | one per held company |
| positions    | `securities` (cap-table, lots aggregated) + `fund_metrics` (fund) | one per company, forward-filled — see §5 |
| transactions | `cash_flows`                                     | double-entry pairs on the funding sentinel — see §7 |
| cash_balances| —                                                | none; the funding sentinel's 0 is implicit in the paired ledger |
| portfolios   | —                                                | not grouped at source (the engine rolls the accounts up under "(no portfolio)") |
| fx_rates     | —                                                | adapter emits none (holdings are USD) |

## 4. Account taxonomy

**One** account for the whole Carta portfolio — like a brokerage account that
holds many securities. All values are adapter defaults; config-side
`account_overrides` win on overlap.

- **account_kind** = `custody`. Carta safekeeps / administers private
  securities and fund interests; it is not a trading brokerage.
- **tax_wrapper** = `taxable_personal`. The holdings are personally held
  and taxable (K-1 / 1042-S confirm); no tax-advantaged wrapper.
- **management_style** = `self_directed`. `management_style` is an
  account-level field (a canonical position carries none), and a Carta account can hold both holder-controlled equity and a GP-managed fund — so the
  GP-vs-holder distinction rides on each position's `asset_class`
  (`private_fund` vs `private_equity`), not here. The holder controls what the
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
disposition date, with no full-portfolio re-storage in silver.

**Cap-table** (the held `securities` lots of one company → one `private_equity`
position):
- `quantity` = Σ the **share** lots' quantity (the common-stock count; an
  unexercised option is a different unit and would double-count the
  certificates it became, so it is excluded here and kept as a 0-value lot in
  the payload).
- `market_value` = Σ every held lot's `market_value` — the holder's per-date
  valuation: held shares × the FMV in effect at the snapshot (a side-loaded
  valuation override when present, else the Carta-derived basis — see the
  collector `DESIGN.md` §5.1); options 0.
- `book_value` = Σ every held lot's `cost` (the cost basis).
- The per-lot detail (label, `security_type`, quantity, cost, market_value,
  issue date, strike) rides in the position payload under `lots`.

**Fund LP** (one position per held `fund_metrics` row → `private_fund`):
- `market_value` = `net_asset_value` — the NAV of the latest quarterly
  statement on/before the as-of date (a real per-quarter time series).
- `book_value` = `capital_contributed` (cost basis paid in).
- `quantity` = NULL (an LP interest has no unit count).
- `commitment` / `called_capital` / `distributions` / `vintage_year` ride
  in the payload.

So an as-of query sees the held cap-table equity valued at its basis and the
fund at the right quarter's NAV; after a cap-table exit, only the
surviving positions remain.

The currency on every position is normalised to ISO (`$` → `USD`).

## 6. Instruments

One instrument per entity (`entity:<id>`), `asset_class` =
`private_fund` for the fund, `private_equity` for the cap-table company.
`name` = the entity legal name; ISIN/CUSIP/symbol/currency are NULL (private
securities have none). The company's single position references its instrument;
its lots (e.g. several share certificates + option grants) are aggregated into that one
position, not separate instruments or positions.

### Asset classes (new, canonical)

`AssetClassPrivateEquity` ("private_equity") and `AssetClassPrivateFund`
("private_fund") were added to `internal/canonical/enums.go` for this
adapter. They sit beside the public-market `equity` / `fund` so portfolio
queries can separate illiquid, non-quotable private holdings from listed
securities. Every cap-table security type folds into `private_equity` (a
private ESO is not lumped with exchange-traded `option`, nor a private share
with public `equity`); the type stays in the payload. `asset_class` carries
no SQL CHECK (Go-validated via `AssetClass.Valid()`), so no gold migration
was needed for the enum — only migration `0013` widening the
`silver_sources.silver_kind` whitelist to admit `carta`.

## 7. Transactions

The silver `cash_flows` ledger (collector DESIGN.md §5.2) is projected as
**balanced double-entry pairs** on a sentinel funding account
(`carta-funding`) — Carta exposes no real cash balance (a capital call is wired
from an external bank straight into the SPV/fund, an exercise is paid
externally, proceeds leave to an external account), so each event splits into an
external-bank leg and a holding leg that net to zero. The funding account is a
pure pass-through clearing account whose derived balance is always exactly 0
(`AccountKind = cash`, no `cash_balance` row). Every leg links to the company's
instrument; the buy / sell legs carry the share lot + price.

| `cash_flows.kind` | gold pair (signed via `ApplyCanonicalSign`) |
|---|---|
| `exercise`     | `deposit` (+) + `buy` (−, with lot) |
| `capital_call` | `deposit` (+) + `contribution` (−) |
| `exit`         | `sell` (+, with lot) + `withdrawal` (−); a $0 exit emits the $0 `sell` and omits the meaningless $0 `withdrawal` |
| `distribution` | `distribution` (+) + `withdrawal` (−) |

A side-loaded `<account_id>-transactions.csv` (collector DESIGN.md §5.2) instead
names canonical kinds directly (`sell` / `withdrawal` / `deposit` / `buy` /
`contribution`), which the adapter emits **1:1** — the CSV supplies both halves
of the exit (a sale plus the withdrawals it splits into), so they
net to 0 without auto-pairing and override the synthesized $0 exit.

`Status` reports the `cash_flows` date range as the transaction extrema; the
load window (the content-table span) already covers them.

**Vesting schedules** (`vesting_schedules` / `vesting_events`),
**documents**, **cap_calls**, and the `capital_events` audit timeline stay
silver-only — gold has no canonical home for a vesting timeline or a document
archive, and the LP capital account already carries the called/uncalled
figures in the fund position payload.

## 8. Change number & window

`ChangeWindow` triggers on any `dump_run` past the watermark (a new download),
and `NewChangeNumber` is `MAX(dump_runs.snapshot_at)`, so an idle reload is a
no-op. The window itself spans the **event-dated content** (MIN/MAX
`snapshot_at` across `entities` / `securities` / `fund_metrics`), **not** the
download time: the reconstructed deltas sit years before the download (an
exercise, an acquisition, a quarterly NAV) and must fall in-window to reach
gold. `Status` reports the same content span as the observable range, with
`LatestChangeNumber` = the newest dump.

## Open questions / future work

- **Cap-table valuation.** Held shares are valued per date — share count from
  the certificate issue dates × the FMV in effect at each snapshot. When a
  company exits, Carta purges its historical 409A timeline, so an exact
  per-date FMV comes from a side-loaded valuation override
  (`<account_id>-valuations.csv` in the bronze root); absent one, the collector falls
  back to the FMV-at-last-exercise — exact from the last exercise onward, but
  over-stating earlier dates (collector `DESIGN.md` §5.1).
- **Per-company aggregation (done).** Each company is one gold position; its
  share certs / option grants are aggregated into it as lots (the per-lot
  detail rides in the position payload), mirroring how a public brokerage
  account holds one position per security with tax lots underneath.
- **Vesting in gold.** Deliberately silver-only. If a future need arises, a
  dedicated gold table (not the positions/transactions facts) would be the
  place.
