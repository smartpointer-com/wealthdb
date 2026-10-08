# wealthdb

A personal-finance and investment-performance data suite.

It collects holdings, transactions and documents from banks,
brokerages, pension providers and private-market platforms. It
consolidates them into one local database. From that database,
one CLI and an optional set of dashboards answer:

- what was held, anywhere, as of any date;
- what each account, each portfolio and the whole returned;
- what came in and what went out, typed and categorised;
- where the cash went, as a cash flow statement with the household's
  own internal moves taken out.

The collectors download, the engine consolidates, the reports read.
All data stays on the local machine, and no service sits in between,
with two exceptions:

- A login linked through [Plaid](collectors/plaid/). Plaid reads that
  institution and keeps a copy of its data.
- The model that `wealthdb categorize` and `resolve-symbols` ask. When
  it runs on a remote endpoint, it receives the merchant, payer and
  instrument names it is asked about
  ([SPENDING.md §5](wealthdb/docs/SPENDING.md#5-what-leaves-the-machine)).

## ⚠️ Security & liability disclaimer

> [!WARNING]
> **The wealthdb suite handles fully privileged financial-account
> credentials. Read this disclaimer in full before configuring any
> credential anywhere in the suite.**

The [collectors](collectors/) sign in to banks, brokerages, and
pension providers with your credentials and your multi-factor
confirmations. The web-scraping collectors **impersonate a human
browser user** (a stealth-hardened browser session), and the API
collectors hold **write-capable credentials**; in both cases the
session is fully privileged — the same login a human uses to move
money — and no provider offers a read-only sub-scope. Nothing but
the codebase's own discipline restricts the collectors to reading.
Plaid is the exception: its tokens read data and cannot move money.
Plaid's keys and tokens still expose the data of every linked login.
If malicious code were ever introduced into this repository, its
dependency chain, or the container images it runs, it could act on
your accounts with your full authority and cause **irreversible
financial damage, up to the total loss of the assets reachable
from those credentials**.

**You are solely responsible for a thorough, independent security
audit** of this code, its dependency chain, and its runtime images
**before** entrusting the suite with credentials, and again after
every update or rebuild. If you cannot perform such an audit, do
not hand this software real credentials. Automated access may
additionally breach a provider's terms of service; verifying that
your use is permitted is likewise your responsibility.

**No warranty; no liability.** This software is provided “AS IS”,
without warranty of any kind, express or implied, including but
not limited to the implied warranties of merchantability, fitness
for a particular purpose, title, and non-infringement. To the
maximum extent permitted by applicable law, **SmartPointer AG and
the contributors accept no responsibility for, and shall not be
liable for, any claim, damages, or other liability** — whether in
an action of contract, tort, or otherwise — arising from, out of,
or in connection with this software or its use, including without
limitation unauthorized or erroneous transactions, loss of funds
or other assets, credential or data compromise, account suspension
or termination, and any direct, indirect, incidental, special,
consequential, or punitive damages. Your use is entirely at your
own risk. See [LICENSE](LICENSE) for the governing terms. This
software is not affiliated with, endorsed by, or sponsored by any
financial institution; nothing in this repository is financial,
legal, or tax advice.

## What it does

wealthdb gives one private, complete view of a household's money,
across every account it collects, and answers the questions a household
asks of it.

**Net worth and allocation, as of any day.** What is held, where, and
what it is worth, in one currency at that day's rates. Holdings are
classified by what they are exposed to and how they are held, so the
answer can be "a third listed equity, a fifth real estate, a tenth of
it inside retirement plans" rather than a list of tickers.

**Investment performance.** Time-weighted and money-weighted returns
for each account, each portfolio, each provider and the whole, over any
period. Money moved between the household's own accounts is not counted
as gain or loss, and every figure carries its own caveats.

**Income and spending.** What arrived — wages, interest, dividends,
rent — and what left, grouped by category, across all cards and
accounts. The household's own transfers are taken out, so a move from
checking to savings never looks like spending.

**Cash flow.** A cash flow statement for the household: what came in,
what went out, what was invested or sold, what went into or came out of
retirement plans and trusts, and what was left. As a table, and as a
Sankey diagram.

A few of the questions it answers, and the commands behind them:

```sh
wealthdb holdings global -d 2024-12-31 -x CHF        # net worth at the end of 2024, in CHF
wealthdb returns portfolios 2025                     # how each portfolio did in 2025
wealthdb spending categories 2025 --period annual    # last year's spending by category
wealthdb income summary 2025-01 2025-03 -C +withheld # Q1 income, with the tax withheld beside it
wealthdb cashflow sankey 2025                        # the year's cash flow as a diagram
```

Everything is available from the command line, in tables, CSV or JSON,
from a set of local dashboards (`wealthdb web`): net worth,
allocation, returns, spending, income and cash flow, and to AI agents
over MCP (`wealthdb mcp`). A privacy mode
shows percentages instead of amounts, for a screen others may see.
`wealthdb help` lists every command with its flags; the topic documents
under [Documentation](#documentation) cover each feature in depth.

## Use with AI agents

The same reports are safe to put in front of an AI agent. An agent is
genuinely useful when it can answer questions over a complete financial
picture. It is genuinely dangerous when the way to get there is handing
it banking credentials: a fully privileged e-banking login in the hands
of a probabilistic, prompt-injectable system is a standing invitation
for irreversible damage. wealthdb keeps the two apart:

1. **Credentials are handled only by static, reviewable code.**
   The collectors are deterministic scripts — auditable line by
   line, human-triggered, with MFA challenges answered by a
   person. No agent drives a banking session, and no agent ever
   sees a credential. A login linked through Plaid is handled by
   Plaid's code instead. Its sign-in starts on a page Plaid hosts,
   and Plaid holds the access to the institution. The collector
   keeps Plaid's keys and one token per login. A token reads data
   and cannot pay.
2. **The collectors only read.** Their contract is navigate,
   filter, export — no code path submits a form, places an order,
   or changes a setting, and no CLI flag can enable one.
3. **The data lands locally.** Everything is parsed into local
   databases and consolidated into one queryable gold store;
   nothing is sent to any third-party service. The two exceptions
   are named at the top: Plaid, and a remote model that
   `categorize` and `resolve-symbols` ask.
4. **Agent access is read-only by construction.** The MCP server
   (`wealthdb mcp`, [mcp/README.md](mcp/README.md)) serves the
   reports as tools: it has no tool that writes, it opens the
   database read-only for one call at a time, and its container
   mounts the data read-only. An agent with a shell can run the CLI
   instead, with `--read-only`. Either way the surface exposed to an
   agent is consolidated, local, read-only queries — and nothing
   else.

The result is a clean separation. An agent runs the same reports a
person does, through the MCP server or `wealthdb --read-only`. The
server's privacy endpoint, or `-p` on the CLI, keeps amounts out of a
transcript. No agent is ever given write access to the financial data,
let alone the banking credentials that produced it.

## Data sources

There is one collector per provider. All of them use the same `login`,
`download` and `load` verbs. [collectors/README.md](collectors/README.md)
has the full table.

- **Banks** — [UBS](collectors/ubs-web/) (netbanking and the
  [PSN feed](collectors/ubs-psn/)), [Swissquote](collectors/swissquote/),
  [Raiffeisen Austria](collectors/raiffeisen_at/),
  [Chase](collectors/chase/), [First Citizens](collectors/firstcitizens/);
  [American Express](collectors/amex/) cards.
- **Brokerages** — [Schwab](collectors/schwab-api/) (the Trader API and
  the [client web](collectors/schwab-web/)), [Fidelity](collectors/fidelity-web/).
- **Pensions** — [VIAC](collectors/viac/) (pillar 3a),
  [Relevate](collectors/relevate/) (pillar 2 vested benefits).
- **Crypto** — [CoinTracking](collectors/cointracking/).
- **Aggregators** — [Plaid](collectors/plaid/) reaches the banks,
  brokers and card issuers it covers, one linked login at a time.
- **Private markets** — [AngelList](collectors/angellist/),
  [Carta](collectors/carta/), [EquityZen](collectors/equityzen/).
- **Archives and reference** — [SVB](collectors/svb/) statement
  archives (load-only), [`manual`](collectors/manual/) for holdings
  with no portal — property, private loans, escrow claims — and
  [FRED](collectors/fred/) for historic FX.

## Adding a data source

wealthdb is designed to be extensible. Adding a bank, brokerage or
pension provider means writing one collector and its adapter. The
rest of the suite is untouched.

- Every collector is a self-contained program with the same four
  verbs: `login`, `download`, `load` and `prune`. It owns its raw
  downloads (bronze) and its parsed, source-shaped SQLite (silver).
  It never touches gold.
- The gold engine reads that silver through an adapter: one Go
  package that implements one interface and yields canonical records.
  An adapter never sees the gold schema.
- The shared [`collectorkit`](shared/collectorkit/) library supplies
  the common parts: the CLI, credential files, browser sessions and
  two-factor prompts, bronze and silver plumbing, and statement
  parsers. A new collector adds only what is specific to its source.

[NEW-COLLECTOR-PROMPT.md](NEW-COLLECTOR-PROMPT.md) is the playbook
for building a collector with an AI coding agent. It has a kickoff
prompt template, a phased build plan, the protocol for the person who
drives the live banking sessions, and the lessons from the existing
fleet. Almost every collector in this repository was built that way.
The agent never sees a credential; the person answers the login and
two-factor prompts.

## How it is built

Four parts, and a demo, in one repository:

| Part | What it is | Runs as |
| --- | --- | --- |
| [`collectors/`](collectors/) | One program per source. Each logs in, downloads the raw files (bronze) and parses them into a source-shaped SQLite database (silver). | Docker, or a Python venv |
| [`wealthdb/`](wealthdb/) | The gold engine. It reads every silver database into one canonical DuckDB store and serves the reports. | Go, in Docker |
| [`web/`](web/) | The optional dashboards: Metabase over a read-only snapshot of gold. | Docker |
| [`mcp/`](mcp/) | The optional MCP server: the reports as tools for AI agents, read-only over live gold. | the engine image, in Docker |
| [`demo/`](demo/) | An invented household, generated into synthetic silver sources, for trying wealthdb without a bank. | Python, stdlib only |

```
collectors/<source>/         wealthdb/
  download  → bronze            load  ─┐
  load      → silver  ────────────────┼─→ gold (DuckDB)  →  wealthdb holdings | returns
  (one SQLite per source)             │                     wealthdb spending | income | cashflow
                                      │                     wealthdb transactions
                                      │                     wealthdb web  (Metabase, over a snapshot)
                                      │                     wealthdb mcp  (AI agents, read-only)
```

Sources only meet at gold. A collector never reads another collector's
data and never touches gold; the engine reads silver and never writes
it. Every part has a `README.md` (usage). Most add a `DESIGN.md`
(internals; the engine's is under `wealthdb/docs/`) and an `AGENTS.md`
(rules for coding agents). [DESIGN.md](DESIGN.md) describes the
bronze → silver → gold model.

## Getting started

Needs `git`, `make`, Docker and Python 3.10 or newer, on macOS or
Linux. No Go toolchain: the engine builds inside its container.

```sh
git clone <this repo> && cd wealthdb
make all                          # build the engine, the dashboards and every collector
make install                      # put wealthdb and wealthdb-collect on PATH (~/.local/bin)

wealthdb-collect list             # the collectors
wealthdb-collect viac login       # sign in to one source; answer its MFA prompt in the terminal
wealthdb-collect viac download    # fetch the raw data (bronze)
wealthdb-collect viac load        # parse it into the source's database (silver)

wealthdb config                   # setup wizard: gold path, currency, the sources to read
wealthdb init                     # create the gold database
wealthdb load -a                  # consolidate every source (gold)
wealthdb holdings global          # the first report
wealthdb web start                # the dashboards (optional; see web/README.md)
wealthdb mcp start                # the reports for AI agents (optional; see mcp/README.md)
```

Credentials go in `~/.secrets/<source>.env`; each collector's README
names the variables. Data lands under `$XDG_DATA_HOME/wealthdb`, which
defaults to `~/.local/share/wealthdb`. `make` alone lists every build
and test target. [collectors/README.md](collectors/README.md) has the
flags all collectors share — data locations, the `--lookback` window —
and [wealthdb/README.md](wealthdb/README.md) the engine's subcommands.

## Try it without a bank

A demo household shows every report and dashboard before a single
source is set up. It is invented from end to end. A generator writes a
family's banking, investing and spending, from mid-2023 to today, into
synthetic sources, and the ordinary `load` builds gold from them.

```sh
make demo                         # build the demo into ~/wealthdb-demo
make demo-web                     # its dashboards on http://127.0.0.1:3100/
make demo-mcp                     # its reports for an AI agent, on http://127.0.0.1:3400/mcp
make demo-roll                    # later: add the days since the last build
```

The demo keeps to its own directory, its own gold and its own
dashboard and MCP containers, so its data never mixes with a real
setup's. It
does share the engine and dashboard images: the demo targets build them
from the checkout, as `make all` does. [demo/README.md](demo/README.md)
describes the household and how to query it from the command line.

## Documentation

- **[DESIGN.md](DESIGN.md)** — suite-wide pipeline,
  layer ownership, canonical model.
- **[wealthdb/docs/DESIGN.md](wealthdb/docs/DESIGN.md)** — the gold
  engine: schema, configuration, adapter contract, load semantics,
  every subcommand.
- **[RETURNS-NOTES.md](wealthdb/docs/RETURNS-NOTES.md)** — the returns
  method and its conventions; **[SPENDING.md](wealthdb/docs/SPENDING.md)**,
  **[INCOME.md](wealthdb/docs/INCOME.md)** and
  **[CASHFLOW.md](wealthdb/docs/CASHFLOW.md)** — the three readings of
  the enrichment engine; **[TAXONOMY.md](wealthdb/docs/TAXONOMY.md)** —
  the `asset_class` × `vehicle` classification.
- **[AGENTS.md](AGENTS.md)** — agent ground rules shared across
  every component (security, PII, read-only access).
- **[NEW-COLLECTOR-PROMPT.md](NEW-COLLECTOR-PROMPT.md)** — the
  playbook for building a collector with a coding agent.
- **[collectors/README.md](collectors/README.md)**,
  **[wealthdb/README.md](wealthdb/README.md)**,
  **[web/README.md](web/README.md)**,
  **[mcp/README.md](mcp/README.md)** — usage of each part; the
  `DESIGN.md` beside each covers its internals.

## License

Released under the [MIT License](LICENSE).
Copyright (c) 2026 SmartPointer AG.
