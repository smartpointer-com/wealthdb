# Exposure

What is really owned. The exposure report looks through every fund to
what it holds, down to the single security, and adds the result to
what is held directly. It answers by asset class, sector, country,
currency and security, as of any day. It also says where each holding
legally sits (§13). This file defines every figure the report shows and
the rules behind them.

Related design: DESIGN.md §4.17 (the `exposure` command), §7.6 (the
reference and config tables), §10.14 (the derived tables and macros);
[adapters/refdata.md](adapters/refdata.md) (how reference data loads,
the keys and aliases, and the wanted list); migrations 0125 to 0131.

Every figure is a share of a holding's market value, the value
`holdings` reports. The look-through moves value between rows; it never
creates or loses any. Every view therefore sums to `holdings global`.

---

## 1. Words

- **A composition** is what one fund holds on one day, as a list of
  lines with weights in percent of the fund's net assets. A provider
  serves it: an issuer's daily file, an SEC filing, a report. Gold keeps
  every composition it ever loads.
- **A fund reference** names a fund: `isin:…`, `cusip:…` or `symbol:…`.
  **A security key** names a security the same way, with `figi:…` and
  `name:…` as last resorts. One scheme serves both: a fund is a security
  that has a composition.
- **An alias** reads one key as another. A ticker a fund line states
  beside an ISIN reads as that ISIN, so a stock held under its ticker
  and a line that names its ISIN are one security.
  [adapters/refdata.md §3](adapters/refdata.md#3-identity) gives the key
  scheme and the alias rules.
- **A declaration** is what the household states about a holding no
  public source describes: a property's country, or a private fund's
  target split by region (§9).
- **A leaf** is what the look-through ends at: a holding nothing
  describes, a part of a declared holding, a fund's line, a remainder,
  or the cash leg of a leveraged proxy.
- **Net worth** is the sum of every leaf, as `holdings global` reports
  it. A loan is a negative leaf.
- **A legal situs** is the one country whose law a holding is an
  interest under (§13).

## 2. The look-through

For a day:

1. Start from the positions and the cash balances on that day, by the
   rule every report uses: each source contributes its latest snapshot
   on or before the day. Each line is valued in the output currency
   exactly as `holdings global` values it. An account `holdings global`
   leaves out, because no rate converts any of its positions or any of
   its cash, is left out here too.
2. Read each holding's key through `holding_aliases` and
   `instrument_aliases()`, as the gains reports do, then through the
   security aliases (§1).
3. A holding whose key names a fund with a composition (§4) is replaced
   by its lines. Each line is worth the holding's value times its
   weight.
4. A line that is itself a fund with a composition is replaced again,
   up to three levels deep. This covers funds of funds and plan sleeves.
   A fund the third level still names stays a leaf, flagged `partial`.
5. What the weights do not explain becomes a remainder leaf under the
   fund: the holding's value times (100 − the sum of the weights) %. A
   line without a weight counts in the remainder.
6. Every leaf gets an asset class, a sector, a country and a currency
   (§3, §6).

What describes a holding, in this order:

1. its fund's own composition, within `max_gap_days` (§4);
2. its declaration (§9);
3. a proxy's composition (§5).

A declaration never stands in for a fund's own composition. Where the
config says a proxy always replaces that composition (§5), the proxy
still does, declaration or not. A declaration describes a holding held
directly: a fund that another fund holds reads its composition or its
proxy, never a declaration.

A holding none of them describes is a leaf of its own. A listed
security takes its attributes from §3. A fund takes none of them, held
directly or named by a line no level expands: where it is domiciled
says nothing of where its value is. A real estate fund keeps the
`real_estate` sector. Anything else stays unknown.

A declared holding splits into parts, each a share of its value (§9).
A part takes the class, region, country, currency and sector the
record states for it. A dimension the record leaves out reads as it
would without a record: a listed security's sector and country come
from §3, a private holding counts in its own currency. A part of class
cash, metal or crypto follows the rules of its class first, as every
leaf does (§3, §6). The parts are held directly.

## 3. What a security is

A leaf's sector and country come from the security it is. Gold keeps
one row per security (`securities`), rebuilt at every load from every
source that describes it. For each field it takes the best candidate
whose label a map knows, and keeps where it came from:

| Field | Order |
| --- | --- |
| sector | the newest fund line; a provider's sector; the SEC SIC code; a bond's type at OpenFIGI; a bond's description |
| country of risk | the newest fund line's country; a provider's risk country; the headquarters country (GLEIF, EDGAR's business address); the country a bond's type or description implies |
| country of domicile | the legal country (GLEIF); the country of incorporation (EDGAR); the ISIN prefix |
| market cap | a provider's market cap; for a US filer, EDGAR's shares outstanding times the newest US-dollar price a position held directly or a fund line implies |

A declaration (§9) wins over every source for the holding it names, in
each dimension it states.

The fund files are the security master: a stock held directly takes its
sector and country from any fund line that names the same security.

A bond held directly that no source gives a sector reads its broker's
description, as the last source of its sector. The rules apply in this
order:

- A bond under a US ISIN reads municipal, US, US dollars where its
  description holds a municipal word (GO, REV, AUTH, CNTY, SCH DIST and
  the like). Two weaker words (WTR, PUB, HSG and the like) do too. A
  legal form only a company has (INC, LLC, PLC) rules this out.
- A bond under a US ISIN that names a US state or territory, with only
  its form beside it, reads municipal too (STATE OF GEORGIA).
- A description that names a country, with only the issuer's form and
  the kind of paper beside it, reads government in that country
  (FINLAND 3.33% 2041, REPUBLIC OF AUSTRIA 3.33% 2041).
- A legal form (INC, CORP, LTD) reads corporate.

Two brokers whose descriptions read otherwise leave the bond as it is.
A country a source states stays. The origin is `description`.

A depositary receipt (an ADR, a GDR) stands for a share listed abroad.
Where no source states its sector or country, it takes the share's.
GLEIF ties the receipt's ISIN to its issuer's LEI and lists the
issuer's other ISINs (`issuer_isins`). Where one of them is an equity
line of the funds with a sector, that line is the share; of several,
the first by ISIN. GLEIF lists a share only
where the numbering agency of its home market reports it, so many
receipts stay without one. For those, gold finds the share by the
issuer's name, among the equity lines of the funds:

- A name loses what it says of the receipt (ADR, sponsored, each
  representing two shares). It also loses the legal form, single letters
  and numbers. A share's name keeps what its parentheses say.
- Each name the receipt goes by is matched on its own.
- Equal names match first. Else the receipt's words may lead the
  share's name, word by word, a word cut short allowed (ELECT for
  ELECTRIC). The first word must be equal, unless it has six letters or
  more and starts the share's first word. A name of one word leads only
  a share with one telling word (NAME BANK leads, NAME CHEMICAL does
  not).
- A share whose name adds a country (NAME INDIA for NAME) is a
  subsidiary there, not the issuer.
- The first step that finds any share decides. Where its shares do not
  fit, there is no match.
- A legal form places the company: SPA in Italy, OYJ in Finland. A
  share elsewhere does not fit. A receipt whose country a source states
  fits only a share of that country.
- A name that finds one issuer in one country votes for it. Any other
  name abstains. The votes must agree. Otherwise the receipt has no
  share and keeps only what sources state of it.

A fund report may name a holding by its name alone. Such a line finds
its share the same way, in two steps:

- First among the lines of the fund's proxy. The proxy is a fund of the
  same index whose lines carry identifiers. Its composition nearest the
  report's day serves. A share found there by a leading name yields to
  another share the world names exactly.
- Then, if the proxy has no match, among the shares of every country.

Both names lose what their parentheses say, a share's class and par
value (`CHF0.10`), legal forms and numbers. A line's country, where its
report states one, fits only a share of it. A name that finds shares in
two countries takes its fund's market: the country of two in three of
the fund's lines that found a share.

`security_underlyings` keeps each link and how it was found: `lei`,
`roster` (in the proxy's lines) or `name` (among all shares). The
security's attributes name the share as their origin: `lei:isin:…`,
`roster:` and the share's key, or `underlying:isin:…`. The leaves of a security
matched among all shares carry the flag `by_name`.

`--country risk` (the default) places a security in its country of
risk, else its domicile, on `countries` and `securities`.
`--country domicile` does the opposite. The other views refuse it.

### Size

A stock's size is `large`, `mid` or `small`:

- From its market cap, where one is known: large from 10bn US dollars,
  mid from 2bn, small below. A depositary receipt's own price and
  EDGAR's share count make none: its price is the receipt's, its shares
  the issuer's.
- Else from its rank in a size fund, a broad fund the config names
  (§9). Among the fund's stocks of its country, by falling weight, the
  first that hold 70% of the market are large, those up to 85% mid, the
  rest small. A fund that covers less than its whole market scales the
  rank. A large and mid cap fund (85%) holds no small stock.
- A stock in two size funds takes the one that covers more of its
  market, then the one with more stocks of its country.
- A size fund whose file names its stocks by name alone (a US issuer's
  file of foreign stocks) ranks them too. Each line finds the share it
  names, as a report's line does, and the share takes the line's size.
  That size rests on a name.
- A depositary receipt, and a line a report names by name alone, takes
  the size of the share it stands for (above).

`securities.size_origin` says which: `market_cap`, `size_fund:` and the
fund, `size_fund_name:` and the fund for a size found by name, or the
share as `lei:`, `roster:` or `underlying:` with its key. A
stock nothing sizes is `unknown`, and so is a fund nothing looks
through.

### Vocabularies

- **Sectors** are wealthdb's own words for the eleven broad sectors
  every vendor draws: `energy`, `materials`, `industrials`,
  `consumer_cyclical`, `consumer_staples`, `health_care`, `financials`,
  `technology`, `communication`, `utilities`, `real_estate`. A bond
  counts under who issued it: `government`, `municipal`, `corporate`,
  `securitised`. A holding that is not a company's security takes its
  asset class: `cash`, `metal`, `crypto`, `real_estate`. A private equity
  fund nothing looks through reads `diversified` (§5).
- **Countries** are ISO 3166 alpha-2 codes, each in one of five
  regions: `north_america`, `latin_america`, `europe`,
  `middle_east_africa`, `asia_pacific`. A sixth region, `other`, is the
  rest of the world a declaration states as a whole. No country
  belongs to it.
- **Asset classes** are the taxonomy's (TAXONOMY.md). A fund line's
  asset type maps onto one; a line without one takes its fund's class.

Each provider's labels map onto these in code
(`internal/canonical/exposurelabels.go`), one to one. A provider whose
words mean something else in the generic map has a map of its own: EDGAR
writes `DE` for Delaware, so its state and country codes read through
EDGAR's code list (`edgarcodes.go`), never the generic one. OpenFIGI's
type of a bond has a map too. `Govt`, `Muni` and `Corp` read as the
bond sectors of those names, and `Mtge` and `Pool` as `securitised`.
`Muni` also places a bond in the US, a territory's bond too. The map is
coarse. `Corp` holds supranational and covered bonds as well. A label
no map knows is **unmapped**: the leaf keeps no value for that field, the row
says so, and `coverage` lists the label. No vendor's scheme ships; the
maps name labels, never securities.

## 4. Time

Holdings and compositions are dated on different days. For a holding on
day D and fund F:

- Take the composition of F with the latest day on or before D, if it
  lies within `max_gap_days` of D (default 400, so last year's report
  still qualifies).
- Else take the one with the earliest day after D, within the same
  bound.
- Else the fund is a leaf.

The weights are the composition's own. The drift of prices between its
day and D is ignored: the error is small for broad buckets, larger for
single names, and `gap_days` states the distance. Two compositions are
never blended.

Where one day has several compositions, one is read: the fund's own
holdings before index lines, never a swap fund's collateral basket (§5),
and one source where two state the same day.

A holding whose mark is older than the day (a private fund's quarterly
NAV) still joins at the day: the composition describes the fund, the
mark describes the holding.

## 5. Special cases

- **Proxy.** The config names another fund whose composition stands in
  (`exposure.proxies`). By default a proxy fills only the days the fund
  has no composition for; `always` makes it replace the fund's own. The
  leaves carry the `proxy` flag. A proxy has no proxy of its own.
- **Leverage.** A proxy may carry a factor. A three-times fund is three
  times its proxy's lines and minus two times cash, in the proxy's
  currency. The total still equals the holding's value.
- **Swap funds.** A provider marks a swap fund's substitute basket as
  `collateral`. The look-through reads the index lines where the
  provider lists them, else the proxy, never the basket.
- **Hedged share classes.** A fund whose attributes name a hedged
  currency counts every leaf below it in that currency. The fund's own
  attributes decide, even where a proxy describes its lines.
- **Currency forwards.** A forward line that states its long leg at
  notional splits in two. The long leg counts in its own currency, at
  the leg's share of the fund's net assets. The rest of the line is the
  short leg. It counts in the short currency, else in the line's. The
  two sum to the line. A forward that states no legs, held directly or
  in a fund, counts in its own currency and is flagged `partial`.
- **Cash and derivatives inside a fund** stay leaves of their own asset
  type.
- **Private holdings.** No public source describes a property, a loan,
  a private fund, or a startup's shares and notes. The last are a stock
  or a bond in a private asset class. A declaration states such a
  holding's country, currency and sector, or a split of them (§9). A
  private equity fund nothing looks through reads the sector
  `diversified`, unless a declaration states one. It carries the
  `opaque` flag.
- **Structured products.** No provider reads the basket of a
  structured product (a certificate or a note). The basket comes
  through the refdata
  [inbox](../../collectors/refdata/README.md#inbox) instead, from the
  issuer's factsheet or term sheet. Its composition wins over a
  declaration. Without one, the product is a private holding. A
  declaration (§9) states its country, currency and sector, and
  `exposure coverage` lists it under `undeclared` until one does. A
  product whose asset class is metal or crypto counts by its class.

## 6. Currency

A leaf counts in the currency its value moves with, not the one it is
priced in:

- Cash and bonds count in their own currency; a stock in its trading
  currency, as the line states it.
- Gold counts as XAU, silver as XAG, platinum as XPT, palladium as XPD,
  held physically or through a fund the look-through does not split.
- A coin held in a wallet counts as its symbol. A product or a fund line
  that holds a coin counts as the coin its name states: bitcoin as BTC,
  ether as ETH. Any other coin reads `unknown`, unless a
  declaration states its currency (§9).
- A hedged class counts in its hedge currency (§5).

The `currencies` view marks each row's kind: `fiat`, `metal` or
`crypto`. A hedged leaf and both legs of a forward are `fiat`.

## 7. The views

`wealthdb exposure <view> [-d DATE] [--country risk|domicile] [-f FORMAT] [-C COLS] [-x CCY] [-p]`

`wealthdb exposure <view> --period monthly|quarterly|annual [FROM [TO]]`

| View | One row per | Answers |
| --- | --- | --- |
| `classes` | asset class | the allocation after look-through |
| `sectors` | asset class and sector | the sector weights, and each one's share within its class |
| `sizes` | asset class and size | large, mid and small caps (§3), and each one's share within its class |
| `countries` | country | where the value is, with each country's region |
| `currencies` | currency | what the value moves with |
| `credit` | credit bucket | how good the debt's credit is, investment grade or high yield |
| `maturities` | time to maturity | when the debt falls due |
| `coupons` | coupon kind | what the debt's coupons follow |
| `cycles` | cycle | how the stocks move with the economy |
| `situs` | legal situs | where each holding legally sits, each fund whole (§13) |
| `securities` | security | each security held directly and through every fund |
| `issuers` | issuer | who the value is owed by or invested in, the largest first |
| `funds` | fund held directly | what describes it on the day, and how old that is |
| `coverage` | reason | what the figures rest on (§10) |

Columns every aggregate view shares, by the names `-C` takes:

- `value`: the sum of the leaves, in the output currency.
- `share`: the share of net worth. A loan is a negative share.
- `direct` and `via_funds`: the part held directly and the part held
  through funds. They sum to `share`. `situs` has neither (§13).
- `quality`: the flags of §8.

`sectors` and `sizes` add `class_share`, the share within the asset
class. `credit` adds `grade`. `issuers` has no `direct` and
`via_funds`; it adds `lei`, `cumulative_share`, `equity`, `debt`,
`cash` and `securities`. A value prints as `value_<CCY>`. A share
prints with a `_pct` suffix, in a table's header and as a JSON key:
`share_pct`, `class_share_pct`, and `weight_sum_pct` in `funds`.
`wealthdb help exposure` lists every view's columns.

A row with no value for its dimension reads `unknown`. In
`countries`, a metal or a coin reads `(none)`: it has no country. A
part declared by region alone reads `unknown` with its region, in
a row for each region. In `sizes`, anything but public equity reads
`(none)`. A declared part of public equity has its holding's size: a
listed stock's, else `unknown`.

`securities` gives a leaf without a security a row of its own: cash per
currency, any other fund line by its fund and name, a holding by its
instrument, a fund's remainder. The parts of a declared holding are one
row, the holding's, with the class, sector, country and currency of its
largest part. The split itself shows in `classes`, `sectors`,
`countries` and `currencies`.

`credit`, `maturities` and `coupons` describe the debt: the leaves of
the fixed income and private debt classes. A holding reads its terms on
the day, a ledger record over a source (§9, yield records). A fund's
line reads what its fund states of it. A bucket no leaf falls in has no
row.

- `credit` has a row for each bucket from `AAA` to `B` (B and below),
  then `unrated`, `cash` and `(none)`. `grade` is `IG` from AAA to BBB
  and `HY` below. A fund that `yield.rating_funds` slices by rating, and
  its lines, read the range's worse end (YIELD.md §8). Any other debt
  leaf with no rating of its own splits by its fund's credit-quality
  breakdown, the fund's cash left out. A label the breakdown's map does
  not know (Government, Other) reads `unrated`. So does a debt leaf with
  neither, and both are flagged `unknown`.
- `maturities` buckets the time to maturity on the day: `under 1y`,
  `1-3y`, `3-5y`, `5-10y`, `10-20y`, `over 20y`. A maturity exactly N
  years on counts in the longer bucket; one past, under a year. A
  maturity a century on, or none on a name that says PERP, reads
  `perpetual`; a fund's own name says nothing of its lines. A cash
  balance or a time deposit matures with its term, where its terms or
  its account's rates state one, else counts under a year. A debt leaf
  with no maturity splits by its fund's maturity breakdown, each band by
  its lower bound, a band in months as its part of a year. An open band
  below 20 years (10+ Years) spans buckets and reads `unknown`. Else the
  leaf reads `unknown`.
- `coupons` reads `fixed`, `floating`, `inflation_linked` or `zero`. A
  line that states a real yield, or whose name says TIPS or inflation,
  is inflation-linked. Any other line reads its fund's type, else its
  name (FRN), else its coupon (zero, else fixed). Nothing stated reads
  `unknown`.

Cash has a row of its own in `credit`, and any leaf of no debt reads
`(none)` in all three. In `maturities`, `coupons` and `cycles`,
`unknown` is a row; in `credit` it is a flag on `unrated`.

`cycles` groups the public equity by its sector's cycle: `cyclical`,
`defensive` or `sensitive`, as `yield.cycles` overrides the built-in map
(YIELD.md §8). A stock or a fund nothing looks through takes its
sector's cycle, a declared one too. Without a sector it reads
`unknown`. Anything else reads `(none)`.

`issuers` puts a company's stock, bonds and deposits in one row. A leaf
belongs to an issuer by the first of these that it has:

- the LEI its fund line or GLEIF states;
- its name, read by a rule below, where a GLEIF legal name of the
  book's securities reads the same: that name's LEI;
- its name, read by that rule;
- for a cash balance or a time deposit, its custodian (§9), else its
  source;
- its security key, else its LEI, else `unknown`.

The rule reads a name as follows:

- accents, ß and Ø read as plain letters, the rest upper case;
- the words end before the first figure after the first word (a
  coupon, a date);
- N/B and A/S are one word, and U.S., UNITED STATES and TREAS read as US
  and TREASURY;
- punctuation and a leading THE drop;
- after the first word, a class letter (CLASS A), and the words of a
  legal form, a share class or a bond's kind (INC, CORP, PLC, AG, ADR,
  NOTE, MTN, SR UNSECURED) drop.

An issuer prints GLEIF's name, else its stock's, else its name as the
rule reads it. `cumulative_share` is the running share of net worth down
to the row. `equity`, `debt` and `cash` split the row's own value. The
leaves no issuer fits are one `(none)` row at the end:

- a fund nothing looks through, and a fund's cash and remainder;
- the parts a declaration splits a private holding into;
- real estate, a metal, a private holding of no debt.

`funds` lists each fund held directly, then one row for all other
holdings together. `securities` and `funds` therefore sum to net worth,
like the other views. A fund a declaration describes reads `via`
`declared`, with the record's `declaration_date` and the dimensions it
`declared`.

`-p` hides every amount and every instrument id a holding is named by.
Shares, names, labels and public identifiers stay.

`--period` shows how a view changes over a window. It runs the view at
the last second of every period in the window and puts a `period`
column first. The last period ends at the window's end, so a window to
today ends with today's figures. The window defaults to the last twelve
months. It starts no earlier than the first holding and ends no later
than today. It works on `classes`, `sectors`, `sizes`, `countries`,
`currencies`, `credit`, `maturities`, `coupons`, `cycles`, `situs` and
`coverage`. Each period is a separate look-through: its holdings and
compositions are the ones of its own day (§4). A security's attributes,
its market cap and size among them, are today's.

## 8. Quality

Each row says what its figure rests on. A flag carries the share of the
row's gross value it covers: `proxy=12%`, `aged=<1%`. `wealthdb help
exposure` and the MCP server's `describe topic=quality` list the same
flags. In `situs`, the quality column gives the bases of the situs
instead (§13).

| Flag | Meaning |
| --- | --- |
| `proxy` | another fund's composition stands in |
| `declared` | a declaration states it (§9) |
| `aged` | the composition is more than 35 days older than the day |
| `forward` | the composition is dated after the day |
| `partial` | the weights leave more than 3% of the fund unexplained (the remainder), a forward states no legs, or a fund lies deeper than three levels |
| `unmapped` | a provider's label has no place in the vocabulary |
| `by_name` | a depositary receipt's or a named line's sector or country comes from a share matched by name among all shares, not in a proxy's lines (§3) |
| `opaque` | a private equity fund nothing looks through; it reads `diversified` |
| `undeclared` | a private holding (§5) has no declaration on the day. A record that leaves a dimension out still declares the holding; one that states only a situs does not |
| `unmatched` | a listed security or fund no source describes |
| `no_composition` | in `funds` only: no composition and no declaration describes the fund on the day |
| `unknown` | in `credit` only: debt that no rating, rating fund or known label of its fund's page describes |

## 9. Config

The `exposure` block of `wealthdb.cfg` holds what no source knows
(DESIGN.md §5.1):

```json
"exposure": {
  "proxies": {
    "isin:XX0000000001": { "fund": "isin:XX0000000002", "always": true },
    "symbol:LEV3":       { "fund": "symbol:IDX1", "leverage": 3 }
  },
  "reference_funds": ["symbol:IDX9"],
  "size_funds": { "symbol:ALL9": 100, "isin:XX0000000003": 85 },
  "declarations": "~/ledgers/exposure-declarations.ndjson",
  "custody": { "bank-1": { "custodian": "Example Bank AG", "country": "CH" } },
  "max_gap_days": 400
}
```

- `proxies` maps a fund reference to its proxy (§5): `fund`, `always`
  (default false) and `leverage` (default 1; not zero, within ±10).
  Two keys may not name one fund. A fund may not be its own proxy, and
  a proxy may not have a proxy of its own.
- `reference_funds` are broad funds fetched for their lines alone. They
  widen the security master; they need not be held.
- `size_funds` are broad funds whose weights size the stocks no market
  cap sizes (§3). Each states the share of its markets' value it
  covers, in percent, from 50 to 100: 100 for an all-cap fund, 85 for a
  large and mid cap one. A small-cap fund is no size fund. They are
  fetched like reference funds.
- `declarations` is the path to the declarations ledger, below. It
  expands like every path in the config (DESIGN.md §5).
- `custody` names who keeps each configured source's assets:
  `custodian`, and optionally `country` (ISO 3166). A cash balance
  counts in its custodian's country, and its legal situs is that
  country, as is a vault's metal's (§13).
- `max_gap_days` bounds the temporal join (§4): 1 to 3650 days,
  default 400.

The block is checked when the config loads. DESIGN.md §7.6 says how a
load stores it.

### The declarations ledger

The declarations ledger states what no public source can know: where a
private holding's value is (§5), where any holding legally sits (§13),
and the yield figures no source states (below). It is newline-delimited
JSON, one record per line, for one holding, or one account for a rate,
and date:

```json
{"silver_source_id":"manual","instrument":"house-1","country":{"CH":100},"currency":{"CHF":100},"note":"the house"}
{"silver_source_id":"manual","instrument":"loan-1","country":{"CH":100},"currency":{"CHF":100},"note":"its mortgage"}
{"silver_source_id":"venture","instrument":"fund-7","as_of":"2026-03-31","region":{"north_america":55,"europe":30,"rest":15},"asset_class":{"private_equity":72,"private_debt":28},"currency":{"USD":90,"EUR":10},"source":{"document":"offering memorandum","page":9},"note":"states 50-60 / 25-35 / 10-20; midpoints"}
{"silver_source_id":"venture","instrument":"fund-7","situs":{"LU":100},"note":"its domicile"}
```

- `silver_source_id` and `instrument` name the holding, as `exposure
  coverage` prints them.
- `as_of` is the date of the document the record comes from,
  `YYYY-MM-DD`. A record without one holds before the holding's first
  dated record (below).
- A record declares at least one dimension, a `situs`, or one of the
  yield records below. Each dimension is an object of values and their
  shares:
  - `region`: a region (§3), or `rest` for the rest of the world, which
    reads as the region `other`;
  - `country`: an ISO 3166 code. A country implies its region, so a
    record declares a region or a country, not both;
  - `currency`: a code of three capital letters, such as `USD`, `XAU`
    for gold or `BTC` (§6);
  - `sector`: a sector (§3);
  - `asset_class`: an asset class (TAXONOMY.md).
- `unknown` is a value of every dimension: the part the record
  leaves open.
- `situs` is the holding's legal situs (§13): one ISO 3166 code at 100,
  such as `{"KY": 100}`. A split, a region, `EU` and `unknown` are
  refused. A record that states only a `situs` is no record for the
  other dimensions: they read the holding's other records as if it were
  not there.
- A share is a number in percent, from 0 to 100. A dimension's shares
  sum to 100, within half a point for rounding. What they leave of 100,
  or take beyond it, counts as `unknown`, so the dimension sums to
  exactly 100.
- A document often states a range ("40–50%"). The record states one
  number, usually the middle, and the range goes into `note`.
- `source` and `note` are free. `source` may be any JSON value, such as
  the document and its page.
- Any other key is refused, so a misspelt dimension cannot drop a split.
  So is a key stated twice.
- A holding has one record per date that states its dimensions, and one
  per date that states its situs. A record may state both. So a
  situs-only line sits beside the record of a split of the same date.
- A load reads the ledger before it opens gold, so a bad record stops
  it early. The error names the line.
- A missing file holds no declarations.

A record states each dimension on its own. The holding splits into one
part per combination of the values. A part's share is the product of
its values' shares. A fund declared 55/30/15 by region and 72/28 by
class has six parts. Every view of one dimension then shows exactly the
declared split, and the totals still reconcile. A view that combines two
dimensions, such as `sectors` within a class, assumes they are
independent: the record does not say which region holds which class.

An `asset_class` split gives each part its class, so `classes` shows a
fund that is part private equity, part private debt in both rows. The
part the record leaves `unknown` keeps the holding's own class.

On a day, the record that describes a holding is the latest one dated on
or before that day that states a dimension. A record without a date
serves the days before the first dated one. A record has no age limit: a
target split does not go stale as a fund's list of holdings does. Before
its first record, a holding is undeclared. The situs has a precedence of
its own (§13).

`wealthdb exposure coverage -f ndjson` prints a skeleton record for
each holding still to declare:

- a private holding without a record (the section `undeclared`) gets
  one with an empty `country`, `currency` and `sector`;
- a holding whose situs is `unknown` gets one with an empty
  `situs` alone. It loads beside the holding's other records.

Its note is the holding's name. A record filled in from the skeleton
goes into the ledger, and the next load takes it. An empty dimension is
refused, so a skeleton loads only once each dimension is filled in or
taken out. A holding its source names by no instrument has an empty
`instrument` and cannot take a record. A holding with no value, in the
`(no value)` line, takes no skeleton.

A load warns of a record whose holding, or whose account for a rate, no
source states, with its line: such a record declares nothing, and a
misspelt instrument would otherwise show only as a holding still
undeclared.

### Yield records

The same ledger states the yield figures no source states. A record
carries one or more of four objects, beside its dimensions or alone:

```json
{"silver_source_id":"bank-1","account":"savings-1","as_of":"2026-01-01","rate":{"pct":0.75}}
{"silver_source_id":"broker-1","instrument":"bond-1","rating":{"bucket":"AA","agency":"Example Ratings"}}
{"silver_source_id":"broker-1","instrument":"QSTK","dividend":{"amount":0.42,"frequency":4}}
{"silver_source_id":"broker-1","instrument":"bond-1","terms":{"frequency":2,"call_date":"2028-06-15"}}
{"silver_source_id":"plan-1","instrument":"sleeve-1","as_of":"2026-09-30","terms":{"duration":6.1,"yield":4.4}}
```

- `rate` is an account's interest rate, from the record's day on. It
  names an `account` instead of an `instrument`, and states nothing
  else. It takes `pct` (percent a year), and optionally `currency` (for
  an account in several, one record per currency and date) and `until`
  (a term deposit's end).
- `rating` is a holding's credit bucket: `bucket` is one of AAA, AA, A,
  BBB, BB, B (B and below) or `unrated`. `agency` is optional.
- `dividend` is a stock's indicated dividend: `amount` per share of one
  payment and `frequency`, the payments a year (1, 2, 4 or 12), and
  optionally `currency`.
- `terms` is what a bond's, a note's or a mortgage's terms state: any of
  `coupon` (percent a year), `frequency` (0 for a discount instrument),
  `maturity`, `call_date`, `fixed_until`, `reset_date`, `coupon_kind`
  (fixed, floating, inflation_linked, zero) and `day_count` (30/360,
  30E/360, ACT/ACT, ACT/360, ACT/365). A fund that files nowhere public
  states its factsheet's `duration` (years) and `yield` (percent) here.

A date is `YYYY-MM-DD`. Any other key is refused, and so are terms that
contradict one another: a coupon with a frequency of 0 or the kind
zero, a call date or a fixed period after the maturity. A holding or an
account has one record of each kind per date. On a day, the record that
holds is the latest one dated on or before it, else the one without a
date; a newer record replaces an older one whole. A record of these
alone is no exposure record: the holding reads its other records, or
none, for its dimensions. A field the record in force states wins over
the same field from any source.

## 10. Coverage

`wealthdb exposure coverage` says what the figures rest on, in seven
sections. With `-f ndjson` it prints the skeleton records of §9
instead.

- `how` splits net worth by how each leaf is classified, and sums to
  it: `looked_through`, `proxied`, `security` (a listed security a
  source describes; its ISIN's country prefix alone does not count),
  `declared`, `cash`, `asset_class` (metal, crypto), `remainder`,
  `truncated` (a fund deeper than three levels) and `unknown`.
- `age` splits what the compositions describe by their distance from
  the day: `0-35 days`, `36-95 days`, `96-185 days`, `186+ days` and
  `forward`.
- `unmapped` lists each provider label no map knows, with the value it
  touches.
- `unmatched` lists each listed security held directly that no source
  describes and no declaration covers.
- `undeclared` lists each private holding no declaration describes on
  the day.
- `divergence` lists each fund held and read from a report whose config
  names a proxy with a composition of its own on the day. It gives the
  value held through the fund. Its detail says how far the report's
  equity lines lie from the proxy's, by sector and by country. The
  measure is half the sum of the differences in each sector's or
  country's share, in points. Each mix holds only the lines that state
  that dimension. A dimension one side states for no line is left out.
- `situs` splits net worth by the basis of each holding's legal situs
  (§13), in the rule's order, and sums to it. Then it lists each holding
  whose situs is `unknown`. Its items count holdings, not leaves.

`unmatched`, `undeclared` and the holdings of `situs` name each holding
by `silver_source_id` and `instrument`, the two keys a declaration
starts with. A holding at two sources has a line for each. In
`unmapped`, `unmatched`, `undeclared` and `situs`, the labels and
holdings with no value share one line, `(no value)`, with their count.
A label or holding whose leaves offset each other still has value and
keeps its own line.

## 11. What exposure is not

- Not a price feed. A fund's weights are not repriced to the day.
- Not a risk model. No volatility, no correlation, no factor.
- Not revenue exposure. A company counts in one country, where its risk
  or its domicile is.
- Not gains. A fund's unrealized gain stays with the fund; the
  look-through does not split it across its lines.
- Not the legal situs. A fund counts in its lines' countries here.
  `situs` says where each holding legally sits (§13).

## 12. The dashboard and MCP

The Metabase Exposure dashboard (`wealthdb web`) charts the same
figures at every month end and today, by country of risk, and each
holding's legal situs ([web/DESIGN.md](../../web/DESIGN.md) §6 and
§8). Its Source, Tax wrapper and Asset class pickers narrow every
chart to a selection. A share then reads as a share of the selection's
net worth. With nothing picked, it is a share of net worth, as here.
The command has no such filter. Every refresh recomputes the whole
history, so a new composition or declaration shows in past months too.
A security's attributes, its size among them, are today's at every
month end, as in a `--period` series (§7). Five cards chart `credit`,
`maturities`, `coupons`, `cycles` and the largest issuers, as of a day
of the last twelve months: the refresh walks those views for today and
the 12 month ends before. The issuers card follows no source or tax
wrapper picker.

The MCP server has an `exposure` tool with the same fourteen views, an
`as_of` day and the `country` choice, but no `--period` series. Its
rows equal the command's, cell for cell.

## 13. Legal situs

`wealthdb exposure situs` says where each holding legally sits. The
situs of a holding is the one country whose law it is an interest
under:

- a fund, an ETF, a feeder or a plan sleeve: where the vehicle is
  domiciled;
- a share: where its issuer is incorporated. A depositary receipt
  reads as the share it stands for;
- a bond or a structured product: its issuer's country;
- cash: the country of the bank that holds it, since a deposit is a
  claim on that bank. Metal in a vault: its custodian's country;
- a property: where it stands. A private loan: the debtor's country;
- a coin: no situs the engine can know.

A holding has one situs, never a split. Its region is its country's.
What a law makes of a situs, such as a treaty or an exception for bank
deposits, is outside the engine. The view gives the country and what it
rests on.

### The rule

For each holding, the first of these that applies:

1. `declared`: a declaration states its `situs` (§9).
2. `underlying`: it is a depositary receipt whose share is known (§3).
   It takes the share's situs, by the rules below. A receipt whose
   share nothing names stays `unknown`: its US ISIN says where the
   receipt was issued, not where the issuer is.
3. `isin`: the prefix of an ISIN a source states, where the prefix is a
   country. A prefix that is no country gives nothing: XS, XA to XD,
   and EU, the European Union. Nor does an ISIN only the US CUSIP rule
   gives: it is a guess.
4. `domicile`: the holding's country of domicile, where it has no ISIN
   of a country. It comes from:
   - a fund's attributes;
   - else, for a fund whose latest composition is an SEC N-PORT filing,
     the US: only a US registered investment company files one;
   - else a security's legal country at GLEIF, or its state of
     incorporation at EDGAR (§3).
5. `custody`: cash at a bank and metal in a vault take the country of
   their source's custodian (`exposure.custody`, §9). Nothing else does:
   an ETF kept at a Swiss bank is not Swiss. Cash swept into a money
   market fund is a fund and reads by rules 3 and 4.
6. `country`: a private holding (§5) takes the one country its
   declaration in force on the day states at 100 (§9). This holds only
   where no composition of its own describes it. A split or a region
   states nothing here.
7. `unknown`.

The declared situs has a precedence of its own. On a day, it is the one
the latest record on or before that day states; a record without a date
counts as before every dated one. A later record that states no situs
leaves it standing. What a situs-only record does to the other
dimensions is in §9.

A depositary receipt is a holding that a lookup or GLEIF calls one
(§3), or whose name says so (ADR, GDR). A fund is never one, and nor is
a holding whose ISIN is of a country other than the US.

A coin held in a wallet has no issuer. Only a declaration places it.

An issuer's own code in the shape of an ISIN gives nothing where its
prefix is no country. Under a country's prefix it reads as an ISIN.

### No look-through

The situs never looks through a fund. A fund's whole value sits at its
own situs; its lines, a proxy and a composition's date play no part.
Every leaf of the look-through carries the situs of the holding it
comes from, so the view sums to `holdings global` like every other
view.

### Three readings of country

| Reading | View | What it answers |
| --- | --- | --- |
| country of risk | `countries`, the default | where the value is exposed: each fund looked through, each company in its country of risk |
| country of domicile | `countries --country domicile` | the same look-through, each company in the country it is incorporated in |
| legal situs | `situs` | where each holding legally sits, each fund whole at its own |

### The view

`situs` has one row per situs, `unknown` among them. Its columns
are `situs`, `country_name`, `region`, `value`, `share`, `funds` and
`quality`:

- `funds` is the share of the row's gross value held in pooled
  vehicles, the vehicles `etf` and `fund` (TAXONOMY.md).
- `quality` gives the share of the row's gross value each basis
  covers, in the rule's order: `declared=12%,isin=80%`.
- There is no `direct` or `via_funds`: nothing is looked through.

`funds` shows each fund's `situs` and `situs_basis` beside its
`domicile`, so a declared situs that differs from the domicile shows.
A fund held in two accounts takes the situs of its larger holding.
`coverage` lists each holding whose situs is `unknown` (§10).
