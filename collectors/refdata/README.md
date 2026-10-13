# refdata

Part of the **wealthdb** suite. See [the architecture overview](../../DESIGN.md)
for the bronze → silver → gold model and [collectors/README.md](../README.md)
for the shared collector conventions.

A collector for **reference data**: what each fund holds, and what each
security is. The engine's exposure report
([EXPOSURE.md](../../wealthdb/docs/EXPOSURE.md)) reads it to look
through every fund to the securities underneath.

`refdata` reads only what gold asks for. The engine prints a **wanted
list**: the funds held, their proxies and reference funds, and the
securities held directly. `refdata` fetches what is missing or old, from
public sources, and loads it into its silver. `wealthdb load refdata`
brings it into gold.

There is no login anywhere. Every provider reads public pages.

## Use

```sh
./refdata download --dry-run       # print the requests a run would make
./refdata download                 # fetch what the wanted list lacks
./refdata download --backfill      # also fetch past dates while each fund was held
./refdata load                     # parse bronze into silver
wealthdb load refdata              # bring it into gold
wealthdb exposure coverage         # see what is described, and what is not
```

`download` asks the engine for the wanted list (`wealthdb -r
refdata-wanted`). `--wanted FILE` reads a saved list instead. A run can
be narrowed:

- `--provider NAME` asks only that provider (repeatable);
- `--ref REF` fetches only that fund or security (repeatable);
- `--limit N` fetches at most N files;
- `--no-securities` skips the security lookups.

`login` is a no-op, since there is no session. `login --check` sends one
request to each enabled provider and reports what it answered.

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
  "providers": ["inbox", "sec-nport"],
  "contact": "name@example.org",
  "max_age_days": 30,
  "engine": "/path/to/wealthdb"
}
```

- `providers` are asked in this order. The first that claims a fund
  serves it.
- `contact` goes into the User-Agent. The SEC requires it; the SEC
  providers do not run without it.
- `engine` is the wealthdb wrapper. `REFDATA_WEALTHDB` overrides it;
  `wealthdb` on PATH is the default.

With no file, `inbox` and `sec-nport` run, and `sec-nport` waits for a
`contact`. A setting or a provider name the collector does not know is
an error, so a typo never passes for a switch that is off.

## Providers

### inbox

A composition obtained some other way: a document a fund sent, a
basket from a note's term sheet. It goes into a folder named after the
fund and the day it describes:

```
<data-dir>/inbox/isin_XX0000000001/2026-06-30/holdings.csv
```

The folder name is the fund reference as `wealthdb exposure funds`
prints it, with `_` in place of `:`. A folder holds one composition
file:

- a CSV with a header naming at least `name` and `weight` (percent of
  net assets), and any of `isin`, `cusip`, `ticker`, `country`,
  `currency`, `sector`, `industry`, `asset_type`, `quantity` and
  `market_value`;
- or an N-PORT `primary_doc.xml`.

A download copies each folder into bronze once, beside whatever
provider serves the fund. The inbox itself is never changed.

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

## Pace

Every request goes through one small client:

- one request a second per host, eight a second for the SEC's hosts,
  under the SEC's limit of ten;
- a User-Agent that names the tool;
- GET only, and no cookies.

`download --debug` writes `screenshots/http-trace.jsonl` into the run
dir: one line per request, with its URL, status, timing and size, and
no body. `prune` reclaims it.

## Where the data goes

Each `download` writes one UTC-timestamped run dir under
`$XDG_DATA_HOME/wealthdb/refdata/`:

```
<run>/run.json                                  the manifest: every request and its outcome
<run>/funds/<provider>/<fund>/<YYYY-MM-DD | current>/   the provider's raw files
<run>/securities/<provider>/<key>/
```

One failed request does not fail the run. The manifest follows the
[shared status lifecycle](../README.md#debug-artefacts-run-status-and-pruning-bronze),
so `load` never reads a crashed run. An N-PORT filing is stored
zstd-compressed.

The silver DB is `refdata.db`, beside the run dirs. It mirrors what the
providers state:

| Table | One row per |
| --- | --- |
| `funds` | fund, as a provider names it |
| `compositions` | fund, day and basis (`holdings`, `collateral`, `index`) |
| `lines` | line of a composition, its labels as the provider wrote them |
| `attributes` | fund and day: domicile, replication, hedged currency, lending |
| `counterparties` | fund, day, role and name |
| `securities` | security, day and provider: its identifiers |
| `security_facts` | security, field, day and provider |

A fund and day served again replaces what silver held for them. The
gold adapter is described in
[adapters/refdata.md](../../wealthdb/docs/adapters/refdata.md).

## Reclaiming disk

```sh
./refdata prune --dry-run   # print the plan, delete nothing
./refdata prune             # delete it
```

`prune` removes run dirs that are not complete and the `--debug`
traces. A complete run's raw files are `load` inputs and stay, as do
the inbox and the silver DB.
