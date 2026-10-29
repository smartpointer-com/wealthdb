# web — design notes

The optional Metabase BI server. The decisions worth recording.

## 1. Host-side, not a Go subcommand

The `wealthdb` wrapper forwards subcommands *into* the engine container
(`wealthdb:<tag>`, wealthdb/docs/DESIGN.md §12), which has no Docker
socket — so it can't manage a sibling Metabase container. `web` is
therefore handled **host-side**: the wrapper special-cases the `web`
verb (like `build`) and execs [`web/web`](web), which drives `docker`
on the host. The Go engine stays pure (CLI/query only; no daemon).

The one thing the host script needs from the engine is config it can't
easily parse itself (`wealthdb.cfg` is JSON, validated in Go): it calls
`wealthdb web-config`, a hidden read-only subcommand that prints the
resolved `web.enabled`, `web.port`, `gold_db`, `default_currency` and
`lots.missing_basis` as shell-evalable `KEY=VALUE` lines. One source of truth (the Go parser), no
`jq` dependency, no bash JSON parsing.

## 2. Read-only snapshot, not the live gold file

DuckDB is single-writer across processes: one read-write handle OR
multiple read-only handles, never both. Metabase's JDBC pool holds a
read-only connection open continuously, which would make every
`wealthdb load` (read-write) fail at open for as long as the server is
up — a load does not queue behind the connection.

So Metabase never touches the live file. `web start` copies the gold
DB to `…/web/snapshot/wealthdb.db` and mounts *that* read-only.
`web refresh` copies gold again when it is newer than the snapshot. It
recreates the container when the running one does not serve the
current snapshot. Where the file system can clone (APFS), the copy is
a clone. Either way the copy keeps gold's mtime, so a gold write during
the copy makes the next refresh copy again. Loads never contend with
the server; dashboards show data as of the last refresh (run
`web refresh` after `wealthdb load`). The copy is atomic
(temp + `mv`) and refuses to run if a `.wal` sidecar is present (gold
mid-write / not checkpointed), so a torn copy is never captured.

Start and refresh print each step's wall time on stderr
(`web: timing: <step> <seconds>s`). Provisioning prints its wait for
Metabase too.

## 3. The driver: glibc base, version pin, readable JAR (load-bearing)

Three things must all be right or DuckDB silently won't work:

- **Storage format.** The driver bundles its own DuckDB, which reads
  files up to its own storage format. Pin driver **1.5.3.0** (DuckDB
  1.5.3, targets Metabase 59), sha256-verified. The engine's DuckDB is
  newer (`duckdb-go/v2` in `wealthdb/go.mod`) and would create files in
  a format the driver cannot open. So the engine creates every gold file
  in the 1.5 format (`gold.StorageVersion`). This holds for `init`, a
  full reload and `compact`. A file keeps the format it was created in.
  A file in a newer format stays unreadable to the driver, and
  `wealthdb compact` rewrites it in the pinned one. The snapshot step
  reads the copy's header. It refuses a newer format and names
  `wealthdb compact`. It refuses a file that is no DuckDB file too. In
  both cases the previous snapshot keeps serving. A driver bump moves
  `gold.StorageVersion`, its header number in the Go tests and
  `web/web`'s `SNAPSHOT_MAX_STORAGE` together, and the tests hold them
  to one another.
- **JAR must be readable.** `ADD`-from-URL writes the JAR root-only
  (0600); Metabase then *silently skips* `/plugins` and the driver never
  registers. `ADD --chmod=0644` fixes it.
- **glibc, not musl.** The driver's JDBC native (`libduckdb_java.so`) is
  glibc-built and needs `libstdc++.so.6`. The official Metabase image is
  **Alpine/musl**, where the native *hard-crashes the JVM* (gcompat does
  not save it). So the image builds Metabase from its official `metabase.jar` on
  `eclipse-temurin:21-jre-noble` (Ubuntu/glibc, already ships libstdc++6)
  rather than `FROM metabase/metabase`.

The smoke test (provision + `SELECT count(*) FROM positions` *through the
driver*) is the canary that all three are right.

## 4. Loopback-only, dual-stack publish

Published on `127.0.0.1:PORT` **and** `[::1]:PORT` → container `:3000`
(two `-p` flags). Dual-stack so an `ssh -L PORT:127.0.0.1:PORT` *or* a
`[::1]` tunnel both reach it — avoiding an IPv4/IPv6 loopback
mismatch. If the daemon can't bind IPv6, `web start`
falls back to IPv4-only with a warning (`WEALTHDB_WEB_BIND=v4` to
force). Never bound to a public interface; auth is Metabase's own.

## 5. H2 metadata on a bind-mounted volume

Metabase's own metadata (dashboards, users, the saved DuckDB data
source) lives in embedded **H2** at `…/web/metabase/`, bind-mounted
into the container. One container, no second Postgres — fits "no heavy
tooling on metal." Fine for a single local instance; `web stop`
preserves it so a restart keeps everything.

## 6. Provisioning (skip the setup wizard)

A private loopback service shouldn't require the "tell us about
your company" wizard. Metabase's declarative config file
(`MB_CONFIG_FILE_PATH`) can create users/databases — but that's a
Pro/EE feature, a silent no-op on OSS. So `web start` provisions over the
OSS **setup API** ([`provision.py`](provision.py), stdlib only): create
the admin (`POST /api/setup`, which also finishes the wizard), pre-add
the gold DuckDB database (`POST /api/database`, `read_only`), and create
the pre-defined report models (`POST /api/card`, `type: "model"`) as native-query
shims over the gold **multi-currency** report macros (migration 0024). The
macros build on the same line bases the CLI's single-currency macros use and
emit one value-column set per currency (USD/CHF/EUR/GBP). A model reads a
macro's rows from the serving table `web refresh` fills with them (§8), so
the models track command output by construction and bake in no data. In a
`wealthdb (pre-defined)` collection: the `_latest` snapshot
reports (+ all-time `report_transactions`), the daily `_history` reports
(migration 0022) for time-series charts, the two taxonomy models over the
`web_*` breakdown views (migration 0032), cast-only shims over the materialized
`report_returns` table (§8), the spending models over `web_spending`
(migration 0043), the income model over `web_income` (migration 0072),
the `report_cashflow` model over `web_cashflow` (migration 0084) — which
backs the Cash Flow dashboard's Section picker rather than any card —
and two `_pct` privacy variants, `report_sources_latest_pct` and
`report_spending_pct`. They back the privacy twins' MBQL cards and
their drill-through.

On top of the models, provisioning creates pre-defined questions and
ten dashboards. Their pickers:

- **Wealth Overview**: a required currency, a time range, a source, a
  *Tax wrapper* picker, and an *Asset class* picker. Its own tiles are
  the net worth figures, the net worth chart and the asset classes over
  time below it. They take all five, but the three headline figures
  take no time range: they read the latest snapshot.
- **Allocation**: a required currency, a required as-of day, a source,
  a *Tax wrapper* picker, and an *Asset class* picker. The as-of day
  defaults to today. Each tile reads the last day of the holdings
  history on or before the day picked. The history ends on the day of
  the last refresh, so a snapshot not refreshed today still shows its
  latest holdings. A day before the history reads nothing. The day
  reaches the tiles through `web_history_calendar` (migration 0166). It
  maps every day from the history's first to tomorrow to the last
  history day on or before it. So the default stays the same, and a
  refresh does not resend Allocation or its twin. The Top 100 positions tile
  carries a *Vehicle* picker of its own. Each stock row shows its
  share's market-cap size, as Exposure sizes it. A fund row shows none.
  The Top 100 issuers tile below it takes the currency and the as-of
  day alone: it covers the whole book. It reads the latest of the
  instants its view holds on or before that history day: today and the
  twelve month ends before.
  Its twin drops the value and hides an issuer named by a holding's own
  key (`holding:<source>:<id>`).
- **Returns**: a required currency (returns are stored one row set per
  currency), a required start year (0 is since inception) and a source.
- **Gains**: the time range and source, a required currency, a *Tax
  wrapper* picker, an *Asset class* picker, and a required *Missing
  cost basis* picker (ignore or zero; the default is
  `lots.missing_basis` from wealthdb.cfg, else ignore). Every tile takes
  all six. The asset class is each position's. A position that names no
  class has no value to pick, so it counts only while no class is
  picked.
- **Spending**: the time range and source, a required currency, an
  account, and a category multi-select.
- **Income**: a required currency, the time range, a required
  *Horizon*, the source, an account, a *type* picker and a *Tax
  wrapper* picker, then a *Position* and the projection's other three
  below. The time range sets every figure of what was received, the
  horizon every figure of what is expected. The type and the tax
  wrapper reach every card, but the type does not reach the withholding
  and the income after it.
- **Cash Flow**: the time range and source, a required currency, an
  *Investing* grain picker and a *Section* picker, then the projection's
  four: *Horizon*, *Rate path*, *Maturing* and *Shift*. The required
  *Investing* picker nets the section whole or by asset class. The
  *Section* picker is narrower than the rest: the by-month charts and
  the line list take it, and the headline figures and the Sankey do not:
  a statement narrowed to one section is no longer one. The
  projection's four reach the cash flow projection's cards alone, and
  those take no other picker but the currency. There is no account
  picker, because the household boundary defines the pool.
- **Exposure**: a required currency, a time range, a source, a *Tax
  wrapper* picker, and an *Asset class* picker. A breakdown reads the
  latest day in the range, and a chart over time each month end in it.
  The asset class does not reach the four cards of the debt and the
  cycles, whose class is fixed.
- **Yield**: the five Exposure carries, and the projection's four: a
  *Rate path* (constant or forward), a *Maturing* rule (cash, idle or
  roll), a *Shift* in basis points and a *Horizon* (12, 24 or 36
  months). All four are required. They default to the CLI's built-in
  assumptions: constant rates, cash, no shift and 12 months, whatever
  the config's `horizon_months` says.
- **Data Freshness**: no pickers.

All of them are definitions only (MBQL or SQL), with no data baked in.

A tile that a picker of its dashboard does not reach says so. Its
description ends with one sentence that names those pickers, for
example "On this dashboard the Section picker does not reach it."
Provisioning writes it from the mappings the tile is sent with, so the
text follows the binding. Allocation's *Vehicle* picker sits on its
one tile, not in the filter bar, so no note names it. Where a card
declines a picker by design, its own description says why.

A tile that sums money in the chosen currency is native SQL over a gold
`web_*` serving view. The views carry each reporting currency as a
column (gold migration 0114), and a dashboard picker selects rows but
never a column. So each
such tile reads a required `{{currency}}` variable, which picks the
column with a CASE. The Gains tiles are the exception: `web_gains`
(§8) carries a row set per currency and per reading of a missing cost
basis, so there `{{currency}}` and `{{missing_basis}}` filter rows.
`web_exposure` (§8) carries a row set per currency too, so on the
Exposure tiles `{{currency}}` filters rows as well.
Every Currency picker, and every card opened on its own, defaults to
`default_currency` from wealthdb.cfg when that is a reporting currency,
and to USD otherwise. `web/web` passes it to
`provision.py` from `wealthdb web-config`. The
Wealth Overview's three headline figures read `web_sources_latest`
(migrations 0113 and 0132), each account at its latest active
snapshot, so they print what `wealthdb holdings sources` prints. They
take no time range: a filter on the snapshot would drop a source whose
last snapshot falls before the range. The Stalest source tile beside
them shows how old the oldest snapshot is. A
native tile has no "see these records" drill-through. That is the
price of the picker.
Metabase takes a chart's series in the order their rows first appear.
So two kinds of chart order their rows by the series' size. One is
every stacked area or bar with a band per series. The other is every
line chart of a level with a line per series. A level ranks by its
value on the last day, a flow by its total over the window. A stacked
chart puts its largest band on the axis, a line chart lists its top
line first, and both legends read largest first.
Each dashboard but Gains also gets a **privacy twin** (linked from the
dashboard's top row): same layout and filters, but every card shows
shares (%) instead of money. Gains has none. Its tiles are gains on
named positions. A twin would redact the names and normalise the money,
and what would remain is the percent columns the tables carry already.

The twins' charts are native SQL over the gold `web_*` serving views
(migrations 0032, 0043, 0072 and 0084 — TIMESTAMP-cast reductions of the report macros to
the grain each card reads, some folding cash in as a class of its own;
Metabase syncs views like tables and assigns their columns field ids), with the
dashboard pickers landing on the cards as field filters. Each card computes
its normalization denominator in-query with those same filters applied:
holdings divide by the *selected* sources' total at the selected window's
end (the net-worth envelope ends at 100, and a subset still totals 100),
the flows by their own peak month within the selected window
(the tallest bar always reads 100). The Wealth Overview twin's scalars are
ratios of sums over the selected sources' latest holdings (`web_sources_latest`,
migrations 0113 and 0132) — net worth reads a constant 100, positions + cash split it.
Each figure from another dashboard is that dashboard's privacy figure,
except the holdings' income received and net spend. Each is the total
its own twin divides by, so it reads 100 by construction. The overview's
twin leaves them out. The two gains, whose dashboard has no twin, read as a percent of the
cost basis they are gains on.
The **Spending** twin adds redaction to normalization, the way the Returns
twin redacts money columns: its shares are of the window's own net spend
(the breakdowns, merchant and account lists) or of its biggest month (the
trend and the monthly bands), and no card renders a merchant or account
label — the merchant list ranks unnamed rows, the account breakdown regroups
onto source × account kind, and `report_spending_pct` drops the merchant
column so a scalar's drill-through cannot surface a counterparty either.
The **Income** dashboard's cards of the income lines are Spending read
in the other direction, over the `web_income` serving view (migration
0072): the same pickers and the same native-over-the-view
construction, for the same reason (the view carries a row per
reporting currency, and a required `{{currency}}` variable picks the
column rather than filtering rows).
Two tiles have no counterpart, by design: there is no second ring, the
income taxonomy having one vendored primary and so no subcategory level
worth one, and no balance-history chart, nothing on this side being a
liability. Its *type* picker binds to the DETAILED label rather than the
primary one for the same reason the second ring is absent — a
primary-level dropdown would offer a handful of values and hide every
distinction a reader opens the dashboard for. The **Income** twin
redacts the way the Spending twin does: shares of the window's own net
income or of its biggest month, the payer list ranked unnamed, and no
card projecting a payer or an account in any projection it renders.
The uncategorised share is a proportion of ROWS, so its FIGURE needs
no twin — but it is built twice anyway, once per dashboard. A native
card carries its own field-filter template tags wherever it is opened,
and the base card's include an `account` filter, which Metabase renders
as a dropdown of account labels; the twin's copy is built over
`PRIVACY_INCOME_FILTERS` like every other card there.
Neither declares a currency variable, a share of rows being the same in
every currency.

The **Exposure** dashboard shows what is really owned once every fund
is looked through ([wealthdb/docs/EXPOSURE.md](../wealthdb/docs/EXPOSURE.md)).
It reads in this order:

- net worth by asset class as a ring, beside net worth by sector as
  bars;
- the classes over time;
- a world map of the countries, each coloured by its share;
- the regions as bars, beside a table of every region and the countries
  inside it;
- the regions over time;
- net worth by legal situs as bars: where each holding legally sits,
  each fund whole at its own (wealthdb/docs/EXPOSURE.md §13);
- the currencies as a ring, beside the currencies over time;
- public equity by size (large, mid, small), as shares of public
  equity, beside what is held directly against what is held through
  funds, over time;
- the debt by credit rating beside its maturities, its coupons beside
  the public equity's cycles;
- the hundred securities with the most value, each held directly and
  through every fund added up, with its market cap and size;
- how the figures are classified over time, so a growing unknown
  band shows where the other charts stop covering.

A breakdown reads the latest day in the range. A chart over time shows
each month end's shares, stacked to 100.

Three pickers narrow a selection. The source and the tax wrapper reach
every tile: `report_exposure` holds the look-through per source and
account wrapper (migration 0130). The asset class picker keeps the
leaves of the classes picked, so a reader can leave out real estate and
its mortgages, or read public equity alone. It reaches every tile but
the four debt cards below. Each tile computes its share over the rows
it reads. So every figure is a share of the
selection's net worth, or of the class a chart counts, and of net worth
itself with nothing picked; either way scale-free. So the Exposure twin
needs no normalisation. Each breakdown plots the shares: a bar carries
its percentage, a ring lists them in its legend. The base card shows
the value on hover, and its privacy card drops the value column.
The charts over time show shares alone. They are privacy-exempt, so the
twin shows the same cards. Country, region, situs and currency labels
stay, since none names an account. Its tiles read `web_exposure`
(migration 0127), a view over a materialized table (§8).

The four debt cards read `web_exposure_debt` (migration 0138). It
holds today and the last second of each of the twelve months before,
so a window that ends earlier shows nothing on them. Each card reads
the latest of those instants in the window:

- credit, maturity and coupon show the debt, in the views' own bucket
  order. Credit and maturity count the cash too. The credit bars carry
  the grade (IG from AAA to BBB, HY below). The unrated bar also carries
  the share of it nothing describes, as `wealthdb exposure credit` gives
  it.
- the cycles show the public equity.
- each dimension's buckets add up to net worth, `(none)` holding what
  it does not describe. A card leaves that row out and plots each
  bucket's share of the rest, the CLI's `class_share`. So its bars sum
  to 100. A card's title names the class it counts. The maturity card's
  names the debt alone, but it counts the debt and the cash, each by its
  term. Where the rest sums to zero or less, a loan outweighing it, no
  bucket has a share. The size of the debt is on the asset class ring
  and on Allocation.
- a bucket under 0.01% of its class is left out of the bars, and a
  label shows two decimals. The CLI's table keeps every bucket.
- each card's description says what it counts. Metabase shows it
  behind the card's info icon.

The debt view carries no asset class, so the class picker does not
reach those four. These are compositions as of a day, so no projection
picker reaches them. Their twins drop the value.

The Top securities card reads `web_exposure_securities` (migration
0165), at the debt cards' instants. It holds each security's slice per
source, tax wrapper and asset class, so all three pickers reach it. The
card adds up a security's slices in the selection:

- each label is that of its heaviest slice, as the CLI takes the labels
  of a security's largest leaves;
- a fund that reaches it in several slices counts once;
- its share is of the selection, every security counted.

With nothing picked, its rows are those of `wealthdb exposure
securities`. Its twin drops the three values for their shares. It shows
`(unnamed)` and no name for a key that is, or holds, a holding's own
(`holding:<source>:<id>`).

The **Yield** dashboard shows what the holdings earn and how their
value moves with rates ([wealthdb/docs/YIELD.md](../wealthdb/docs/YIELD.md)).
It reads in this order:

- four headline figures: the income yield, the projected income over
  the horizon, the implied yield and the realized yield;
- the income and implied yields per currency the holdings are in,
  beside what a one-percent rise in rates adds to each currency's
  income over the horizon;
- each cash balance, term deposit and money-market holding against
  its benchmark, the worst paid first;
- each bond and fund's implied yield against its duration, beside the
  DV01 per curve currency and tenor: a group of bars per currency, the
  most rate risk first, a bar per tenor. `wealthdb yield curve` splits
  each by what holds it;
- each stock's dividend yield against its payout, beside the share of
  value each quality tag covers, `thin` among them;
- the yields over time, beside the duration over time. They are two
  charts, a yield and a duration differing in unit.

A figure as of a day reads the latest day in the range from `web_yield`
(migration 0138), a row set per currency and instant. It covers today
and the twelve month ends before, so a window that ends earlier shows
nothing on those cards. A rate weighs the holdings that state it as
`yield.summarize` does: over the assets, or over every holding where a
group holds no asset. So a figure is `wealthdb yield global`'s over
the same selection.

The realized yield pools as the engine pools a row. It is the
holdings' income of the twelve months over the sum of their mean daily
values: `received_12m` over `mean_value`, which the gold macro
`yield_realized_pct` sums. A position's own `realized_pct` is its
security's, pooled across accounts and sources
(`wealthdb/docs/YIELD.md` §4), while its `received_12m` and
`mean_value` are its own. The CLI's thin rule is per entity: it reads a
row's pooled days and leaves a thin row blank. The table holds no days,
so a card cannot apply that rule to a picker's selection. It shows the
pooled figure, blank only where the selection's mean value is zero.
The quality card shows the share tagged `thin`: a thin holding was near
zero on most days of the year. A Go test holds the card's figure to the
CLI's row for each grain and currency, and to the pooled figure where
the CLI's row is thin.

The income yield, the projected income and the
sensitivity read today's projection (`web_yield_parts`) for today's
selected holdings, whatever the range, under the projection's pickers.
Their income is the holdings' alone, as `income_12m` is: rent or a
pension has no holding. The sensitivity is each part's income for
rates one percent higher, less its income under the pickers, as
`income_100bp` reads it. A floating rate floors at zero, so where the
rate path and the shift take a rate below zero they change it, and it
takes all four pickers.

The Yield twin shows shares and rates as they are. DV01 becomes a share
of net worth per basis point: 100 times DV01 over the selection's net
worth that day. Expected income becomes a yield: per year, over the
value it is earned on. The projected income would then repeat the
income yield beside it, so its twin shows the horizon's monthly average
as a share of its biggest month instead. The cash table ranks its rows,
names no account, and gives each its share of the credit balances. The
cards of rates, years and securities alone are exempt: the yields, the
dividend scatter and the two charts over time.

The **Income** dashboard also puts what was received beside what the
holdings, and an account's rent or pension, are expected to pay, as
`wealthdb income`'s forward views do
([wealthdb/docs/INCOME.md](../wealthdb/docs/INCOME.md) §12). Two
populations share it:

- the income lines: every receipt, wages and gifts among them. Net
  income, the trend, the types, the payers and the largest receipts
  read them.
- the holdings' income: the six types the projection counts. They are
  interest, coupons, dividends, distributions, rent and pension. Every
  other card reads it, but the withholding and the income after it.

A card of the holdings' income is named for it ("Holdings' income
received"), or its description names it. A title travels with its card,
to the Wealth Overview and to a card opened alone, where a heading would
not. The chart by month carries a subtitle, a row of text above it: wages
and other receipts are in Net income. The dashboard reads in this order:

- six small figures: the holdings' income received and expected, what
  they received, what they are expected to pay, the income lines' net
  income, the holdings' monthly average and the share of income lines
  nothing placed;
- the income lines' monthly trend, the withholding, and the income
  after it;
- the holdings' income received and expected by month, a band per type
  and status, the expected one in a lighter shade;
- the holdings' interest and dividends received, and those expected;
- the income lines by type, beside the top payers;
- received and expected by account, then by position, grouped by
  vehicle;
- a calendar of the payments and principals expected, a holding's own
  income apart from what its matured principal earns, with the basis
  each rests on;
- the largest receipts.

The Time range sets every figure of what was received, and the Horizon
every figure of what is expected: the rest of the current month and
the horizon's months after it. The holdings' income received comes
from `web_yield_received`, which holds the income lines of the six
types. Expected income comes from `web_yield_parts`. A figure of what
was received depends on no assumption, so it takes no projection picker. On the
base dashboard a figure of what is expected takes no Time range. On
the twin it is a share of both halves, so it takes the Time range too.

A card reads several views, and a picker reaches one template tag of a
card. So the Time range is bound to `web_income_days` (migration 0146),
one row per day to today from the first day income was received or tax
withheld, or from 1 January of last year where that is earlier. A card
keeps the received rows and the withheld tax of the days it selects. It
also counts its months by them: each day counts its share of its month.
The Horizon counts its months and the rest of the current one. The
monthly average divides by both. The Source, Account and Tax wrapper
field filters are bound to the views their dropdowns list. The source
and the tax wrapper bind `web_sources_latest`, which holds every
account's, and the account binds `web_income`. Each view the card reads
keeps the rows whose source, account or tax wrapper those select. The
tax booked carries no tax wrapper of its own, so it keeps its
account's.

The Type picker lists the income lines' labels. A card of the holdings'
income keeps the rows of the types a picked label's detailed type maps
to, the map the engine types a line by: interest earned is interest or
a bond's coupons, a dividend is a dividend or a fund's distribution,
and a distribution, a rent and a pension are their own. A test holds
the map to the engine's. The map is by type, so a label reaches every
row of its types: Dividends also reaches a fund's distributions. A label
of any other type, wages among them, selects nothing there. With a type
picked, the positions table lists the holdings that pay it.

The Withholding tile reads two things:

- over the Time range, the tax booked on the income's accounts
  (`web_income_withheld`, migration 0146): every negative tax row
  there, the rows `wealthdb income summary -C +withheld` counts. A row
  counts whatever income it was withheld from, holdings' or not.
- over the Horizon, what the config's withholding table withholds at
  each holding's situs, by its account's own rate or its wrapper's.
  This is a model.

The tax booked is on any income line, so the income after withholding
is every income line received, Net income's, and the income expected,
less the Withholding tile. With no Time range it counts every line. A
range keeps the lines of the days the received figures cover. So a line
older than the first of those days counts only with no range. A tax row names no income type, so neither
tile takes the Type picker, in either half.

Provisioning probes the snapshot's withholding tables
(`yield_withholding`, `yield_withholding_accounts`). Where they hold no
rate, the two tiles' titles say they read the tax booked alone:
"Received withholding — no withholding table" and "Income after
received withholding". Neither tile reads null: with nothing withheld,
the withholding reads 0.

The calendar lists the position the *Position* picker names, by
symbol, name or position key, and every position while it is empty. A
click on a position's symbol or name in the positions table sets that
picker (Metabase's crossfilter click behaviour) to the value the cell
shows.

The twin shows a figure as a share of the income it covers, or of its
biggest month; what was received is a share of what was received. The
income, and each position's, becomes a yield: per year, over the value
it is earned on. The withholding becomes the share it takes of the
income it is set against, and the income after it the rest. A share
or a yield of nothing reads 0. A position with no name reads
`(unnamed)`, unless its key is a cash balance's or an account income's:
any other key is a holding's own, as on the issuers twin. The twin's
calendar also matches the name the twin shows, so a click on
`(unnamed)` lists every unnamed position's payments. No account label,
amount, quantity or price reaches it.

The **Cash Flow** dashboard has a projection section below the
statement. It shows `wealthdb cashflow projection`
([wealthdb/docs/CASHFLOW.md](../wealthdb/docs/CASHFLOW.md) §12): the
household's expected income against its expected outflows, month by
month over the Horizon. It starts with the month after the current
one. Two cards make it, both full width:

- *Cash flow projection by month*: stacked bars by month. What comes
  in sits above the axis: the investment income, the other income, the
  principal returned and the vehicles where they pay out. What goes out
  sits below it: the operating outflows and the debt service. The net
  is a line over the bars. The vehicles' income is a second line: it
  stays outside the pool.
- *Cash flow projection*: one row per month with every part, the net
  and the quality (`projected`, then `trailing`, `terms` or `thin`
  where they apply).

The investment income is what the Income cards expect on the pool's
accounts, read from `web_yield_parts` under the four projection
pickers. The principal returned comes from `web_yield_events` under the
Maturing rule. It counts towards the net unless the rule is `roll`, as
in the CLI. `web_cashflow_pool` (migration 0149) lists the pool's
accounts. A projected part on any other account is the vehicles'
income, and the net leaves it out. The other parts come from
`web_cashflow_plan` (migrations 0149 and 0169): the look-back's monthly averages,
which a refresh stores per currency. So the projection's cards take the
Currency and the projection's four pickers, and no Time range, Source,
Section or Investing picker. The other cards of
the dashboard take none of the projection's four.

The twin shows each part, and the net, as a share of all the income
the projection expects over the Horizon, on every account. Where the
Horizon expects none, the shares are blank. No amount reaches it.

**Data Freshness** counts a source's age from its last read, not from
its latest snapshot: gold's `source_observed` (migration 0122) says
when a run last read each source, whether or not anything changed
(wealthdb/docs/DESIGN.md §6.4). A portal that marks its holdings
quarterly and was read today is current. The table shows the read,
the days since, the latest mark and the days the source has been
quiet: read with nothing new. The mark is `valued_at` (migration
0123), not the snapshot: a read that found a holding gone dates a
snapshot at the read while the marks keep their dates. Both sources
models carry the read as `observed_at` and the mark as `valued_at`, so
the privacy twin's table reads them too.

The **Wealth Overview** charts net worth over time stacked by source.
Below it, it charts the asset classes over time: each day's classes as
shares of that day's total, stacked to 100. The query computes the
shares, and the chart stacks them as they are. Metabase's normalized
stack would pin the axis to 0..100%. That drops a negative band, such
as a debt above what it is owed on, and clips the bands above it. A
day whose total is not positive shows nothing. The chart reads the
daily holdings history, as the net worth chart does, so a fund is one
class. Exposure's chart of that title looks through the funds at month
ends. The Asset class picker narrows the mix to the chosen classes,
which then sum to 100. The card holds shares only, so the twin shows
it as it is.

Below the charts the overview shows a figure or two from the other
dashboards:

- the Stalest source, beside the net worth figures;
- under the heading "Over the time range": the holdings' income
  received, net spend, the spend trend, net cash flow, the savings rate
  and the realized gain;
- under "At the range's end, today and ahead": the unrealized gain,
  the returns year to date (TWR and MWR), the income yield, the
  holdings' income expected over twelve months and the projected net
  cash flow.

Each borrowed figure is the card its own dashboard shows. Four are
built for the overview alone, from their dashboard's data: the asset
classes over time (Allocation), the two returns year to date (Returns)
and the projected net cash flow (Cash Flow). The overview's pickers land on
a figure by slug: a picker reaches the card where
the card has a template tag of that name, or, for an MBQL card, where
its home dashboard maps a picker of that slug. Any other tag of the
card runs at its default. So the flows take the currency, the time
range and the source, the holdings' income the tax wrapper too, the
gains the tax wrapper and the asset class too, and the returns and the
projection the currency alone. As on every dashboard, a tile's
description ends with the pickers that do not reach it. The returns
read the summary since 1 January of the latest year they cover, as no
stored window follows a time range. That is this year once a refresh
has read it. The projected net is the net of the cash flow projection at its defaults.

A click on one of these figures opens its dashboard, and passes the
overview's pickers that dashboard also carries. A year-to-date return
passes its year to the Returns *Start year*. A click on *Asset class
mix over time* passes the day clicked to Allocation's *As of day*. On
the twin a figure opens its dashboard's twin. Gains has no twin, so its
two figures on the twin open nothing: a click never leads from the
privacy view to money. `demo/check_dashboards.py` fails a click whose
dashboard or picker is not there, or that passes a column its card does
not return. A click whose card has not run yet has no stored columns.
The script counts it as not checked.

On both spending views the merchant list ranks merchants only: a line whose resolved
category is a delta — a gift, a bill on a card not itemised, cash out of an
ATM — is not a merchant transaction, and neither is a line nothing has
resolved, so both are left out of the ranking. The predicate is the delta
categories themselves (a delta is primary-level, so the two category columns
are equal on one, and equal at `(uncategorized)` on a line nothing resolved)
plus a merchant to rank by. The two halves are tested separately because a
blank merchant does not imply a delta: 0048 blanks the column on a delta
line, but 0052 gives a card bill the ISSUER it was paid to — a handle on which
card the money went to, not a shop to rank among.

The merchant column is the store's name where the model tier named one, and
otherwise the line's own merchant signature (0054). It is still NULL where a
line has no signature at all, which is why the predicate tests for one rather
than assuming it. So what may rank is every line whose category is not a
delta, and what does rank is each of those that carries a signature, at
whatever grain the fold gives it — a chain appears once per branch signature.
The gates that keep a bare booking code or the bank's own filing away from the
model fence the merchant store, not this column, so a fold that is only a
bank's tag ranks under that tag.
The transaction lists keep such lines, since a line is a line, and the twin's
shares stay relative to the window's whole net spend.
The Spending twin also carries **no account picker**, where the money
view does: a picker renders as a dropdown of the values its column
takes, and every column that identifies an account is a label (a card's
display name falls back to its masked last four digits), so the filter
is dropped rather than rebound onto a column that would mean something
else.
A stale snapshot is caught before any of this is written. Provisioning
probes the driver for the snapshot's schema version and for the serving
views **by name**. It aborts with a "run `wealthdb web refresh`" message
rather than converging the cards to a degraded shape. The version check
catches a snapshot older than 0169, whose `web_cashflow_plan` lacks
the other income's class and account. One older than
0168 lacks the accounts' withholding rates the withholding probe
counts. One older than 0166
lacks the calendar the Allocation cards read their day through. One
older than 0165 lacks the securities the Exposure dashboard ranks. One
older than 0154 lacks the stock positions' sizes. One older than 0149
lacks the cash flow projection's look-back parts. One older than 0146
lacks the days the Income cards' received figures cover and the tax
withheld on them. One older than 0138 lacks the yield views and the
exposure of the debt too.
The by-name probe finds each of these. An older one also reads wrong in ways no
probe by name sees:

- before 0132, it lacks the serving tables the models read and the tax
  wrapper and asset class columns the tiles filter on;
- before 0133, its gains and exposure views leave a missing tax wrapper
  blank, where the holdings views read `taxable_personal`.

On such a snapshot `provision.py` exits 3, where any other failure exits
1. `web start`, `web restart` and `web refresh` then exit 3 as well.
They name `wealthdb web refresh` instead of the browser wizard and leave
Metabase running with the cards it had. A Metabase listing that fails
aborts the run too. Read as empty, it would create a second collection,
database, card or dashboard of the same name.

Metabase's field sync fingerprints a table in one query. That query
cuts each text column to a prefix with `substring()`. The DuckDB driver
reports a list as text, and DuckDB has no `substring()` for a list. So a
list column would fail its whole table's fingerprint every hour.
Metabase never fingerprints a field marked `sensitive`, so provisioning
marks each list column of the gold `sensitive`. Such a column no longer
shows in Metabase's table browser or query builder. Native SQL still
reads it, and the one card that reads `tags` ("What the yields rest
on") is native SQL.

Provisioning is idempotent. A card or dashboard that matches its
definition is left as it is. One that differs is updated in place.
Retired names are archived. `web/web` takes the admin password
from `WEALTHDB_WEB_ADMIN_PASSWORD` (for example in
`~/.secrets/wealthdb-web.env`), or generates one once and saves it
chmod 600. It hands the password to `provision.py` in that same
environment variable, never on the command line, where `ps` would show
it. `WEALTHDB_WEB_NO_PROVISION=1` opts back into the browser wizard.

The container runs as root (temurin default), so it reads the mode-600
gold snapshot without loosening the snapshot's permissions.

## 7. Memory discipline (DuckDB runs in-process)

The DuckDB engine lives inside the Metabase JVM, so one `java` process
holds the JVM heap *plus* DuckDB's native memory. Unconstrained, DuckDB
assumes 80% of the machine's RAM; a dashboard opening fires all of its
tiles concurrently, and history-heavy queries can grow the process until
the kernel OOM-kills it (taking the whole Docker VM's memory with it).
Four settings work together, each load-bearing:

- **DuckDB `memory_limit` (4GB) + `threads` (8)** — set by `provision.py`
  as connection *details*, which the driver forwards as instance-level
  JDBC config. Instance-level means every tile a dashboard fires at once
  shares one pool, so the cap is sized against the busiest page rather
  than a single query (`ensure_database`'s docstring says why). They
  must NOT move into `init_sql`: that runs
  per pooled connection, and DuckDB refuses to re-`SET` a used
  `temp_directory` — the second connection then poisons every query
  after it.
- **A writable spill mount** — the driver hard-wires DuckDB's
  `temp_directory` to `<database_file>.tmp`, which sits on the read-only
  snapshot mount. `web/web` mounts a host directory at exactly that path
  so memory-capped queries spill to disk instead of failing.
- **Container caps** (`--memory 8g`, `-Xmx2g`, `MALLOC_ARENA_MAX=2`) —
  the backstop: a runaway kills only this container, never the VM; the
  heap can't auto-size against the container cap; glibc doesn't hoard
  arenas under DuckDB's thread pool.
- **`--restart unless-stopped`** — if the backstop ever fires, Metabase
  comes back on its own (metadata is safe on the H2 volume); a plain
  `web stop` still stops it for good.

The settings cap what the queries take. The serving tables (§8) keep
what the queries need small:

- A report macro computes its whole output on every read.
- A history macro computes every day from the first snapshot, whatever
  the tile filters on.
- So every holdings, history and transaction model and view reads a
  table that holds a macro's rows.
- A tile then scans only the rows it keeps. Its memory grows with the
  rows it reads, not with the length of the history.

## 8. Materialized returns, gains, exposure, yield and serving tables

The Returns dashboards can't be views or macros: MWR is XIRR (iterative
root-finding), and the returns engine leans on things that only exist
in Go and config: the per-source `ReturnsPolicy`, `wealthdb.cfg`'s
inception overrides and exclusions, the transfer-netting heuristic,
synthetic onboarding. A SQL re-implementation would be a second returns
engine that drifts. So the existing engine computes and the result is
**materialized** into `report_returns` (migration 0026). It,
`report_gains`, the `report_exposure` tables, the `report_yield`
tables and the serving tables (below) are gold's *derived* tables;
everything else is loader-written source of truth or computed on read.

The contract keeps it honest: each `(grain, granularity, currency)`
partition is the **verbatim output of one `RunReturns` call with the
CLI's default knobs** — `wealthdb returns <grain> --period
<granularity> --method both -x <CCY>` — so dashboard numbers equal CLI
numbers by construction. 4 grains × 4 granularities (monthly,
quarterly, annual, total) × 4 currencies (the reporting set of the
`_multi` macros) = 64 partitions; the whole table is rewritten in one transaction (DELETE +
batched INSERT), so a failed run leaves the previous materialization
intact. Bucket rows carry TWR only and MWR lives on the
since-inception summary rows — that is engine behavior, mirrored, not
smoothed over. Diagnostic knobs (`--netting off`, `--inception strict`)
stay CLI-only.

The 64 partitions are cheap because the loaded data depends only on the
currency, never the grain or period: `RunReturns` is split into a load
step (the DuckDB scans) and a pure in-memory `computeReturns`, so the
materializer loads each currency's dataset **once** and drives all 16
`(grain, period)` computations off it — and loads every currency
in a single pass over the `_multi` report macros rather than one scan
per currency, and writes in batched multi-row `INSERT`s.
Aggregate grains sum constituent account values as floats, so their
cent-and-below digits depend on summation order; `groupAccounts`
sorts each group's members by `(source, account)` so a run is
byte-deterministic (the accounts grain's groups are singletons, so it
is exact either way).

That cheap re-compute also buys the dashboard's **start-year picker**.
The since-inception TWR is frequently `null` — the earliest months are
degenerate (a non-positive opening base), and the noise around
inception swamps the charts. So beyond the base matrix
(`window_from_year = 0`), the materializer writes, for each grain,
currency and year in the data's span, a *since-that-year* total summary
(`window_from_year = Y`, migration 0027) — each just another
`computeReturns` over the already-loaded dataset with `FromEpoch =
Jan 1 Y` (the verbatim equivalent of `wealthdb returns <grain>
<Y>-01-01 - -x <CCY>`). Only the since-X summary depends on the window,
so windowed rows are `is_summary` totals only; the per-period buckets
exist once at `window_from_year = 0`. The picker (`window_from_year` on
the MBQL scalars/table) then rescopes the whole figure exactly, so a
later start makes the null a real number.

The dashboard reads this as: rescopable **scalars** (TWR / MWR /
annualized) and a **by-source table**; a **cumulative return chart**
compounded from the chosen start year, on a linear percent axis; and
**monthly / quarterly / annual** per-period charts. Every chart is split by source with the
global grain unioned in as a toggleable `(all sources)` line — the
pseudo-source that keeps one chart per granularity instead of separate
global and by-source views. These charts are native SQL (window
functions and the union need it) and take the Currency / Start-year /
Source pickers as template variables (Source is a field filter).

The cumulative return is derived from the **windowed** summaries, not
by chaining the per-period buckets. The growth from the start of the
earliest visible year `Y0` to the start of year `Y` is
`(1 + TWR_since_Y0) / (1 + TWR_since_Y)`; the chart plots that minus 1,
so it starts at 0% in `Y0`. Chaining calendar-bucket
Modified-Dietz returns would be wrong here — a flow landing between two
sparse snapshots poisons that bucket (a mid-month deposit with no fresh
snapshot reads as a large loss, then a large gain next period), so a
chained index can diverge from the true TWR by hundreds of points for
sparse-snapshot sources: a source that gained can chain down to a
spurious near-total loss. The windowed TWRs use the
engine's snapshot-aligned chain, so the chart is correct and is
built from the same figures the scalars and table show — at the cost
of annual granularity (a finer curve would need per-month windows).
The last point is the start of the current year, so the stretch from
then to today is not drawn.

**Gains** are materialized too, into `report_gains` (migration 0117),
for a different reason. The gains figures are SQL (`gains_windows`,
migration 0116), so a view would work. But one dashboard open fires two
dozen tiles, and each would compute the whole history again. So
`web-materialize` computes the monthly gains rows of every reporting
currency under both readings of a missing cost basis (`ignore`, `zero`;
docs/GAINS.md §8) in one evaluation (`gains_windows_all`, migration
0119) and stores them, a second or two per refresh. The rows are that
call's output, less the figures in a holding's own currency. So the
rows summed by month equal `wealthdb gains summary --period monthly -x
CCY --missing-basis M`, and a Go test holds that.
The `web_gains` view labels the accounts and joins in their tax
wrapper.

The Gains dashboard reads it month by month. A figure over the window
sums the months in it. A figure at the window's end reads the last
month in the window. The time picker selects whole months: a custom day
range takes the months it touches.

The dashboard splits the gain into the gain on positions sold and the
gain on positions held (`sold_gain`, `held_change`, migration 0120).
Realized gain and unrealized change add up to the same total. But a
sale realizes its whole gain since purchase, and the same amount leaves
the unrealized change, so each sale would show as two opposite bars. The
sold part counts a sale only from its month's start
(wealthdb/docs/GAINS.md §2). The realized gain keeps its own tile: it
is the figure a tax return reads.

**Exposure** is materialized into `report_exposure` (migration 0127)
for the reason gains are. Each instant's look-through walks every fund
the holdings reach, and the dashboard charts every month end. So
`web-materialize` computes the look-through at these instants:

- the last second of every month from the first holding;
- the end of today.

wealthdb/docs/DESIGN.md §10.14 says how it stores them. One instant's
rows add up to `wealthdb exposure classes -d <day> -x CCY`, class by
class, and to net worth. A Go test compares each instant with
`holdings global`, with every row of `wealthdb exposure sizes`, which
splits each class, and with every row of `wealthdb exposure situs`.
Countries are by country of risk; each row also carries its holdings'
legal situs. A stock's size is today's at every month end: gold keeps
one size per security.

The `web_exposure` view does three things:

- it names a missing dimension `unknown`, a missing situs too, a
  metal's or a coin's country `(none)`, and the size of anything but
  public equity `(none)`;
- it labels each row direct or via funds;
- it adds each row's share of that instant's net worth.

The debt and equity buckets of the credit, maturity, coupon and cycle
views and the largest issuers are materialized beside it, into
`report_exposure_debt` and `report_exposure_issuer_ranks` (migration
0138). Their look-through costs several times the plain one, so they
hold today and the last second of each of the twelve months before
only. `web_exposure_debt` keeps the source and the tax wrapper the
pickers narrow; `web_exposure_issuers` holds the whole book. The
securities are many rows an instant, so `report_exposure_security_slices`
(migration 0165) holds the same instants. It cuts the rows of `wealthdb
exposure securities` by source, tax wrapper and asset class. A Go test
adds up each security's slices as the card does and compares them with
the command's rows, and each selection's total with `report_exposure`.

**Yield** is materialized too. The yield report and the income
projection are computed in Go (`internal/yield`), so `web-materialize`
writes their figures (`gold.MaterializeYield`, migration 0138;
wealthdb/docs/DESIGN.md §10.15 has the engine side):

- every holding's figures and its DV01 per curve currency and tenor,
  in every reporting currency, today and at the last second of each of
  the twelve months before;
- today's projection as its parts, per holding, month and maturing
  rule, in the holding's currency with its rate to each reporting
  currency; each currency's rate per month and path; and the principals
  maturities return;
- every income line received, to today;
- the cash flow projection's look-back parts per currency and month of
  the projection (`report_cashflow_plan`, migration 0149).

Four views sit beside them. `web_income_days` and `web_income_withheld`
(migration 0146) list each day the received figures cover and the tax
booked on the income's accounts. `web_cashflow_plan` (migrations 0149 and 0169)
serves the cash flow projection's look-back parts, which depend on no
picker. `web_cashflow_pool` (migration 0149) lists the cash pool's
accounts, which split the income projection for it.

The pickers multiply: two rate paths, three maturing rules, any shift,
three horizons. So no combination is stored. A card sums each part's
income (`yield.Part.Income`) at query time, which costs a join and a
sum. A Go test holds the sum of the parts under the default
assumptions to the sum of the holdings' `income_12m`. The `web_yield*`
views label the rows and render each instant as a day. The refresh
computes the yield tables for 13 instants in 4 currencies.

The **serving tables** (`serving_*`, migration 0132) hold report macros'
rows verbatim, for the reason §7 gives. They are:

- the daily histories: global, per source, account, portfolio and
  position, and the card balances;
- the latest holdings: global, per source, portfolio, account and
  position;
- every transaction line: all of them, and the spending, income and
  cash flow lines.

Each table is one macro's output. `web-materialize` rewrites them all
in one transaction, so a reader sees all old or all new. A Go test
compares every table with its macro, row by row. Two views build on
them: `holdings_lines_history` and `holdings_lines_latest` hold each
position line and each account's cash. A line carries its
account's tax wrapper and its asset class, cash being the class `cash`.
The holdings `web_*` views group these lines, so every holdings tile
can filter by both. The flow view `web_income` carries the tax wrapper
alone.

The refresh hook: `web refresh` (and `web start`'s initial snapshot)
runs the engine's hidden `web-materialize` subcommand *before*
`_snapshot`, so the serving tables, returns, gains, exposure and yield
are exactly as fresh as the holdings and `wealthdb load && wealthdb web
refresh` remains the whole update flow.
`web-materialize` first compares what it would build from with what it
last built from: the loads in gold, the config and the files it reads,
the engine binary and the UTC day. When all match, it writes nothing
and prints `inputs unchanged`. Gold then keeps its mtime, so refresh
does not copy it again. A run that stops partway leaves no record, so
the next run rebuilds.
It is an engine-owned write to live gold — the same class as `load` and
mutually exclusive with it under the engine's write mutex, an advisory
flock on a `<gold_db>.wealthdb.lock` sidecar taken before gold is
opened at all (wealthdb/docs/DESIGN.md §4.10), with DuckDB's own
single-writer file lock behind it. Either way a concurrent load makes
`web-materialize` fail rather than wait — exit 5, which means retry —
and refresh aborts *before* the snapshot is touched, while the previous
snapshot keeps serving. The sidecar is an expected artefact beside the
gold DB: the kernel drops the lock when the process ends, the empty
file stays, and a copy or a backup may ignore it. The web container
itself still never sees anything but the `:ro` snapshot (AGENTS.md §1).

## Testing

[`test_web.sh`](test_web.sh) (`make test-web`) unit-tests the pure
helpers with no Docker: the dual-stack `-p` flag construction, the
read-only snapshot mount in the `docker run` args, the `.wal` guard,
the storage-format guard, the generated-password complexity and a
failed generation, and — against a stubbed engine — the `_materialize`
invocation, the engine's own error line and status repeated on a
failure, `web_refresh`'s materialize-then-snapshot ordering and the
steps' timing lines. It checks the refresh skips too: an unchanged
gold is not copied, a container that does not serve the
snapshot's inode is restarted, and a failed copy keeps the old
snapshot. Against a stubbed `provision.py`, it checks that a stale
snapshot fails `web start` and `web refresh` without the browser hint.
It then runs [`test_provision.py`](test_provision.py), which asserts
everything `provision.py` builds *before* it talks to the API — the
definitions are pure functions of module constants, so the filter
registry, the model SQL, the card and dashboard defs, the parameter ids
and the picker → template-tag wiring all check statically. Against a
fake API it checks that an unchanged card or dashboard is not sent
again and that a changed one is. Three behaviours are worth naming:

- the privacy twins render no merchant or account label;
- the Yield twin, the Income twin's cards of the holdings' income and
  the Cash Flow twin's projection cards carry no column but a share, a rate,
  a year, a date or a label;
- a snapshot missing a serving view, or older than the cards' schema,
  aborts the run (exit 3 for the older one).

The SQL of the income projection cards, the cash flow projection
cards and the Exposure debt and cycle cards also runs on an invented
book under DuckDB, so the tests need the `duckdb` Python module, and
fail without it. `make
test-web` runs them in a venv of its own, `web/.venv`, that holds it
(`web/requirements-test.txt`). Whether Metabase *accepts* a payload
needs a live instance: `demo/check_dashboards.py` runs every card of a
running demo Metabase through its API (demo/README.md). The Go side
(`web` config block, validation, the `web-config` emitter,
`MaterializeReturns`, `MaterializeGains`, `MaterializeExposure`,
`MaterializeYield`, `MaterializeServingTables` and the
`web-materialize` command) is covered by `go test ./...` (`make
test-wealthdb`). End-to-end (build → start → provision → query through
the driver) is the manual smoke test in §3.
