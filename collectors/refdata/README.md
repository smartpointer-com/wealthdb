# refdata

Part of the **wealthdb** suite. See [the architecture overview](../../DESIGN.md)
for the bronze → silver → gold model and [collectors/README.md](../README.md)
for the shared collector conventions.

A collector for **reference data**: what each fund holds, and what each
security is. The engine's exposure report
([EXPOSURE.md](../../wealthdb/docs/EXPOSURE.md)) reads it to look
through every fund to the securities underneath.

`refdata` reads only what gold asks for. The engine prints a **wanted
list**: the funds held, the funds the config names (proxies, reference
and size funds), the funds a held fund holds, and the securities held
directly or through funds. `refdata` fetches what is missing or old
from public sources and loads it into its silver. `wealthdb load`
brings it into gold.

There is no login anywhere. Every provider reads public pages.

## Use

```sh
./refdata download --dry-run       # plan a run and print what it would fetch
./refdata download                 # fetch what the wanted list lacks
./refdata download --backfill      # also fetch past dates while each fund was held
./refdata load                     # parse bronze into silver
wealthdb load refdata              # bring it into gold (the source's id in wealthdb.cfg)
wealthdb exposure coverage         # see what is described, and what is not
```

`download` asks the engine for the wanted list (`wealthdb -r
refdata-wanted`). `--wanted FILE` reads a saved list instead. A dry run
still makes the lookups that find each fund, such as a product list. A
run can be narrowed:

- `--provider NAME` asks only that provider (repeatable). A name
  `refdata.cfg` does not enable stops the run.
- `--ref REF` fetches only that fund or security (repeatable).
- `--limit N` makes at most N fetch attempts. The lookups that find a
  fund are not counted. A request left without an attempt is recorded
  as deferred.
- `--no-securities` skips the security lookups.

`login` is a no-op, since there is no session. `login --check` asks each
enabled provider for its cheapest answer (a product list, one lookup)
and reports what it answered. A provider with nothing cheap to ask (the
inbox, xtrackers, invesco) says so.

A fund counts as current while its newest composition is younger than
`max_age_days`. Past that age, a provider that can name its newest date
is asked first, and a composition already had is not fetched again. A
composition fetched by an earlier run counts as had before it reaches
gold. A security looked up within `max_age_days` is not looked up
again. So a second `download` on the same day fetches nothing.

## Settings

The settings live in `$XDG_CONFIG_HOME/refdata.cfg`, by default
`~/.config/refdata.cfg`. The file is JSON:

```json
{
  "providers": ["inbox", "ishares", "invesco", "spdr", "sec-nport", "sec-edgar", "gleif", "openfigi"],
  "contact": "name@example.org",
  "max_age_days": 30,
  "engine": "/path/to/wealthdb",
  "constituents": true,
  "constituent_floor_pct": 0.0001,
  "fidelity_529_plans": {"XX": "https://www.example.com/xx-annual-report.pdf"}
}
```

- `providers` are asked in this order. The first that claims a fund
  serves it; the inbox serves beside it. A fund is offered only while it
  wants a composition. The next provider claims it only when the fetch
  of the one before fails. A provider standing by also reads the
  identifiers the first one found. So N-PORT finds a fund named by its
  ISIN by the ticker iShares lists for it. Every security provider is
  asked for every security held directly that it can claim. A security
  held through funds is asked only of a provider that can fill what it
  lacks.
- `max_age_days` (1 to 3650, default 30) is how old a composition or a
  lookup may be before it is fetched again.
- `constituents` lets the security providers look up the securities a
  held fund's lines name that no source describes yet (an N-PORT filing
  states no sector), the biggest share of net worth first. Off by
  default: a broad fund names thousands, and OpenFIGI takes 25 requests
  a minute. Securities held directly are always looked up.
- `constituent_floor_pct` skips a security held through funds whose
  share of net worth, in percent, is below it. The default, 0.0001, is
  one millionth of net worth.
- `contact` goes into the User-Agent of requests to the SEC, and to no
  other host. The SEC requires it. The providers that ask the SEC
  (`sec-nport`, `sec-edgar` and `fidelity-529`) do not run without it.
  It is kept in this file only, never in bronze, silver or a log.
- `engine` is the wealthdb wrapper. `REFDATA_WEALTHDB` overrides it.
  Without either, the collector asks the wrapper of its own checkout,
  which runs that checkout's engine image; outside a checkout,
  `wealthdb` on PATH.
- `fidelity_529_plans` names the 529 plans `fidelity-529` reads. Each
  entry maps the prefix a broker writes before a plan's portfolios, one
  word, to the https URL of the plan's annual report. Without it, that
  provider does not run.

With no file, the providers of public data run: `inbox`, `sec-nport`,
`sec-edgar`, `gleif` and `openfigi`. The two SEC providers wait for a
`contact`. An issuer's provider (`ishares`, `xtrackers`, `invesco`,
`amundi`, `spdr`, `sprott`) and a report provider (`swisscanto`,
`swissfunddata`, `fidelity-529`) run only once `providers` names them.
Enabling one accepts its source's terms, quoted below. An issuer's
daily file is fresher than a report or a quarterly N-PORT filing, so
list the issuers first, then the report providers, then `sec-nport`.

A setting or a provider name the collector does not know is an error,
so a typo never passes for a switch that is off.

## Providers

### inbox

A composition obtained some other way: a document a fund sent, a
basket from a note's term sheet. It goes into a folder named after the
fund and the day it describes:

```
<data-dir>/inbox/isin_XX0000000001/2026-06-30/holdings.csv
```

The folder name is the fund's reference, with `_` in place of `:`.
`wealthdb -r refdata-wanted` prints the reference of every fund the
collector reads, a structured product among them. A folder holds one
composition file:

- a holdings table, CSV or XLSX: an issuer's own download, or a file
  made by hand. Its header row names at least a holding (`name`) and
  its `weight` (percent of net assets), and may name `isin`, `cusip`,
  `ticker`, `country`, `currency`, `sector`, `industry`, `asset_type`,
  `quantity` and `market_value`, in an issuer's wording too. Lines above
  the header and notes below the table are skipped;
- or an N-PORT `primary_doc.xml`.

A download copies each folder into bronze once, beside whatever
provider serves the fund. The inbox itself is never changed.

A structured product's basket comes only this way. No provider reads
one. Exchange reference data lists no constituents, and issuers'
product pages do not allow automated download.

A factsheet that states a split rather than a roster (40% Europe, 35%
North America, 25% the rest) has no lines to look through, and
neither has a private fund's offering document. Such a split goes into
the engine's declarations ledger, one record per holding and date
([EXPOSURE.md §9](../../wealthdb/docs/EXPOSURE.md#9-config)):

```json
{"silver_source_id":"bank","instrument":"XX0000000001","as_of":"2026-06-30","region":{"europe":40,"north_america":35,"rest":25},"currency":{"EUR":64,"USD":36},"source":{"document":"factsheet"}}
```

No provider reads factsheets. One that does would write the same shape
into silver, as a composition of basis `breakdown`.

### sec-nport

US registered funds and ETFs, from their N-PORT filings on SEC EDGAR.

- The fund is found by its ticker in the SEC's ticker file.
- Each filing lists every holding: ISIN, CUSIP, ticker, value, weight,
  currency, asset category and the issuer's country. A currency forward
  states both legs; a derivative names its counterparty.
- `--backfill` reads every quarterly filing inside the days the fund
  was held, and the last one before them: the composition in force when
  the holding began.

N-PORT carries no sector. Public filings are quarterly and two to five
months old, so a composition is often flagged `aged` in the report.

**Terms.** "Information presented on sec.gov is considered public
information and may be copied or further distributed by users of the web
site without the SEC's permission." The SEC's fair-access rules allow at
most ten requests a second and require a User-Agent that names the tool
and a contact address. The collector sends
`wealthdb-refdata/1 <contact>` and keeps to the limit.

### spdr

State Street's SPDR ETFs, US and UCITS, from the daily holdings file
each fund page links.

- The fund is found by its ISIN or CUSIP in State Street's fund finder,
  or by its ticker among the US funds.
- The file lists every holding with its ISIN (UCITS) or CUSIP (US),
  weight, shares, currency and sector; a UCITS file adds the trading
  country and the industry. A cash line states no weight and counts in
  the fund's remainder.
- The file is today's holdings; there is no history.

**Terms.** State Street's site terms allow "producing print-outs for
your own personal use", and forbid publishing or redistributing the
content "for other than use for or on behalf of yourself". Its
robots.txt leaves the fund finder and the holdings files open. Each
file's footer adds: "The whole or any part of this work may not be
reproduced, copied or transmitted or any of its contents disclosed to
third parties without State Street Investment Management's express
written consent." Off until `providers` names it.

### sprott

Sprott's US ETFs, from the holdings table each fund page embeds.

- The fund is found by its ticker among the fund pages the site's home
  page links.
- The table lists every holding with its name, weight, shares and
  market value, and its ticker. A ticker with an exchange code after it
  names a listing abroad and is kept aside, not read as a US symbol. The
  SEDOL column is not read.
- The table is today's holdings; there is no history.

**Terms.** The site links no terms of use (checked 2026-10-14); its
privacy policy covers personal data only. The holdings are the fund's
own publication, read once a run. Off until `providers` names it.

### swisscanto and swissfunddata

A fund no daily file describes, from its newest annual and semi-annual
reports.

- `swisscanto` finds a Swisscanto fund by its ISIN in Swisscanto's
  product API, which links its umbrella's reports.
- `swissfunddata` finds a fund distributed in Switzerland by its ISIN
  in the document list of swissfunddata.ch.
- Each report is a PDF with an inventory per sub-fund. The held fund's
  is the one whose title its name names. Both reports are read, so a
  fund has two compositions a year.
- A report is months old the day it appears, so its composition reads
  as aged. It is fetched again once max_age_days has passed since the
  last fetch.
- Swisscanto's inventory lists each position's ISIN. Other reports list
  names only, under industry or country headings. Gold finds each
  name's share among the shares the other funds' files list.
- swissfunddata shows its document list only after a visitor states an
  investor category and agrees to its disclaimer. The provider states
  "private investor" and agrees, once a run. The site keeps that in a
  session cookie. No account, no login.

**Terms.** Swisscanto's legal notice prohibits accessing the site's
contents systematically "to compile a collection, a database or a
directory". swissfunddata's disclaimer bars professional market data
distributors from using, systematically extracting, scraping or
commercially exploiting its data, and requires consent for any public
or commercial use. The reports are the funds' own publications, read for
the funds held, at most once in max_age_days. Off until `providers`
names them.

### fidelity-529

The portfolios of a Fidelity-run state plan, from the plan's annual
report. The report URL and the broker's name prefix are configured
(`fidelity_529_plans`).

- A portfolio is claimed by its name, as brokers write it: the prefix,
  then "… PORT …". No request.
- The report lists each portfolio's Fidelity funds with their shares and
  value, as of the plan's year end. A fund's weight is its value's share
  of the portfolio's net assets.
- The SEC's list of fund series and classes names each fund's ticker.
  The provider needs the SEC `contact` for it.
- Once a portfolio's composition is in gold, the wanted list names its
  funds too. The next download reads their filings with `sec-nport`.
  The look-through then reaches the stocks and bonds they hold.

**Terms.** The report states: "This report and the financial
statements contained herein are submitted for the general information
of the holders of units of the Portfolios." fidelity.com answers the
tool's User-Agent with 403 for its other pages, its terms too. It
serves the report. The SEC's content may be copied and redistributed.
The report is read for the portfolios held, at most once in
max_age_days. Off until `providers` names it.

### ishares

BlackRock's iShares funds, from the holdings each fund page serves.

- **US funds** are found by ISIN, CUSIP or ticker in the US site's
  product list. The holdings file is a CSV dated in its preamble. Equity
  lines carry tickers, bond lines CUSIPs and ISINs. The site serves the
  current holdings only.
- **UCITS and Swiss funds** are found by ISIN in the Swiss site's
  product list. The holdings carry ISINs. The fund page names the
  columns and the days the site serves: the latest, the last month end
  and the last year end. `--backfill` fetches those.

A local ticker of a non-US listing goes to the payload, not the ticker
column: it would read as a different US symbol.

**Terms.** BlackRock's terms forbid using "any robot, spider,
intelligent agent, other automatic device, or manual process to search,
monitor or copy this Website ... without BlackRock's permission", and
allow downloading "material displayed on this Website for your personal
use". Off until `providers` names it.

### xtrackers

DWS's Xtrackers ETFs, from the constituent export each fund page links.

- A fund is claimed by its ISIN when its name names the issuer, as
  brokers write it.
- The workbook lists every constituent with its ISIN, country,
  currency, exchange, type and weight. Its "Industry Classification" is
  the sector. Weights are fractions and read as percent.
- The workbook states no day; the day of the fetch dates it. A
  swap-replicated fund exports its substitute basket, not the index:
  name a proxy with `always` for such a fund.

**Terms.** DWS's robots.txt disallows `/etfdata/*` and `/export/*`, the
export's path. DWS's legal notice states no rule on automated access.
The export's first row reads: "Although information in this document
has been obtained from sources believed to be reliable, DWS does not
guarantee its accuracy, completeness or fairness". Off until
`providers` names it.

### invesco

Invesco's ETFs, from the data API its fund pages read.

- A fund is claimed when its name names the issuer. A UCITS fund is
  asked for by ISIN, a US fund by CUSIP.
- UCITS: the fund's holdings, its index's constituents and its swap
  counterparties. A fund with swap counterparties holds a substitute
  basket: its holdings count as `collateral`, the look-through reads the
  index lines, and each counterparty is a row of role `swap`.
- US: the holdings with ticker, CUSIP, units and security type.
- The API serves the current holdings only.

**Terms.** Invesco's terms grant "a limited, non-exclusive and
revocable license to access and make personal use of the Website", and
forbid "devices (including software) that are designed to provide
repeated automated access to the Website" and "any data mining, robots,
or similar data gathering and extraction tools". Off until `providers`
names it.

### amundi

Amundi's ETFs, from the product API behind the fund pages of Amundi's
Swiss site.

- A fund is claimed when its name names Amundi or Lyxor and an ETF, as
  brokers write it.
- The API answers a read-only POST, since it has no GET form, for
  private investors in Switzerland. It serves the current holdings,
  each with its ISIN, weight, sector and country of risk.
- A fund that replicates its index through a swap holds a substitute
  basket. Its holdings count as `collateral`. The API lists only an
  index's top ten, so no index lines are read: name a proxy for such a
  fund in `exposure.proxies`.

**Terms.** Amundi's legal notice for its Swiss site lets a visitor
download pages or parts of the site "ausschließlich für Ihren
persönlichen Gebrauch" (for personal use only). It forbids reproducing
the site or using it for public or commercial purposes without
Amundi's written consent. Off until `providers` names it.

### sec-edgar

What the SEC knows of a US-listed company, found by its ticker in the
SEC's company ticker file:

- its SIC code, which gold reads as a coarse sector;
- its state or country of incorporation and of its business address,
  as EDGAR codes, which gold reads through EDGAR's own code list;
- its latest reported shares outstanding.

A ticker abroad may be another company's in New York. So a security
quoted abroad is asked only where its name and the SEC's agree on the
first word. Quoted abroad means an ISIN from outside the US, or no ISIN
and a price in another currency than US dollars. A depositary receipt's registrant
(SIC 8880, or a name ending in `/ADR`) files from its depositary's
address, so its business address states nothing.

**Terms.** As `sec-nport`.

### gleif

Who issued a security, from the Global LEI Foundation's API, by the
security's ISIN: the issuer's LEI, legal name, legal country and
headquarters country. A US share ISIN that GLEIF maps to a company
abroad is a depositary receipt's. For it, the issuer's other ISINs come
too, so gold can find the share the receipt stands for. A security held
through funds is asked for when it lacks a country, or a sector under a
US ISIN, which may be a receipt's.

**Terms.** "The data available through the Access Service are provided
under the CC0 licence" (GLEIF's terms of use for LEI data, §II.3).

### openfigi

A security's FIGI, name, ticker, exchange and type, from OpenFIGI's
mapping API, by ISIN, else CUSIP, else US ticker. It ties identifiers
together. Five securities go into one request. A ticker is kept from a
US listing only. A bond's type names its market, and gold reads the
bond's sector from it.

**Terms.** Bloomberg "hereby dedicates FIGI Identifiers to the public
domain", including "redistribution of the FIGI Identifiers to your
customers for their use" (OpenFIGI's terms of service, §1). The API
takes 25 requests a minute without a key.

## Pace

Every request goes through one small client:

- https only;
- one request a second per host; under a source's stated limit, with a
  margin: eight a second for the SEC (limit ten), 24 a minute for
  OpenFIGI (25), 50 a minute for GLEIF (60);
- a User-Agent that names the tool, with the `contact` for the SEC's
  hosts only;
- a redirect counts as a request to the host it leads to, with that
  host's pace and User-Agent;
- GET; a read-only POST for OpenFIGI's mapping and Amundi's product
  API, which have no GET form; and the POST that states swissfunddata's
  investor category;
- no cookies, except swissfunddata's session;
- a source that refuses a request for its rate is not asked again in
  the run.

`download --debug` writes `screenshots/http-trace.jsonl` into the run
dir: one line per request, the plan's lookups included, with its URL,
status, timing and size, and no body. `prune` reclaims it.

## Where the data goes

Each `download` writes one UTC-timestamped run dir under
`$XDG_DATA_HOME/wealthdb/refdata/`:

```
<run>/run.json                                  the manifest: every request and its outcome
<run>/funds/<provider>/<fund>/<YYYY-MM-DD | current>/   the provider's raw files
<run>/securities/<provider>/<key>/
```

A folder is named after the fund's reference or the security's key,
`isin_XX…` as in the inbox. One failed request does not fail the run.
The manifest follows the
[shared status lifecycle](../README.md#debug-artefacts-run-status-and-pruning-bronze),
so `load` never reads a crashed run. An N-PORT filing is stored
zstd-compressed.

The silver DB is `refdata.db`, beside the run dirs. It keeps what the
providers state, labels in their own words: each fund's compositions
and their lines, its attributes and counterparties, and each security's
identifiers and facts. A fund and day served again replaces what silver
held for them. [adapters/refdata.md](../../wealthdb/docs/adapters/refdata.md)
describes the tables and how gold reads them.

Gold reads the silver as a source of kind `refdata` in `wealthdb.cfg`:

```json
{ "id": "refdata", "kind": "refdata", "path": "$XDG_DATA_HOME/wealthdb/refdata/refdata.db" }
```

## Reclaiming disk

```sh
./refdata prune --dry-run   # print the plan, delete nothing
./refdata prune             # delete it
```

`prune` removes run dirs that are not complete and the `--debug`
traces. A complete run's raw files are `load` inputs and stay, as do
the inbox and the silver DB.
