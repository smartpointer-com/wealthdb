# refdata

Part of the **wealthdb** suite. See [the architecture overview](../../DESIGN.md)
for the bronze → silver → gold model and [collectors/README.md](../README.md)
for the shared collector conventions.

A collector for **reference data**: what each fund holds, what each
fund states about itself, and what each security is. The engine's
exposure report ([EXPOSURE.md](../../wealthdb/docs/EXPOSURE.md)) reads
it to look through every fund to the securities underneath. It also
keeps a bond fund's duration, yields and rate sensitivities, a
money-market fund's yields, and each company's earnings, dividends and
other reported figures by period.

`refdata` reads only what gold asks for. The engine prints a **wanted
list**: the funds held, the funds the config names (proxies, reference
and size funds), the funds a held fund holds, and the securities held
directly or through funds, each with the facts gold still lacks for it.
`refdata` fetches what is missing or old from public sources and loads
it into its silver. `wealthdb load` brings it into gold.

There is no login anywhere. Every provider reads public pages. A
request names the fund or security it asks about by its ISIN, CUSIP or
ticker. Some sources take their own page or number for it instead. So
each source sees which funds and securities are asked about. No amount,
account or bank is sent.

## Use

```sh
./refdata download --dry-run       # plan a run and print what it would fetch
./refdata download                 # fetch what the wanted list lacks
./refdata download --backfill      # also fetch past dates while each fund was held
./refdata load                     # parse bronze into silver
./refdata load --force             # rebuild silver from every kept run, as after a parser fix
wealthdb load refdata              # bring it into gold (the source's id in wealthdb.cfg)
wealthdb exposure coverage         # see what is described, and what is not
```

`download` asks the engine for the wanted list (`wealthdb -r
refdata-wanted`). `--wanted FILE` reads a saved list instead. A dry run
still makes the lookups that find each fund, such as a product list. A
run can be narrowed:

- `--provider NAME` asks only that provider (repeatable). A name
  `refdata.cfg` does not enable stops the run.
- `--ref REF` fetches only that fund, security or bulk request
  (repeatable). A bulk request is named by its own ref, such as a
  frame's. A run that names only funds or securities makes no bulk
  request.
- `--limit N` makes at most N fetch attempts. A bulk request counts as
  one. The lookups that find a fund are not counted. A request left
  without an attempt is recorded as deferred.
- `--no-securities` skips the security lookups and the bulk requests.

`login` is a no-op, since there is no session. `login --check` asks each
enabled provider for its cheapest answer (a product list, one lookup)
and reports what it answered. A provider with nothing cheap to ask (the
inbox, xtrackers, invesco) says so.

A fund counts as current while its newest composition is younger than
`max_age_days`. Past that age, a provider that can name its newest date
is asked first, and a composition already had is not fetched again. A
composition fetched by an earlier run counts as had before it reaches
gold. A security looked up within `max_age_days` is not looked up
again. A stock that lacks a company's figures is asked for them unless
a run within `max_age_days` asked for them. So a second `download` on
the same day fetches nothing.

A held fund also wants facts beyond its composition: its risk metrics,
its characteristics, its money-market figures and the durations of its
lines ([adapters/refdata.md §5](../../wealthdb/docs/adapters/refdata.md#5-the-wanted-list)).
The providers that answer a fact are offered it in `providers` order,
even where another provider serves the composition. The first that
knows the fund is asked; the next ones stand by. A provider asked for
facts alone keeps only those facts in silver, never the composition its
document also states. A fact counts as current while gold's newest day
for it is younger than `max_age_days`. A fact an earlier run fetched
counts as current for `max_age_days` after that run. `--backfill` also
asks for each past date while the fund was held, and the last one
before the holding began, from a provider that states the fact for past
days, such as a filing. A date that gold holds the fact for, or that an
earlier run fetched, is skipped.

An issuer's fund page answers a fund's characteristics. A page that does
not come leaves them due for the next run, and a warning names it. So
does a page that comes in no layout its provider knows. A fund page that
states no figure, such as an accumulating equity fund's, still answers
them for `max_age_days`. iShares checks no layout: its page answers them
only where it states a figure. Each provider section says what its fund
page is.

The durations of a fund's lines come only with a composition that
states them, such as an iShares bond fund's holdings file. No request
asks for them alone. A run asks for them when it asks such a provider
for the fund's composition. A composition an earlier run fetched from
it counts for `max_age_days` after that run.

A bulk provider answers many securities with one request, such as a
table of every company's figure for one quarter. It plans its requests
after the security lookups. It decides itself which requests of earlier
runs to make again.

A dry run prints a `facts` section after its requests. For each fact
wanted it prints one line of counts. Then it prints one line per fund
or security that stays without the fact:

```
facts
characteristics	3 wanted, 1 planned, 1 had
characteristics	symbol:EXMB	no provider claims it
earnings	2 wanted, 1 planned, 0 had
earnings	isin:XX0000000002	no provider claims it
```

`planned` counts those that a request of this run asks for. `had`
counts those that gold or an earlier run holds within `max_age_days`.
For a company's figures, an earlier run counts only where it asked for
them, or where a bulk answer stated one.
The reason for one left is one of these:

- `no enabled provider answers it`: no provider of the collector
  answers the fact.
- `comes only with a composition from: NAMES`: only a composition from
  these providers states the fact, and the run asks none of them for
  the fund's composition.
- `provider off: NAMES`: only providers that the run does not ask
  answer it.
- `no provider claims it`: the providers that answer it do not know the
  fund or security.
- `deferred by --limit`: its request waits for the next run.

A download logs the same counts, one line per fact.

## Settings

The settings live in `$XDG_CONFIG_HOME/refdata.cfg`, by default
`~/.config/refdata.cfg`. The file is JSON:

```json
{
  "providers": ["inbox", "ishares", "invesco", "spdr", "sec-nport", "sec-nmfp", "sec-edgar", "gleif", "openfigi"],
  "contact": "name@example.org",
  "max_age_days": 30,
  "engine": "/path/to/wealthdb",
  "constituents": true,
  "constituent_floor_pct": 0.0001,
  "fidelity_529_plans": {"XX": "https://www.example.com/xx-annual-report.pdf"}
}
```

- `providers` are asked in this order. For a fund:
  - A fund is offered for its composition only while it wants one, and
    for a fact only to the providers that answer it.
  - The first provider that claims it serves it. The inbox serves
    beside it.
  - The next provider claims it only when the fetch of the one before
    fails.
  - A provider standing by also reads the identifiers the first one
    found. So N-PORT finds a fund named by its ISIN by the ticker
    iShares lists for it.

  For a security:
  - Every security provider is asked for every security held directly
    that it can claim.
  - A security held through funds is asked only of a provider that can
    fill what it lacks.
- `max_age_days` (1 to 3650, default 30) is how old a composition, a
  fund's fact or a lookup may be before it is fetched again.
- `constituents` lets the security providers look up the securities a
  held fund's lines name that no source describes yet (an N-PORT filing
  states no sector), the biggest share of net worth first. Off by
  default: a broad fund names thousands, and OpenFIGI takes 25 requests
  a minute. Securities held directly are always looked up. It also
  lets `sec-edgar` read the figures of the companies a held fund holds,
  by frame.
- `constituent_floor_pct` skips a security held through funds whose
  share of net worth, in percent, is below it. The default, 0.0001, is
  one millionth of net worth.
- `contact` goes into the User-Agent of requests to the SEC, and to no
  other host. The SEC requires it. The providers that ask the SEC
  (`sec-nport`, `sec-nmfp`, `sec-edgar` and `fidelity-529`) do not run
  without it.
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
`sec-nmfp`, `sec-edgar`, `gleif` and `openfigi`. The three SEC providers
wait for a `contact`. An issuer's provider (`ishares`, `xtrackers`, `invesco`,
`amundi`, `spdr`, `sprott`) and a report provider (`swisscanto`,
`swissfunddata`, `fidelity-529`) run only once `providers` names them.
Enabling one accepts its source's terms, quoted below. An issuer's
daily file is fresher than a report or a quarterly N-PORT filing, so
list the issuers first, then the report providers, then `sec-nport`
and `sec-nmfp`.

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
  the header and notes below the table are skipped. A row with a name
  alone ends the table, so a file with holdings after such a row is
  refused;
- or an N-PORT `primary_doc.xml`.

A download copies each folder into bronze once, beside whatever
provider serves the fund. The inbox itself is never changed.

A structured product's basket comes only this way. No provider reads
one. Exchange reference data lists no constituents, and issuers'
product pages do not allow automated download.

A factsheet that states a split rather than a roster (40% Europe, 35%
North America, 25% the rest) has no lines to look through. Neither has
a private fund's offering document. Such a split goes into the engine's
declarations ledger
([EXPOSURE.md, The declarations ledger](../../wealthdb/docs/EXPOSURE.md#the-declarations-ledger)).
No provider reads factsheets.

### sec-nport

US registered funds and ETFs, from their N-PORT filings on SEC EDGAR.

- The fund is found by its ticker in the SEC's ticker file.
- A fund named by a US ISIN alone is found by the ticker of its US
  listing. OpenFIGI names that ticker, once a run for each ISIN. This
  lookup runs only in a run that asks `openfigi`. So `providers` must
  enable it, and `--provider`, where given, must name it.
- A fund with an ISIN of another country files no N-PORT. It is not
  claimed.
- Each filing lists every holding: ISIN, CUSIP, ticker, value, weight,
  currency, asset category and the issuer's country. A currency forward
  states both legs; a derivative names its counterparty.
- `--backfill` reads every quarterly filing inside the days the fund
  was held, and the last one before them: the composition in force when
  the holding began.

A debt holding also states its coupon kind, and whether it is in
default, its interest payments in arrears or its coupon paid in kind. A
holding in another currency states the exchange rate it was valued at,
in units of its currency per US dollar.
A fund with a quarter or more of its net assets in debt states its rate
sensitivities: what its value moves by for a move in rates of one and
of one hundred basis points, per currency and tenor, and for a one
basis point move of investment-grade and of non-investment-grade credit
spreads. `sec-nport` is asked for these figures for a fund whose
wanted-list entry wants `risk_metrics`
([adapters/refdata.md §5](../../wealthdb/docs/adapters/refdata.md#5-the-wanted-list)),
even where another provider serves its composition.

N-PORT carries no sector. Public filings are quarterly and two to five
months old, so a composition is often flagged `aged` in the report.

**Terms.** "Information presented on sec.gov is considered public
information and may be copied or further distributed by users of the web
site without the SEC's permission." The SEC's fair-access rules allow at
most ten requests a second and require a User-Agent that names the tool
and a contact address. The collector sends
`wealthdb-refdata/1 <contact>` and keeps to the limit.

### sec-nmfp

US money-market funds, from their N-MFP filings on SEC EDGAR.

- A fund is asked for only when its wanted-list entry wants
  `money_market`.
- The fund is found as for `sec-nport`, by its ticker or its US ISIN.
  A series that files no N-MFP is not claimed.
- Each monthly filing states the series' weighted average maturity and
  weighted average life, in days, and its 7-day gross yield. It states
  each share class's 7-day net yield; the held class's is read. A class
  the filing does not list has no net yield. The portfolio's securities
  are not read.
- An N-MFP3 filing states the yields for each business day of the
  month. An N-MFP2 filing, the form before it, states them once, for the
  report date. Each figure is dated the day it describes.
- A filing states a yield as a fraction (0.0425). It is kept in percent
  (4.25).
- `--backfill` reads every monthly filing inside the days the fund was
  held.

A fund files within five business days of the month's end, so its
figures are at most about five weeks old.

**Terms.** As `sec-nport`.

### spdr

State Street's SPDR ETFs, US and UCITS, from the daily holdings file
each fund page links and the figures the page states.

- The fund is found by its ISIN or CUSIP in State Street's fund finder,
  or by its ticker among the US funds.
- The file lists every holding with its ISIN (UCITS) or CUSIP (US),
  weight, shares, currency and sector; a UCITS file adds the trading
  country and the industry. A cash line states no weight and counts in
  the fund's remainder.
- The file is today's holdings; there is no history.

The fund page states figures for the fund as a whole:

- a bond fund's duration, its yields to maturity and to worst, its
  average coupon, its average maturity in years, its option-adjusted
  spread and its convexity;
- the 30-day SEC yield (US funds) and the distribution yield;
- a bond fund's credit-quality breakdown and maturity ladder.

An equity fund's page states its P/E only as a forecast for the next
fiscal year, so the P/E is not read. The figures of the fund's index are
not read either. A UCITS page writes decimal commas. The page names its
own number format, and its figures are read through it.

For a fund that wants characteristics, a fetch makes one more request,
for this page. Each figure carries the day its table states, else the
day of the holdings file. A page without a table of the fund's figures
is no fund page.

**Terms.** State Street's site terms allow "producing print-outs for
your own personal use", and forbid publishing or redistributing the
content "for other than use for or on behalf of yourself". Its
robots.txt leaves the fund finder, the fund pages and the holdings
files open. Each file's footer adds: "The whole or any part of this
work may not be reproduced, copied or transmitted or any of its
contents disclosed to third parties without State Street Investment
Management's express written consent." Off until `providers` names it.

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

BlackRock's iShares funds, from the holdings each fund page serves and
the figures the page states.

- **US funds** are found by ISIN, CUSIP or ticker in the US site's
  product list. The holdings file is a CSV dated in its preamble. Equity
  lines carry tickers, bond lines CUSIPs and ISINs. The site serves the
  current holdings only.
- **UCITS and Swiss funds** are found by ISIN in the Swiss site's
  product list. The holdings carry ISINs. The fund page names the
  columns and the days the site serves: the latest, the last month end
  and the last year end. `--backfill` fetches those.

A local ticker of a non-US listing goes to the payload, not the ticker
column: it would read as a different US symbol. Bond lines add their
duration, yields, exchange rate and accrual date. So a composition
brings the durations of a bond fund's lines.

The fund page states figures for the fund as a whole:

- its effective duration;
- its yields: average yield to maturity, yield to worst, distribution
  yield, 30-day SEC yield and 12-month trailing yield;
- its weighted average maturity and coupon, option-adjusted spread and
  convexity;
- an equity fund's P/E ratio;
- on a US page, the credit-quality and maturity breakdowns.

The weighted average maturity is kept in years. A page that states it
in days is converted at 365.25 days a year.

For a fund that wants characteristics, a US fetch makes one more
request, for this page. It keeps the fund's entry in the product list,
which fills a figure the page lacks. A Swiss fetch keeps the page it
already reads. Each figure carries the day the page or the entry states
for it, else the day of the site's newest holdings.

**Terms.** BlackRock's terms forbid using "any robot, spider,
intelligent agent, other automatic device, or manual process to search,
monitor or copy this Website ... without BlackRock's permission", and
allow downloading "material displayed on this Website for your personal
use". Off until `providers` names it.

### xtrackers

DWS's Xtrackers ETFs, from the constituent export each fund page links
and the figures the page states.

- A fund is claimed by its ISIN when its name names the issuer, as
  brokers write it.
- The workbook lists every constituent with its ISIN, country,
  currency, exchange, type and weight. Its "Industry Classification" is
  the sector. Weights are fractions and read as percent.
- The workbook states no day; the day of the fetch dates it. A
  swap-replicated fund exports its substitute basket, not the index:
  name a proxy with `always` for such a fund.

The fund page states a bond fund's effective duration, yield to worst
and average coupon. It states no figure of an equity fund's own: the
P/E and the dividend yield it shows are its index's. It shows no
credit-quality or maturity breakdown.

The page is a script app. For a fund that wants characteristics, a
fetch makes one more request, for the data the page reads. Each figure
carries the day the page states, else the day of the fetch. Data
without the fund's key facts is no fund page's.

**Terms.** DWS's robots.txt disallows `/etfdata/*` and `/export/*`, the
export's path. It leaves the page data (`/api/`) open. DWS's legal
notice states no rule on automated access. The export's first row
reads: "Although information in this document has been obtained from
sources believed to be reliable, DWS does not guarantee its accuracy,
completeness or fairness". Off until `providers` names it.

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

A fund page reads its figures from the same API:

- a bond fund's effective duration, yield to maturity, average coupon
  and maturity in years; a US bond fund's yield to worst too;
- a US equity fund's P/E;
- the distribution yield; a US fund's 30-day SEC yield and 12-month
  distribution rate, read as its trailing yield;
- a bond fund's credit-quality and maturity breakdowns.

The forward P/E is a forecast, so it is not read. A UCITS fund's
answers name a breakdown's parts by a key, such as `aA` or
`5to10Years`. They are kept as named.

For a fund that wants characteristics, a fetch makes two more requests:
the fund's characteristics and its yields. For a bond fund it makes two
more, for the breakdowns. A view of the fund page makes more requests
than these. Each figure carries the day its answer states, else the day
of the holdings. Answers of which none is an object are no fund page's.
The answers name no asset class: a fund whose characteristics state a
bond figure reads no P/E.

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
- its latest reported shares outstanding;
- its reported figures, each for the period it covers: diluted earnings
  and dividends per share, net income, revenue, operating income,
  interest expense, long-term debt, cash, operating cash flow and
  capital expenditure.

The figures come from the SEC's XBRL API in two ways:

- By concept, for a security held directly that lacks a family of
  figures. One request per concept and company returns every period the
  company reported. Where several filings state a period, the last one
  filed counts. Some figures have a second concept, such as a newer one;
  the first concept that states a period counts. A company that
  reports none of the US GAAP concepts is asked for their IFRS twins,
  as a 20-F filer reports. A figure keeps the unit its filing states,
  so an IFRS filer's may be in its own currency. A concept a company
  does not report leaves that figure unknown.
- By frame, for the securities held through funds. One request per
  concept and calendar period answers for every company at once. A run
  asks for each quarter from the one the least current of these
  securities last reported, at most eight back, up to the last quarter
  that ended 45 days ago or earlier. For a figure summed over a period,
  it also asks for the calendar years of those quarters. A balance, such
  as debt or cash, is asked for at each quarter's end. A frame fetched
  before is not asked for again, unless it was fetched within 120 days
  of its period's end and is older than `max_age_days`, since late
  filers arrive. A frame holds US GAAP figures in US dollars only. It is
  stored zstd-compressed.

A figure by concept counts only from 10-K, 10-Q, 20-F, 40-F and 6-K
filings and their amendments. A frame states no form.

A ticker abroad may be another company's in New York. So a security
quoted abroad is asked for only where its name and the SEC's agree on
the first word. Quoted abroad means an ISIN from outside the US, or no
ISIN and a price in another currency than US dollars. A depositary
receipt's registrant (SIC 8880, or a name ending in `/ADR`) files from
its depositary's address, so its business address states nothing.

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

`sec-nport` and `sec-nmfp` also ask it for the US ticker of a fund
named by a US ISIN alone, one ISIN to a request.

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
<run>/bulk/<provider>/<ref>/                    a bulk request's answer
```

A folder is named after the fund's reference or the security's key,
`isin_XX…` as in the inbox. One failed request does not fail the run.
The manifest follows the
[shared status lifecycle](../README.md#debug-artefacts-run-status-and-pruning-bronze),
so `load` never reads a crashed run. An N-PORT or N-MFP filing and a
frame are stored zstd-compressed.

The silver DB is `refdata.db`, beside the run dirs. It keeps what the
providers state, labels in their own words: each fund's compositions
and their lines, its attributes, counterparties and rate sensitivities,
the figures it states for itself, each security's identifiers and
facts, and each company's figures by period. A fund and day served
again replaces what silver held for them; a provider's figure for a
fund and day replaces that provider's same figure.
[adapters/refdata.md](../../wealthdb/docs/adapters/refdata.md)
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
