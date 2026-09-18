# Instrument taxonomy: asset class × vehicle

Reference for the two-dimensional instrument classification. Status:
**implemented** — `asset_class` (exposure) and `vehicle` (wrapper)
are the live gold columns; the canonical enums live in
`internal/canonical/taxonomy.go` and `enums.go`, and every adapter
emits an (`asset_class`, `vehicle`) pair. Section 4 records how the
former single `asset_class` values map onto the pair, for reading
data or history captured before the split.

## 1. Design principles

1. **`asset_class` is exposure** — *what moves the value*. The
   allocation dimension: what the holder is invested in, independent
   of packaging. Allocation dashboards, risk views, and target
   weights group by this.
2. **`vehicle` is the wrapper** — *how the exposure is held*. Drives
   liquidity, custody, fee and tax mechanics. An S&P 500 ETF and an
   S&P 500 mutual fund are the same exposure in different wrappers.
3. This mirrors industry practice: ISO 10962 CFI separates the
   instrument *category* (≈ vehicle) from its *attributes*
   (≈ underlying); family-office reporting platforms separate
   "asset class" from "investment structure". Bank feeds disagree on
   which dimension they report (UBS UAC codes are exposure, CFI codes
   are wrapper) — the 1-D scheme collided precisely because of that.
4. **Derivatives classify by underlying**: an option on a listed
   stock is `public_equity × option`; an FX forward is
   `foreign_exchange × forward`.
5. **Liabilities classify by what they finance**: a mortgage is
   negative `real_estate` exposure held via the `mortgage` vehicle,
   so allocation views show net property equity.
6. **Look-through stops at one level**: a private-equity fund is
   `private_equity × fund`; the companies inside the fund are not
   modelled. (Same rule the private-market adapters already follow.)

## 2. `asset_class` values (13)

| Value | Definition | Example holdings |
| --- | --- | --- |
| `public_equity` | Listed company shares and products whose return is listed-equity performance | common/preferred stock, ADRs, equity ETFs and index funds, equity options' underlying exposure, equity-linked structured products, subscription rights |
| `private_equity` | Unlisted company ownership | direct stakes in private companies, employee equity comp (options/RSUs/warrants), venture SPVs, PE/VC funds and feeders, listed private-equity ETFs, equity-like pre-seed convertible notes (via override, see §5.10) |
| `fixed_income` | Public / rated debt | government, municipal and corporate bonds, MTNs, bond ETFs and bond index funds |
| `private_debt` | Non-public credit claims | convertible notes and SAFEs, bilateral private loans, sale-proceeds escrow receivables |
| `real_estate` | Property exposure, direct or securitized; includes property-secured liabilities as negative exposure | directly-held property, real-estate index funds, REITs, mortgages |
| `infrastructure` | Infrastructure assets | infrastructure funds and feeders, listed-infrastructure funds |
| `metal` | Precious metals | vaulted bullion, physical-metal ETFs/ETPs (e.g. GLD), physical-gold index funds |
| `crypto` | Digital assets | coins and tokens held in wallets, spot crypto ETFs/ETPs (e.g. IBIT) |
| `cash` | Currency and cash equivalents | account balances, money-market funds, time deposits; negative = margin debit |
| `foreign_exchange` | Currency derivative exposure | FX forwards, FX options, currency-linked structured products |
| `hedge_fund` | Absolute-return / manager-strategy exposure, where the strategy — not a single class — is the exposure | hedge funds, long-short liquid-alternative funds |
| `multi_asset` | Blended-allocation products | target-date and 529 plan sleeves, balanced / "real return" funds, robo strategy sleeves |
| `other` | Fallback; adapters must set explicitly | unclassifiable placeholders, synthetic $0 closure markers |

## 3. `vehicle` values (18)

| Value | Definition | Notes |
| --- | --- | --- |
| `stock` | Direct shares, listed or private | includes ADRs, preferred shares, restricted/RSU-settled shares |
| `etf` | Exchange-traded fund / product | ETFs, ETPs/ETCs, exchange-traded grantor trusts (GLD, IBIT) |
| `fund` | Pooled vehicle, not exchange-traded | mutual/index/institutional funds, SICAVs, LP funds and feeders, hedge funds, plan and robo sleeves, money-market funds |
| `spv` | Single-deal special-purpose vehicle | one underlying company per vehicle |
| `bond` | Direct debt security | government / municipal / corporate issues, MTNs |
| `convertible_note` | Convertible note or SAFE | pre-conversion; converts to `stock` or writes to zero |
| `loan` | Bilateral private loan | |
| `option` | Option-shaped claim | listed options, employee options, warrants, FX options |
| `future` | Listed future | enum-supported; no adapter emits it yet |
| `forward` | OTC forward | FX forwards |
| `time_deposit` | Term-locked cash placement | time / fiduciary deposits, money-market contracts |
| `demand_deposit` | On-demand account cash | plain cash balances, used where views union cash into allocation |
| `physical` | Outright holding of the asset itself | bullion bars, coins in wallets, directly-held property |
| `structured_product` | Structured note / certificate | capital-protected and currency-linked structures |
| `right` | Subscription / entitlement right | |
| `mortgage` | Property-secured liability | negative market value by convention |
| `escrow` | Sale-proceeds holdback | contingent receivable |
| `other` | Fallback; adapters must set explicitly | |

## 4. How the former 1-D classes map to the pair

The single `asset_class` scheme this replaced used the values in the
left column; each dissolves into one or more `(asset_class, vehicle)`
pairs. Kept as a reading aid for pre-split data and for the adapters'
intermediate 1-D classifiers, which still emit these values internally
before mapping to the pair.

| former 1-D `asset_class` | 2-D `(asset_class, vehicle)` |
| --- | --- |
| `equity` | `public_equity × stock` |
| `etf` | `public_equity × etf` (the residual default after exposure refinement) |
| `bond_etf` | `fixed_income × etf` |
| `fund` | vehicle `fund`; asset_class re-derived per fund (public_equity / fixed_income / real_estate / multi_asset / cash / …) |
| `bond` | `fixed_income × bond` |
| `option` | underlying `× option` (equity options → `public_equity`) |
| `future` | underlying `× future` |
| `fx_forward` | `foreign_exchange × forward` |
| `fx_option` | `foreign_exchange × option` |
| `money_market` | `cash × fund` (money-market funds) or `cash × time_deposit` (contracts) |
| `otc_derivative` | underlying `× forward` / `other` |
| `metal` | `metal × physical / etf / fund` |
| `crypto` | `crypto × physical / etf` |
| `private_equity` | `private_equity × stock / option` |
| `spv` | `private_equity × spv` |
| `private_fund` | `private_equity / infrastructure / hedge_fund × fund` |
| `real_estate` | `real_estate × physical` |
| `convertible_note` | `private_debt × convertible_note` |
| `mortgage` | `real_estate × mortgage` |
| `other` | `other × other`, minus the recoverable cases above |

`fund` and `private_fund` were the only values whose dissolution
needed real classification logic; every other mapping is mechanical.

## 5. Decisions of record

1. **Mortgages sit under `real_estate`** (netting), not a dedicated
   liability class.
2. **`hedge_fund` is an asset class, not a vehicle** — the manager's
   strategy is the exposure; matches private-banking allocation
   practice.
3. **Listed private-equity / listed infrastructure classify by
   exposure** (`private_equity × etf`, `infrastructure × fund`); the
   vehicle column preserves the liquidity fact.
4. **Escrow is `private_debt`** — a contingent receivable from a
   sale. Weakest call; revisit if it distorts the private-debt
   bucket.
5. **Warrants → `option`, RSUs → `stock`** — no dedicated vehicles
   until a use case demands the split.
6. **`metal`, not `commodity`** — the metal holdings the sources
   surface are precious metals, and `metal` names the class without
   implying broad commodity exposure. A broad-commodities product
   would prompt either a rename or a sibling `commodity` class.
7. **Blends/target-date → `multi_asset`; absolute-return/long-short
   → `hedge_fund`.**
8. **Money-market funds are `cash`**, not fixed income.
9. **`demand_deposit` / `time_deposit`** name the two cash wrappers:
   at-sight account cash vs term-locked placements.
10. **Convertible notes default to `private_debt`, but the pair
    `private_equity × convertible_note` is admitted** for notes that
    are economically equity — e.g. a hypothetical 0%-interest pre-seed
    note with no repayment expectation, which either converts in the
    next round or writes to zero. Adapters keep emitting `private_debt` (the legal
    form); an `instrument_overrides` entry pins the exposure to
    `private_equity` per holding.

## 6. Source signals per dimension (implementation guide)

| Source signal | Feeds | Notes |
| --- | --- | --- |
| CFI category + group (UBS, ISO 10962) | both | `E`→(public_equity, stock), group `EY` participation certs→structured_product; `CE`→etf, other `C`→fund; `D`→bond; `R`→right; `O`/`H`→option; `F`→future; `J`→forward (FX); `S`/`I`/`K`/`L`/`T`/`M`/unknown→other |
| UAC asset-class code (UBS) | asset_class | 0100→cash, 0300→public_equity, 0400→private-markets family, 0600→metal |
| `instrument.assetType` + `.type` (Schwab) | both | EQUITY→(public_equity, stock); COLLECTIVE_INVESTMENT+EXCHANGE_TRADED_FUND→(…, etf) |
| Statement section headers (Swissquote) | vehicle-leaning | "ETFs"/"Funds"/"Bonds"/"Shares"/"Options"/"Structured Products" |
| Description templates (ubs-web / historical PDFs) | both | "Reg.shs"/"Shs"/DRs/participation certs→stock, ETF umbrellas→etf, SICAV/fund→fund, "Actively Managed Certificate"→structured_product, precious-metals lines→(metal, physical) |
| Ticker/description shapes (fidelity family + schwab statements) | both | CUSIP-9→bond, `…X`→fund, `…XX`→money fund (cash), OCC→option, word-ETF or ETF-only issuer→etf |
| Security-name keywords (shared refiner) | asset_class | bullion / crypto / bond keywords refine exposure inside etf/fund vehicles |
| Collector kind (private-market + manual) | both | carta/angellist/equityzen/manual kinds map directly to pairs |
| `instrument_overrides` (config) | both | escape hatch; pins both `asset_class` and `vehicle` for a named instrument |
| Booking type / narrative on a trade (UBS statements) | both | last resort, on the TRADE row rather than the instrument: `SHARE`→(public_equity, stock), `SUBSCRIPTION RIGHT` and a narrative `ANR`→(…, right), `PRECIOUS METAL …` and the metal currency codes→(metal, physical), `CAPITAL CALL`→(private_equity, fund) |

Where a source pins only one dimension, the other defaults from the
pair tables above (e.g. a vehicle-only "Funds" section header defaults
to the fund's name-derived exposure, `other` if underivable).

The last row is the exception, and the only signal that lands on a
fact row rather than on the instrument. `transactions` carries its own
`(asset_class, vehicle)` for what the instrument cannot answer: both
NULL where the instrument is known, one half alone where that is all
the trade adds (an option resolves to its underlying and states
`option` in the vehicle), and both where the feed named the kind of
thing but no instrument at all. Neither half defaults — an unstated
half means "ask the instrument", not `other`. See DESIGN.md §10.8.
