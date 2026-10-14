# Exposure

What is really owned. The exposure report looks through every fund to
what it holds, down to the single security, and adds the result to
what is held directly. It answers by asset class, sector, country,
currency and security, as of any day. This file defines every figure
the report shows and the rules behind them.

Related design: DESIGN.md §4.17 (the `exposure` command), §7.6 (the
reference tables), §10.14 (the macros);
[adapters/refdata.md](adapters/refdata.md) (how reference data loads,
and the wanted list); migrations 0125 to 0129.

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
- **An alias** reads one key as another. A fund line that states a
  ticker beside an ISIN makes the ticker an alias of the ISIN, so a
  stock held under its ticker and a line that names its ISIN are one
  security. The line must name a US listing (a US ISIN, a US exchange,
  or no exchange and a US-dollar price): a local ticker abroad is often
  another company's US symbol.
- **A leaf** is what the look-through ends at: a holding no composition
  describes, a fund's line, a remainder, or the cash leg of a leveraged
  proxy.
- **Net worth** is the sum of every leaf, as `holdings global` reports
  it. A loan is a negative leaf.

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
   security aliases.
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

A holding without a composition is a leaf of its own. A listed
security takes its attributes from §3. A fund takes its tags alone,
held directly or named by a line no level expands: where it is
domiciled says nothing of where its value is. A real estate fund keeps
the `real_estate` sector. Anything else takes its tags (§9), or stays
unclassified.

## 3. What a security is

A leaf's sector and country come from the security it is. Gold keeps
one row per security (`securities`), rebuilt at every load from every
source that describes it. For each field it takes the best candidate
whose label a map knows, and keeps where it came from:

| Field | Order |
| --- | --- |
| sector | a vendor; the newest fund line; another provider's sector; the SEC SIC code |
| country of risk | a vendor's risk country; the newest fund line's country; another provider's risk country; the headquarters country (GLEIF, EDGAR's business address) |
| country of domicile | the legal country (GLEIF); the country of incorporation (EDGAR); the ISIN prefix |
| market cap | a vendor; another provider's market cap |

A tag in the config wins over every source for the holding it names.
The fund files are the security master: a stock held directly takes its
sector and country from any fund line that names the same security.

A depositary receipt (an ADR, a GDR) stands for a share listed abroad.
Where no source states its sector or country, it takes the share's. No
free source says which share an over-the-counter receipt stands for. So
gold finds the share by the issuer's name, among the equity lines of the
funds:

- A name loses what it says of the receipt (ADR, sponsored, each
  representing two shares). It also loses the legal form, single letters
  and numbers. A share's name keeps what its parentheses say.
- Equal names match first. Else the receipt's words may lead the
  share's name, word by word, a word cut short allowed (ELECT for
  ELECTRIC). A name of one word leads only a share with one telling
  word (NAME BANK leads, NAME CHEMICAL does not).
- A share whose name adds a country (NAME INDIA for NAME) is a
  subsidiary there, not the issuer.
- The first step that finds any share decides. Where its shares do not
  fit, there is no match.
- A legal form places the company: AG in Germany, Switzerland or
  Austria, SPA in Italy. A share elsewhere does not fit. A receipt whose
  country a source states fits only a share of that country.
- What the names find must be one issuer in one country. Anything else
  stays `unmatched`.

`security_underlyings` keeps each link. The receipt's attributes name
the share as their origin (`underlying:isin:…`), and its leaves carry
the flag `by_name`.

`--country risk` (the default) places a security in its country of
risk, else its domicile. `--country domicile` does the opposite.

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
  `middle_east_africa`, `asia_pacific`.
- **Asset classes** are the taxonomy's (TAXONOMY.md). A fund line's
  asset type maps onto one; a line without one takes its fund's class.

Each provider's labels map onto these in code
(`internal/canonical/exposurelabels.go`), one to one. A provider whose
words mean something else in the generic map has a map of its own: EDGAR
writes `DE` for Delaware, so its state and country codes read through
EDGAR's code list (`edgarcodes.go`), never the generic one. A label no map
knows is **unmapped**: the leaf keeps no value for that field, the row
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
  notional splits in two: the long leg in its currency, at the leg's
  share of the fund's net assets, and the rest of the line, the short
  leg, in the short currency, else the line's. The two sum to the
  line. A forward that states no legs, held directly or in a fund,
  counts in its own currency and is flagged `partial`.
- **Cash and derivatives inside a fund** stay leaves of their own asset
  type.
- **Private holdings.** No public source describes a property, a loan,
  a private fund, or a startup's shares and notes. The last are a stock
  or a bond in a private asset class. A tag states such a holding's
  country, currency and sector. A private equity fund nothing looks
  through reads the sector `diversified`, unless a tag says otherwise. It
  carries the `opaque` flag.

## 6. Currency

A leaf counts in the currency its value moves with, not the one it is
priced in:

- Cash and bonds count in their own currency; a stock in its trading
  currency, as the line states it.
- Gold counts as XAU, silver as XAG, platinum as XPT, palladium as XPD,
  held physically or through a fund the look-through does not split.
- A coin held in a wallet counts as its symbol. A product or a fund line
  that holds a coin counts as the coin its name states: bitcoin as BTC,
  ether as ETH. Any other coin reads `unclassified`.
- A hedged class counts in its hedge currency (§5).

The `currencies` view marks each row's kind: `fiat`, `metal` or
`crypto`. A hedged leaf and both legs of a forward are `fiat`.

## 7. The views

`wealthdb exposure <view> [-d DATE] [--country risk|domicile] [-f FORMAT] [-C COLS] [-x CCY] [-p]`

`wealthdb exposure <view> --period monthly|quarterly|annual [FROM [TO]]`

| View | One row per | Answers |
| --- | --- | --- |
| `classes` | asset class | the allocation after look-through |
| `sectors` | asset class and sector | the sector weights, and `class_share_pct` within the class |
| `countries` | country | where the value is, with each country's region |
| `currencies` | currency | what the value moves with |
| `securities` | security | each security held directly and through every fund |
| `funds` | fund held directly | what describes it on the day, and how old that is |
| `coverage` | reason | what the figures rest on (§10) |

Columns every aggregate view shares:

- `value_<CCY>`: the sum of the leaves, in the output currency.
- `share_pct`: the share of net worth. A loan is a negative share.
- `direct_pct` and `via_funds_pct`: the part held directly and the part
  held through funds; they sum to `share_pct`.
- `quality`: the flags of §8.

A row with no value for its dimension reads `unclassified`. In
`countries`, a metal or a coin reads `(none)`: it has no country.

`securities` gives a leaf without a security a row of its own: cash per
currency, any other fund line by its fund and name, a holding by its
instrument, a fund's remainder. `funds` lists each fund held directly,
then one row for all other holdings together. Both therefore sum to net
worth, like the other views.

`-p` hides every amount and every instrument id a holding is named by.
Shares, names, labels and public identifiers stay.

`--period` shows how a view changes over a window. It runs the view at
the last second of every period in the window and puts a `period`
column first. The last period ends at the window's end, so a window to
today ends with today's figures. The window defaults to the last twelve
months. It starts no earlier than the first holding and ends no later
than today. It works on `classes`, `sectors`, `countries`, `currencies`
and `coverage`. Each period is a separate look-through: its holdings
and compositions are the ones of its own day (§4).

## 8. Quality

Each row says what its figure rests on. A flag carries the share of the
row's gross value it covers: `proxy=12%`, `aged=<1%`.

| Flag | Meaning |
| --- | --- |
| `proxy` | another fund's composition stands in |
| `aged` | the composition is more than 35 days older than the day |
| `forward` | the composition is dated after the day |
| `partial` | the weights leave more than 3% of the fund unexplained (the remainder), a forward states no legs, or a fund lies deeper than three levels |
| `unmapped` | a provider's label has no place in the vocabulary |
| `by_name` | a depositary receipt's sector or country comes from a share matched by name (§3) |
| `opaque` | a private equity fund nothing looks through; it reads `diversified` |
| `untagged` | a private holding (§5) has no tag. A tag that leaves a field blank is still a tag |
| `unmatched` | a listed security or fund no source describes |
| `no_composition` | in `funds` only: no composition describes the fund on the day |

## 9. Config

The `exposure` block of `wealthdb.cfg` holds what no source knows
(DESIGN.md §5.1):

```json
"exposure": {
  "proxies": {
    "isin:XX0000000001": { "fund": "isin:XX0000000002" },
    "symbol:LEV3":       { "fund": "symbol:IDX1", "leverage": 3 }
  },
  "reference_funds": ["symbol:IDX9"],
  "tags": {
    "manual": { "house-1": { "country": "CH", "currency": "CHF" } }
  },
  "custody": { "bank-1": { "custodian": "Example Bank AG", "country": "CH" } },
  "max_gap_days": 400
}
```

- `proxies` maps a fund reference to its proxy (§5).
- `reference_funds` are broad funds fetched for their lines alone. They
  widen the security master; they need not be held.
- `tags` state the sector, country and currency of a holding no
  composition describes, keyed by source and instrument like
  `instrument_overrides`.
- `custody` names who keeps each source's assets. A cash balance counts
  in its custodian's country.
- `max_gap_days` bounds the temporal join (§4).

## 10. Coverage

`wealthdb exposure coverage` says what the figures rest on, in four
sections:

- `how` splits net worth by how each leaf is classified, and sums to
  it: `looked_through`, `proxied`, `security` (a listed security a
  source describes; its ISIN's country prefix alone does not count),
  `tagged`, `cash`, `asset_class` (metal, crypto), `remainder`,
  `truncated` (a fund deeper than three levels) and `unclassified`.
- `age` splits what the compositions describe by their distance from
  the day: `0-35 days`, `36-95 days`, `96-185 days`, `186+ days` and
  `forward`.
- `unmapped` lists each provider label no map knows, with the value it
  touches.
- `unmatched` lists each holding no source describes and no tag covers.

In the last two, the labels and holdings with no value share one line,
`(no value)`, with their count. A label or holding whose leaves offset
each other still has value and keeps its own line.

## 11. What exposure is not

- Not a price feed. A fund's weights are not repriced to the day.
- Not a risk model. No volatility, no correlation, no factor.
- Not revenue exposure. A company counts in one country, where its risk
  or its domicile is.
- Not gains. A fund's unrealized gain stays with the fund; the
  look-through does not split it across its lines.

## 12. The dashboard and MCP

The Metabase Exposure dashboard (`wealthdb web`) charts the same
figures:

- on the latest day in its time range, by asset class, sector, region,
  country and currency;
- at every month end in the range, as shares of net worth over time;
- split into what is held directly and what through funds, and by how
  each part is classified (§10).

`wealthdb web-materialize` stores the look-through at every month end
and at today, in each reporting currency (web/DESIGN.md §8). A refresh
recomputes the whole history, so a new composition or tag shows in
past months too. The privacy twin shows the shares alone.

The MCP server has an `exposure` tool with the same seven views, an
`as_of` day and the `country` choice. Its rows equal the command's,
cell for cell.
