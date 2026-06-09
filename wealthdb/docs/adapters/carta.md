# carta adapter

Projects the **carta** silver (private-market holdings on carta.com —
see `collectors/carta/`) into canonical gold change records.
`internal/silver/carta/`.

Carta is the first wealthdb source for **non-public-market securities**:
direct private-company equity + equity-comp (shares, option grants with
strike + vesting, RSUs/RSAs, SAFEs/notes, warrants) and an LP interest in a
venture/PE fund. None has an ISIN/CUSIP/symbol or a quotable market price, so
the adapter introduces two private asset classes and values the fund at its
NAV and cap-table equity at the holder's basis (the fair-market-value at the
last exercise), rather than inventing prices.

Single-source, no transactions (§7). The silver is a per-position **change
delta** series (collector `DESIGN.md` §5.1); the adapter forward-fills it into
a complete portfolio at every event date (§5).

## 1. Silver source

One SQLite DB (`~/wealthdb/carta/carta.db`). The relevant tables:

- `entities` — one row per (event date, held entity). An entity is either a
  cap-table corporation (`is_fund_investment = 0`) or a fund investment
  (`is_fund_investment = 1`), both under one Carta "individual portfolio"
  (the `individual_id` / `firm_id` columns name it).
- `securities` — one **change-delta** row per cap-table security line per
  event date: `security_type`, `quantity`, `exercise_price` (strike), `cost`,
  `market_value` (the holder's valuation — held shares × FMV-at-last-exercise;
  0 for unexercised options and exited lines), `position_status`
  (`held` | `exited`), `currency`.
- `fund_metrics` — the LP capital account per fund entity, **one row per
  quarterly statement** (a NAV time series): `net_asset_value`, `commitment`,
  `called_capital`, `capital_contributed`, `distributions`, `vintage_year`
  (decimal **strings**, parsed exactly).
- `vesting_schedules` / `vesting_events` / `documents` / `cap_calls` /
  `capital_events` — silver-only; not projected to gold (no canonical home —
  see §7).

## 2. Identifier conventions

- `account_external_id` = the Carta entity (`corporation_id`), bare. One
  gold account per entity.
- `instrument_external_id` = `entity:<corporation_id>`. One instrument per
  entity (the issuer company / the fund). Prefixed so it reads
  unambiguously against the same-valued account id (different tables).
  Carta private securities carry no ISIN/CUSIP/symbol, so the instrument is
  adapter-scoped; cross-source joins on these don't apply.
- `position_key` = `<security_type>:<security_id>` for cap-table lines;
  the literal `fund` for the single fund-LP position.

## 3. Coverage matrix

| Gold table   | Carta silver source                              | Notes |
|--------------|--------------------------------------------------|-------|
| accounts     | `entities`                                       | one per held entity |
| instruments  | `entities`                                       | one per held entity |
| positions    | `securities` (cap-table) + `fund_metrics` (fund) | forward-filled — see §5 |
| transactions | —                                                | none (§7) |
| cash_balances| —                                                | Carta holds no cash |
| portfolios   | —                                                | not grouped at source (the engine rolls the accounts up under "(no portfolio)") |
| fx_rates     | —                                                | adapter emits none (holdings are USD) |

## 4. Account taxonomy

One account per entity. All values are adapter defaults; config-side
`account_overrides` win on overlap.

- **account_kind** = `custody`. Carta safekeeps / administers private
  securities and fund interests; it is not a trading brokerage.
- **tax_wrapper** = `taxable_personal`. The holdings are personally held
  and taxable (K-1 / 1042-S confirm); no tax-advantaged wrapper.
- **management_style**:
  - fund entity → `discretionary` (the GP invests; the LP places no trades).
  - cap-table entity → `self_directed` (the holder controls exercise / sale
    timing, even though the equity was employer-granted).
- **base_currency** = the entity's holdings currency (USD; the cap-table side
  reports it as `$`, normalised to `USD`). Set on the account so the
  base-currency aggregates (`positions_value`) populate.
- **display_name** = the entity's legal name (the company / fund name).

## 5. Positions

The silver holds per-position **change deltas** — a row only when a position's
state changes (acquisition, exercise, exercise-price change, disposition,
quarterly NAV). gold's as-of query reads the latest snapshot per source, so
for every event date the adapter emits a **complete forward-filled** snapshot:
each position's latest delta on/before that date, **dropping** those whose
latest `position_status` is `exited`. An exited holding therefore disappears
exactly at its disposition date, with no full-portfolio re-storage in silver.

**Cap-table** (one position per held `securities` line → `private_equity`):
- `quantity` = silver `quantity`.
- `market_value` = silver `market_value` — the holder's valuation: held shares
  × the fair-market-value at the last exercise; unexercised options 0.
- `book_value` = silver `cost`.
- The strike (`exercise_price`), vested / exercised / exercisable counts, and
  `security_type` ride in the position payload.

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
securities have none). All of an entity's positions (e.g. several share lots and option grants) reference its single instrument.

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

None. The carta silver has no transaction table: option exercises and fund
cash-flows are reconstructed into the per-position deltas / NAV series above,
not exposed as discrete events. `Status` reports the −1 transaction sentinel.

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

- **Cap-table valuation accuracy.** Held shares are valued at the single
  FMV-at-last-exercise across the whole held period — exact from the last
  exercise onward, but over-stating the count/value at earlier dates (the
  count grew via the intervening exercises, at then-lower FMVs). A full
  common-stock 409A timeline would refine it; Carta purges it when a company
exits, so only the FMV-at-each-exercise survives (collector `DESIGN.md`
  §5.1).
- **Per-lot vs aggregate.** One gold position per silver security row
  preserves lot detail (one position per share lot). Aggregating per
  (issuer, security_type) is a future option if the row count becomes noisy.
- **Vesting in gold.** Deliberately silver-only. If a future need arises, a
  dedicated gold table (not the positions/transactions facts) would be the
  place.
