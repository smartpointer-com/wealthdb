# Demo household

An invented household that shows every wealthdb report and dashboard.
A generator simulates one family's money day by day, from mid-2023 to
the as-of date, and writes it as silver sources of the `synthetic`
kind. The ordinary `wealthdb init` and `wealthdb load -a` then build
gold from those sources, so every enrichment runs for real: spending
and income verdicts, transfer pairing, the cash-flow boundary, returns.

Nothing in it comes from real data. Every person, institution,
merchant, company, fund and ticker is invented. No name belongs to a
real business, as far as web searches and public registries show. No
ISIN ends in its check digit, so none can be a real security's. The
two coins keep their real tickers, because they name an asset class
rather than a holding.

## Try it

```sh
make demo                 # build silver and gold into the demo root
make demo-web             # dashboards on http://127.0.0.1:3100/
make demo-roll            # later: add the days since the last build, load only those
make demo-web-stop        # stop the demo's dashboards
make demo-mcp             # MCP on http://127.0.0.1:3400/mcp; prints the token
make demo-mcp-stop        # stop the demo's MCP server
```

`make demo` takes `WEALTHDB_DEMO_ROOT=` (default: the first line of the
checkout's git-ignored `.demo-root`, else `~/wealthdb-demo`),
`AS_OF=YYYY-MM-DD` (default today, UTC), `SEED=` and `FINDINGS=1`
(below). `make demo-roll` takes the same root and seed as the build it
extends. The dashboards' admin password is in `web/admin-password.txt`
under the demo root, and the MCP server's token in `mcp/token`. The demo
shares the engine and dashboard images with a real setup. Its make
targets build them from the checkout, as `make all` does. Its
containers take their name from the last part of the demo root:
`wealthdb-metabase-demo` and `wealthdb-mcp-demo` for `~/wealthdb-demo`,
`wealthdb-metabase-other-demo` for `~/wealthdb-other-demo`. Demo roots
whose last parts differ get their own containers. Every demo serves on
the same ports, so one runs at a time.

The engine reads the demo when its data root and config dir both point
at the demo root. The web settings point `demo web …` at the demo's
own container, data dir and secrets file, as the make targets do:

```sh
demo() {
  WEALTHDB_DATA_ROOT=~/wealthdb-demo XDG_CONFIG_HOME=~/wealthdb-demo \
  WEALTHDB_CONFIG=~/wealthdb-demo/wealthdb.cfg \
  WEALTHDB_WEB_CONTAINER=wealthdb-metabase-demo \
  WEALTHDB_WEB_DATA_DIR=~/wealthdb-demo/web WEALTHDB_WEB_ENV_FILE=~/wealthdb-demo/web.env \
  wealthdb "$@"
}

demo -r status
demo -r holdings accounts
demo -r holdings positions -x CHF
demo -r returns sources --method both --period total
demo -r spending categories 2025 --period annual
demo -r income summary 2025 --period annual -C +withheld
demo -r cashflow sankey 2025
demo -r cashflow coverage 2025 --period quarterly
demo -r transactions 2026-05-18 2026-05-18
demo -r gains coverage 2025-07-01 today
demo -r exposure sectors
demo -r exposure funds -d 2024-03-31
demo -r exposure credit
demo -r yield global
demo -r yield cash
demo -r income projection -C +principal
```

## The household

Two adults, two children, a house with a mortgage and solar panels. One
earner is paid twice a month, the other monthly. Family abroad means a
multi-currency account for summer trips and some foreign holdings.
History starts on 2023-07-01 and runs to the as-of date.

| Source | Accounts | What it shows |
| --- | --- | --- |
| `brindlecove` | checking, savings, a term deposit, a rewards card, the home loan | payroll, bills, the card cycle, ATM cash, savings sweeps, a tiered savings rate, a term deposit that rolls every year, the mortgage split into interest and amortisation |
| `quayvane` | joint brokerage, two Roth IRAs, a rollover IRA, a 529 plan | monthly buying, dividends with foreign withholding, a 2-for-1 split, a fund rename, tax-loss selling, bonds held directly, a ladder of discount notes, a money-market fund, retirement and education vehicles |
| `tamberlow` | two 401(k) plans, a health savings account | payroll contributions that never touch household cash, HSA reimbursements |
| `aubervane` | a discretionary mandate in one portfolio | four currencies, currency conversions, coupons, withholding, management fees |
| `emberwright` | a venture fund interest and an SPV, in one account | capital calls, a distribution, NAV marks, an SPV that exits in listed shares |
| `tessarite` | a crypto exchange account | the `crypto_partial` regime, staking income, a deposit from and a withdrawal to a wallet nothing tracks, and no stated cost basis: the lot engine rebuilds it from the trades |
| `driftwren` | a multi-currency account in EUR and GBP | spending abroad, a yearly USD wire paired across currencies, interest on the euro balance |
| `homestead` | the family home | a NAV-only holding valued by yearly appraisals |
| `fx` | none | daily USD rates for EUR, GBP and CHF, and a US dollar yield curve |
| `refdata` | none | what the funds hold, every month-end, what the directly held stocks and bonds are, the bond funds' yields and rate risk, the money-market fund's filings, and the companies' reported figures |

One account is declared rather than collected: `kids-savings`, a
children's savings account at a bank nothing collects. A monthly
transfer to it is an own-account move that stays inside the household.

Every holding counted in units keeps its tax lots: shares, fund
units, bonds, gold and coins. A private fund, the SPV and the home are
valued by marks and have none. A buy adds a lot at its cost, with the trading fee where there is one.
A sale relieves the oldest lots first and writes one realized lot per
piece it relieves. A taxable account states its sales on a Form
1099-B, without the gain. The `aubervane` mandate and the
tax-advantaged accounts state them on a statement, with the gain.

`tessarite` states neither a cost basis nor its lots, as many crypto
exchanges do not. The config's `lots` block turns the lot engine on for
it, so every load rebuilds its basis and its realized sale from the
trades (`wealthdb gains coverage` shows it as `rebuilt/fifo/unknown`).
The coins deposited from the untracked wallet have no known cost:
`wealthdb gains summary --missing-basis zero` counts it as 0.

The in-kind exit writes two rows into the equity-transfer ledger. Two
payments are pinned. Three spending rules and one income rule place
rows the provider does not file. A transfer-override row pairs each
yearly wire across currencies.

The `refdata` source states what each fund holds at every month-end,
the way the refdata collector's sources do. Its funds cover each case
the look-through handles:

- The two target-date funds hold three of the other funds.
- The emerging-markets fund is swap-based: it holds a basket and lists
  the index it tracks.
- The international fund is a hedged class.
- The dividend fund has a six-month gap, and its weights leave a
  remainder.
- The real-estate fund's compositions start a year after it is first
  held.
- One stock carries a sector label outside the vocabulary.
- A structured note has neither a composition nor a declaration, so
  `wealthdb exposure coverage` lists it as `undeclared`.

The bond fund's lines state each bond's coupon, duration and yields,
from the `fx` source's curve. One bond floats, one is inflation-linked
and one is callable. The fund states its duration, yields and credit
quality every month-end, and its rate and credit-spread risk every
quarter-end. Every fund with a payout states its distribution yield.
The directly held US companies state their quarterly and yearly
figures, each about 40 days after its period ends. Not every company
pays a dividend, and one reports a loss.

The household holds what every view of `wealthdb yield` reads:

- The joint brokerage holds three bonds: a callable fixed-coupon bond
  above par, a zero and a floating note. The fx source's curve prices
  them.
- It holds a ladder of discount notes. Each one matures after 26 weeks.
  Its discount is booked as interest, and its face buys the next note.
- Its cash earns a sweep rate, so a maturing note earns that rate once
  it lands in cash.
- Its money-market fund keeps a price of one dollar and pays the
  month's yield. The fund's filings state its seven-day yield and the
  maturity of its holdings.
- The rollover IRA holds a high-yield fund that publishes its page but
  not its holdings. The config's `yield.rating_funds` gives its credit,
  and its page gives its maturity bands. With no holdings to look
  through, `wealthdb exposure coverage` lists it and the money-market
  fund as `unmatched`.
- The savings balance states two tiers of rates, and the term deposit
  its rate and its end. Both use the shape of the UBS export.
- The euro bond prices off the dollar curve, at a spread below zero.
  The demo publishes no euro curve, so its euro holdings have no
  benchmark.
- A stock paid twice a year pays nothing in the first months of
  history, so its dividend there is the one its filings report.

The 529 portfolio has no composition of its own. The config names a
target-date fund as its proxy. The declarations ledger states what no
source knows:

- The home and its loan are in the US, in US dollars.
- The SPV is a US industrial company, in US dollars.
- The private fund declares a split from its offering document: three
  regions, two currencies, and two asset classes, private equity and
  infrastructure. `wealthdb exposure classes` shows the fund in both
  classes. It declares its legal situs too, Luxembourg, which
  `wealthdb exposure situs` shows.
- The rates on the euro balance and on the brokerage's cash, which no
  source states.
- A German company's dividend, as its annual meeting resolves it each
  year.
- The terms and ratings the spec and the catalogue hold, which the
  generator writes as the household takes each holding on: the loan's
  terms, two bonds' ratings, the floating note's coupon kind, the
  callable bond's call date, and each discount note's terms.

## How it is built

`demo/generate.py` reads `demo/household.json` (the household's accounts,
amounts, schedules, dated events), `demo/catalogue/` (merchants,
instruments, payers, the funds' holdings and sizes, and the bonds' and
companies' terms) and the `synthetic` kind's schema file in the engine's
source tree. It reads nothing else: no environment, no real config, no
network. The code is in `demo/demohouse/`:

| Module | Does |
| --- | --- |
| `spec.py` | loads the spec and the catalogue, and hashes them and the generator's code |
| `keyed.py` | randomness keyed by (seed, stream, the draw's own keys), so adding days never changes an earlier day |
| `market.py` | four price factors (stocks, bonds, gold, crypto), instrument prices, exchange rates |
| `rates.py` | the US dollar yield curve, from the calendar and the seed |
| `bonds.py` | a bond's price, yield and duration off the curve, and its coupon dates |
| `book.py` | the ledger: every balance is the running sum of its transactions, and every holding the sum of its lots |
| `household.py` | the day-by-day simulation |
| `reference.py` | the funds' month-end compositions, the bond funds' and the money-market fund's figures and the securities' facts, from `catalogue/funds.json` and the instruments |
| `filings.py` | the companies' reported figures, each written once it is filed |
| `silver.py` | writes a full build, or appends the new days to an earlier one |
| `config.py` | renders `wealthdb.cfg`, the ledger CSVs and the declarations ledger |
| `dates.py`, `money.py` | the calendar and timestamps, and Decimal rounding and formats |

The demo root then holds `silver/<source>.db`, `wealthdb.cfg` (every
path in it relative to the root), `overrides/` (the ledger CSVs and the
declarations ledger), the gold file and a `.wealthdb-demo` marker. The
generator writes only into a root that is new, empty or marked. The web
and MCP targets run only against a marked root.

A full build also removes the root's gold file. Each run's change
number is its as-of date. A rebuild at the same as-of carries the
change number the old gold already read, so that gold would skip it.
A rebuild at an earlier as-of would read as silver going backwards.

**Determinism.** Two builds with the same inputs, seed and as-of are
byte-identical. The default seed is `harlow-19`, whose market looks
ordinary: no crash, no boom, no instrument below half its starting
value.

**Appending.** `--append` replays the whole history in memory and adds
only the rows dated after the as-of the files already reached. A
company's reported figure is added on the day it is filed, not the day
its period ends. Each append records a run whose window is the new
days, and the `synthetic` adapter hands gold only the runs since its
last load. A `refdata` run that adds a filed figure reaches back to the
end of its period, and gold reads that source's rows of those days
again, unchanged. So `make demo-roll` loads the new days and changes
nothing older, and the result matches a fresh build at the same date in
every report. An append refuses an earlier as-of, and it refuses when
the generator, the spec, the catalogue, the seed or the findings switch
differ from the ones that wrote the files. A full build is the way past
that.

## The findings switch

`FINDINGS=1` (or `--with-findings`) plants four imperfections for the
diagnostic surfaces:

- about 2% of card purchases from merchants no provider files, so the
  uncategorised share is non-zero;
- one transfer to a brokerage the config does not declare, so one row
  sits in `Unpaired transfers`;
- one month missing from the multi-currency account's statements, so
  coverage shows a gap;
- the venture account's snapshots stop 45 days before as-of, so Data
  Freshness shows a stale source.

A findings build is a one-off picture: it cannot be appended to.

## Checks

- `make test-demo` runs the generator's tests: determinism, the
  append contract, the ledger and calendar invariants, the bond
  arithmetic, and every value checked against the engine's own
  vocabularies (read from its Go source). It then loads the household's first months through the
  engine image in a scratch root under the cache dir. It also builds
  and loads the whole history to today, because the income projection
  starts from today's holdings. That case takes the longest. What it
  expects follows the catalogue's maturities on the day it runs.
- `demo/check_dashboards.py` runs every card of every dashboard of a
  running demo Metabase through its API. It covers each time window,
  each source, every currency the Currency picker offers, and the
  investing grain, section, start year, as-of, missing cost basis, asset
  class and tax wrapper pickers. It also varies each of the projection's
  pickers on its own; the script's docstring lists them. It uses a
  temporary API key, so no filter values stay behind on the admin
  account. Its defaults are the demo's port and the demo root
  `make demo` uses. Another root needs `--password-file`.
- `demo/disjoint.py --demo <gold> --live <gold>` lists every
  identifying value a demo gold shares with another gold: ids, names,
  symbols, keys, descriptions, the ledger's notes, (date, amount) pairs
  and bonds' (maturity, coupon) pairs. The script's docstring lists
  every field it compares. A table one gold lacks is reported and
  skipped. It prints counts and values only, at most N values per field
  with `--show N`. It needs the `duckdb` command-line tool on PATH.
