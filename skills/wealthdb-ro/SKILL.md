---
name: wealthdb-ro
description: Query the user's consolidated cross-institution investment portfolio through the read-only `wealthdb` CLI — holdings, account and portfolio balances, net worth, asset allocation, transaction history, investment returns (TWR and MWR/XIRR), look-through exposure by asset class, sector, size, country and currency, each holding's legal situs, realized and unrealized gains with their cost basis and tax lots, categorised spending, categorised income, and the household cash flow statement with its Sankey. Use whenever a question is about current holdings, what an account or portfolio is worth, allocation, money in or out, what is really owned once every fund is looked through, how something performed over a period, what was gained or lost (realized or unrealized, or a holding's cost basis), what was spent and on what, what was received and from whom, or where the household's cash came from and where it went.
---

# wealthdb — portfolio queries (read-only)

`wealthdb` is a read-only CLI over one database that merges every configured
bank, card, broker, pension and crypto source, plus the property, loans and
private holdings recorded by hand. A house, a mortgage and a venture fund are
accounts like any other. Just run the command: no setup, paths or connection
flags.

## Hard rules

1. **Run only these commands:** `holdings`, `exposure`, `transactions`,
   `returns`, `gains`, `spending`, `income`, `cashflow`, `status`,
   `snapshots`, `help`, `version`. Every other command writes or serves
   other clients and is forbidden. Never write; just query.
2. **One number for a whole window: add `--period total`.** The default is
   one row per month (per quarter for `returns`). Never add rows up yourself.
3. **There are no row-filter flags** (`--source`, `--account`, `--category`,
   `--symbol` do not exist). Run the view, then `| grep -i text` the table
   (one row per line) for a name, wrapper, category or source.
4. **Read tables; parse JSON only to sort.** `-f json | jq`: the output is one
   JSON **array** (start with `.[] |` or `map(...)`). Keys are the table
   headers, currency suffix included (`value_USD`, `net_spend_USD`,
   `twr_pct`). Money is a decimal **string**: `tonumber` before comparing. An
   empty column is **omitted**, so `map(select(.value_USD))` before sorting.
   A return that cannot be computed is `"n/a"`, reason in `quality`.
5. **Signs.** In line views (`transactions`, `spending transactions`,
   `cashflow transactions`) money leaving is negative. In `summary`,
   `categories` and `types`, `spend`, `refunds` and `income` are positive.
6. **`wealthdb help <command>`** prints everything this file leaves out.

## Recipes

Windows: `2025` a year, `2026-03` a month, `2025-01-01 2025-06-30` a range.

- **Net worth or total cash, now / at a date / in CHF** — `wealthdb holdings global`
  (one row: cash, positions, total), then `-d 2025-12-31` or `-x CHF`
- **Each account, with its tax wrapper** — `wealthdb holdings accounts`
- **One institution's total** — `wealthdb holdings sources | grep -i schwab`
- **One wrapper, account, property or loan** —
  `wealthdb holdings accounts | grep -i roth` (or `401`, `mortgage`)
- **Largest holdings** —
  `wealthdb holdings positions -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber) | reverse | .[0:5]'`
  (a house or a loan is a position too)
- **Allocation once funds are looked through** — `wealthdb exposure classes`
- **Sectors, company sizes, countries, currencies** — `wealthdb exposure sectors`,
  `sizes`, `countries`, `currencies` (`-d 2025-12-31` for a past day)
- **One company, directly and through funds** —
  `wealthdb exposure securities | grep -i <company>` (`value` is the total)
- **Exposure over time** — `wealthdb exposure countries --period quarterly 2025`
- **How far to trust exposure** — `wealthdb exposure coverage`; how old each
  fund's data is, and which fund's holdings stand in for it (`proxy`, a key):
  `wealthdb exposure funds`
- **What the household states about a private holding** — a fund:
  `wealthdb exposure funds` (`via` reads `declared`, with the record's
  `declaration_date` and the dimensions it `declared`); a property, a
  loan or an SPV: `wealthdb exposure securities | grep -i <name>` (quality
  `declared`); the private holdings with no record:
  `wealthdb exposure coverage | grep undeclared`
- **Where holdings legally sit** (for an estate question) —
  `wealthdb exposure situs`; one fund's situs:
  `wealthdb exposure funds -C fund,name,domicile,situs,situs_basis`; what
  each situs rests on, and the holdings whose situs is unknown:
  `wealthdb exposure coverage -f csv | grep '^situs,'`
- **Records still to write** — `wealthdb exposure coverage -f ndjson`: a
  skeleton per undeclared holding, and a situs-only one per holding of
  unclassified situs, for the user to fill into the declarations ledger
- **Return of the portfolio / each account** —
  `wealthdb returns global 2025 --period total` / `wealthdb returns accounts 2025 --period total`
  (the account column is `entity`; one account: `| grep -i 'brokerage'`)
- **Best account by return** —
  `wealthdb returns accounts 2025 --period total -f json | jq 'map(select(.twr_pct != "n/a")) | sort_by(.twr_pct|tonumber) | reverse | .[0:3]'`
- **Realized gains of a year** — `wealthdb gains summary 2025 --period total` (`realized`)
- **Total unrealized gain now** — `wealthdb gains summary --period total`
  (`unrealized_end`); never sum positions
- **Lots sold in a year** — `wealthdb gains realized 2025`
- **Holdings that gained most** —
  `wealthdb gains positions 2025 -f json | jq 'map(select(.gain_USD and .symbol)) | sort_by(.gain_USD|tonumber) | reverse | .[0:5]'`
  (one row per account and holding: a symbol in two accounts has two rows)
- **A holding's cost basis and unrealized gain** —
  `wealthdb holdings positions -C +cost_basis,unrealized_gain,basis_stamp | grep -i btc`
  (`basis_stamp`: `stated/…` the institution's, `derived/…` computed from its
  figures, `rebuilt/<method>/…` rebuilt from the trades); lots:
  `wealthdb gains lots | grep -i btc`
- **Where gains are missing or rebuilt** — `wealthdb gains coverage 2025`
  (`no_basis`, `no_realized`: missing, not zero); against the statements:
  `wealthdb gains check 2025`. `--missing-basis zero` on a `gains` view or
  `holdings positions` counts an unknown cost as zero.
- **Spent in a window / by category** — `wealthdb spending summary 2025 --period total` /
  `wealthdb spending categories 2025 --period total` (add `--level detailed`
  for groceries, restaurants, flights …, then `| grep -i grocer`)
- **Biggest purchases in a month** —
  `wealthdb spending transactions 2026-03 -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber) | .[0:5]'`
- **Income / by type** — `wealthdb income summary 2025 --period total` /
  `wealthdb income types 2025 --period total | grep -i dividend`
- **Net cash flow / by class** — `wealthdb cashflow summary 2025 --period total` /
  `wealthdb cashflow flows 2025 --period total --level class`
- **Mortgage, retirement or education payments** —
  `wealthdb cashflow flows 2025 --period total --level class | grep -i -E 'mortgage|retirement|education'`
- **Largest transactions** —
  `wealthdb transactions 2025 -f json | jq 'map(select(.value_USD)) | sort_by(.value_USD|tonumber|fabs) | reverse | .[0:10]'`
- **Is the data current / which dates exist** — `wealthdb status` / `wealthdb snapshots <source>`

## Pick the command

| The question | Command | Views |
|---|---|---|
| Net worth, balances, holdings (as of a day) | `holdings` | `global`, `sources`, `portfolios`, `accounts`, `positions` |
| What is **really owned** once funds are looked through (as of a day); where holdings legally sit | `exposure` | `classes`, `sectors`, `sizes`, `countries`, `currencies`, `situs`, `securities`, `funds`, `coverage` |
| Trades, dividends, fees, cash in and out | `transactions` | — |
| How something **performed** (return %) | `returns` | `global`, `sources`, `portfolios`, `accounts` |
| **Gained or lost**; cost basis; tax lots | `gains` | `summary`, `sources`, `portfolios`, `accounts`, `positions`, `realized`, `lots`, `coverage`, `check` |
| **Spent**, on what | `spending` | `summary`, `categories`, `transactions` |
| **Received**, from whom | `income` | `summary`, `types`, `transactions` |
| Where the household's cash came from and went; mortgage, retirement, education payments | `cashflow` | `summary`, `flows`, `sankey`, `transactions`, `coverage` |

Holdings totals reconcile: `global` ≈ sum of `sources`, `portfolios`,
`accounts`, and `positions --with-cash`.

## Dates

`holdings` and `exposure`: `-d YYYY-MM-DD` is the day (default today); each
source gives its latest snapshot on or before it. `exposure --period
monthly|quarterly|annual` takes a window instead, as below.

Every other command takes the window as positional arguments, never `-d`:
`2026` (a year), `2026-06` (a month), `2026-06-15` (a day),
`2026-01-01 2026-06-30` (a range), `2026-01-01 -` (to today),
`- 2026-06-30` (from the start), `- today` (all time). Defaults:
`transactions` the past 30 days; `returns` since the first snapshot; the
rest the trailing twelve months. Resolve "last year" and "this year" from
today's date and pass them (`2025`, `2026-01-01 today`).

## Flags and columns

`-f table|json|csv` · `-x CCY` output currency · `-C COLS` columns (names,
`all`, or `+add,-drop`) · `-p` redact · `--period` on `returns`,
`spending`, `income`, `cashflow`, `gains`, `exposure` · `--level` on
`spending`, `income`, `cashflow`.

`-C` takes column **names**, not headers: ask for `value`, `share`,
`net_flow`, `twr`; they print as `value_USD`, `share_pct`, `net_flow_CHF`,
`twr_pct`. `-C all` lists them; `wealthdb help <command>` lists each view's
defaults.

`-p` prints amounts as `*****.**` (and drops them from JSON) and narrative
text as `***`; categories, classes and shares stay legible.

Account attributes, for `grep` or `-C`: `silver_source` (the institution:
`schwab`, `ubs`, …), `account_kind`, `tax_wrapper` (`roth_ira`, `529`,
`pillar_3a`, `trust_*`, …), `management_style`.

`category`, `income_type`, `class` and `group` are display labels; the
taxonomy values (`cash_withdrawal`) are in `spend_detailed`,
`income_type_id`, `class_id`, `group_id`. Filter on the value, quote the
label.

`counterparty`, `description`, `merchant_signature` and `payer_signature`
are raw statement text that can name a private person: request them only
when needed, never echo them wholesale.

## Returns

TWR (default) is the strategy's return, cash flow timing removed: "how did
the investments do?". MWR (`--method mwr`, XIRR) is the return on the money
as it went in and out: "what did I make?". `--period` defaults to
quarterly plus a since-inception row. Returns are after fees and taxes
paid.

**Read `quality`:** an `n/a` always has its reason there.
`nonpositive_base` (a loan: no return), `mwr_no_flows` (no flows: no MWR),
`nav_only…` (value-only source: rough), `since_data_inception` (since the
first snapshot, not the account's opening), `stale_snapshot` (old
valuation). Account returns are exact; portfolios, sources and global are
best-effort. Returns never add across views: query the grain you want.

## Exposure

`exposure` looks through every fund to the stocks, bonds and cash it holds
and adds what is held directly. Every view sums to net worth: `share_pct` is
a share of net worth (a loan is negative), `class_share_pct` a share of the
row's asset class.

- **Read `quality` first.** `proxy` (another fund's composition stands in),
  `declared` (the household's own statement: a target split from a fund's
  documents, or a property's country), `aged`/`forward` (a composition
  from another day), `partial`, `unmapped`, `undeclared`, `unmatched`
  (part of the value is not fully classified), `by_name` (a sector found
  by matching a name), `opaque` (a private fund nothing looks through).
- In an aggregate row each flag reads `flag=NN%`: the share of the row's
  gross value that carries the flag (`proxy=12%`, `aged=<1%`).
- A declared split is marginal: a fund declared 40% Europe and 80% private
  equity shows both, but how its regions and classes combine is assumed,
  not known.
- `securities` shows a declared holding as one row, with its largest
  part's class, sector, country and currency. The split itself is in
  `classes`, `sectors`, `countries` and `currencies`; `funds` says which
  dimensions a declared fund states.
- In `countries`, a part declared by region alone reads country
  `unclassified` under its region, and a declared rest of the world reads
  region `other`. Both are declared (quality `declared`), not unknown.
- `unclassified` is real money nothing describes, not zero.
- Sizes are `large` (from 10bn USD market cap), `mid` (from 2bn), `small`;
  `unclassified` where nothing sizes a stock.
- `--country risk|domicile` (on `countries`, `securities`): where the
  business is (default) or where the company is registered.
- **`situs` is not exposure.** It gives the one country whose law each
  holding is an interest under, and looks through no fund: an Irish ETF
  of US stocks is Irish there, and US in `countries`. Use it for an
  estate question, never for allocation. Its `quality` gives what each
  situs rests on (`isin=80%`, `declared`, `custody`, ...), and `funds_pct`
  the share of the row held in funds. `unclassified` is a coin or a fund
  with no known domicile, which a declaration can place, or cash at a
  source whose `exposure.custody` names no country. A declared situs
  changes no other view.

## Spending, income, cash flow

- `spend` and `refunds` are positive; `net_spend` is what a budget counts.
  Own-account moves (card payments, transfers, mortgage payments) are never
  spending. `--level detailed` for a named category (groceries).
  `(uncategorized)` is the backlog: say so when it is large.
- `income` defaults to `--level detailed`. Tax withheld is on the spending
  side; `-C +withheld` shows it beside the income. A private fund's
  `distribution` is a `capital_return`, not income.
- `cashflow`: positive is cash arriving. `operating`, `investing`,
  `financing` and `vehicles` sum to `net_cash_flow`; `operating_in` and
  `operating_out` are positive magnitudes. Retirement, education and health
  plans are `vehicles`: one of them is a row of `flows --level class`;
  `financing` holds the mortgage. These totals are not `income` or
  `spending`'s. `sankey --period`, `sankey --level section` and
  `coverage -x` are refused. `coverage` says per account how far the
  statement can be trusted (`status`: `measured`, `obscured`, `opening`,
  `unmeasurable`).

## Diagnostics and gotchas

- `wealthdb status` — per source, the date its data was last observed and
  whether newer data waits to load. `wealthdb snapshots <source>` — the
  dates gold has, to check a date exists before calling a balance zero.
- A source holds nothing before its first snapshot: an early zero is
  missing history, not a real zero. A blank value means no exchange rate
  to your `-x` currency.
- An agent without a shell uses the MCP server instead (`wealthdb mcp`,
  mcp/README.md).
