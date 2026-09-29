# Demo household

An invented household that shows every wealthdb report and dashboard.
A generator simulates three years of one family's money, day by day,
and writes it as silver sources of the `synthetic` kind. The ordinary
`wealthdb init` and `wealthdb load -a` then build gold from those
sources, so every enrichment runs for real: spending and income
verdicts, transfer pairing, the cash-flow boundary, returns.

Nothing in it comes from real data. Every person, institution,
merchant, company, fund and ticker is invented, and each name was
checked against a web search or public registries so none belongs to
a real business. The two coins keep their real tickers, because they
name an asset class rather than a holding.

## Try it

```sh
make demo                 # build silver and gold into ~/wealthdb-demo
make demo-web             # dashboards on http://127.0.0.1:3100/ (container wealthdb-metabase-demo)
make demo-roll            # later: add the days since the last build, load only those
make demo-web-stop        # stop the demo's dashboards
```

`make demo` takes `WEALTHDB_DEMO_ROOT=` (default `~/wealthdb-demo`),
`AS_OF=YYYY-MM-DD` (default today), `SEED=` and `FINDINGS=1` (below).
The dashboards' admin password is in `web/admin-password.txt` under the
demo root.

The engine reads the demo when its data root and config dir both point
at the demo root:

```sh
demo() { WEALTHDB_DATA_ROOT=~/wealthdb-demo XDG_CONFIG_HOME=~/wealthdb-demo wealthdb "$@"; }

demo -r status
demo -r holdings accounts
demo -r holdings positions -x CHF
demo -r returns sources --method both --period total
demo -r spending categories 2025 --period annual
demo -r income summary 2025 --period annual -C +withheld
demo -r cashflow sankey 2025
demo -r cashflow coverage 2025 --period quarterly
demo -r transactions 2026-05-18 2026-05-18
```

## The household

Two adults, two children, a house with a mortgage and solar panels. One
earner is paid twice a month, the other monthly. Family abroad means a
multi-currency account for summer trips and some foreign holdings.
History starts on 2023-07-01 and runs to the as-of date.

| Source | Accounts | What it shows |
| --- | --- | --- |
| `brindlecove` | checking, savings, a rewards card, the home loan | payroll, bills, the card cycle, ATM cash, savings sweeps, the mortgage split into interest and amortisation |
| `quayvane` | joint brokerage, two Roth IRAs, a rollover IRA, a 529 plan | monthly buying, dividends with foreign withholding, a 2-for-1 split, a fund rename, tax-loss selling, retirement and education vehicles |
| `tamberlow` | two 401(k) plans, a health savings account | payroll contributions that never touch household cash, HSA reimbursements |
| `aubervane` | a discretionary mandate in one portfolio | four currencies, currency conversions, coupons, withholding, management fees |
| `emberwright` | a venture fund interest and an SPV, in one account | capital calls, a distribution, NAV marks, an SPV that exits in listed shares |
| `tessarite` | a crypto exchange account | the `crypto_partial` regime, staking income, a withdrawal to a wallet nothing tracks |
| `driftwren` | a multi-currency account in EUR and GBP | spending abroad, a yearly USD wire paired across currencies |
| `homestead` | the family home | a NAV-only holding valued by yearly appraisals |
| `fx` | none | daily USD rates for EUR, GBP and CHF |

One account is declared rather than collected: `kids-savings`, a
children's savings account at a bank nothing collects. A monthly
transfer to it is an own-account move that stays inside the household.

The in-kind exit writes two rows into the equity-transfer ledger. Two
payments are pinned. Three spending rules and one income rule place
rows the provider does not file. A transfer-override row pairs each
yearly wire across currencies.

## How it is built

`demo/generate.py` reads `demo/household.json` (the persona: accounts,
amounts, schedules, dated events) and `demo/catalogue/` (merchants,
instruments, payers). It reads nothing else: no environment, no real
config, no network. The code is in `demo/demohouse/`:

| Module | Does |
| --- | --- |
| `keyed.py` | randomness keyed by (seed, stream, day), so adding days never changes an earlier day |
| `market.py` | four price factors (stocks, bonds, gold, crypto), instrument prices, exchange rates |
| `book.py` | the ledger: every balance is the running sum of its transactions |
| `household.py` | the day-by-day simulation |
| `silver.py` | writes a full build, or appends the new days to an earlier one |
| `config.py` | renders `wealthdb.cfg` and the ledger CSVs |

The demo root then holds `silver/<source>.db`, `wealthdb.cfg` (every
path in it relative to the root), `overrides/*.csv`, the gold file and
a `.wealthdb-demo` marker. The generator refuses a root that holds
wealthdb files without the marker, and `make demo` deletes a gold file
only beside it.

**Determinism.** Two builds with the same inputs, seed and as-of are
byte-identical. The default seed is `harlow-19`: the first of `harlow`,
`harlow-2`, … whose simulated market lands within half a standard
deviation of the model's median, with no instrument below half its
starting value and the crypto account not down since it opened.

**Appending.** `--append` replays the whole history in memory and adds
only the rows dated after the as-of the files already reached. Each
append records a run whose window is exactly the new days, and the
`synthetic` adapter hands gold only the runs since its last load. So
`make demo-roll` loads the new days and touches nothing older, and the
result matches a fresh build at the same date in every report. An
append refuses an earlier as-of, and it refuses when the generator,
the spec, the catalogue, the seed or the findings switch differ from
the ones that wrote the files. A full build is the way past that.

## The findings switch

`FINDINGS=1` (or `--with-findings`) plants four imperfections for the
diagnostic surfaces:

- about 2% of card purchases from merchants no provider files, so the
  uncategorised share is non-zero;
- one transfer to a brokerage the config does not declare, so one row
  sits in `Unpaired transfers`;
- one month missing from the multi-currency account's statements, so
  coverage shows a gap;
- the venture fund's marks stop 45 days before as-of, so Data
  Freshness shows a stale source.

A findings build is a one-off picture: it cannot be appended to.

## Checks

- `make test-demo` runs the generator's tests: determinism, the
  append contract, the ledger and calendar invariants, and every value
  checked against the engine's own vocabularies (read from its Go
  source). It then loads the household's first months through the
  engine image in a scratch root under the cache dir.
- `demo/check_dashboards.py` runs every card of every dashboard of a
  running demo Metabase through its API: each time window, each source,
  each currency and grain picker. It uses a temporary API key, so no
  filter values stay behind for the admin's next visit.
- `demo/disjoint.py --demo <gold> --live <gold>` lists every
  identifying value a demo gold shares with another gold: ids, names,
  symbols, ISINs, counterparties, descriptions, signatures, and
  (date, amount) pairs. It prints counts and values only.
