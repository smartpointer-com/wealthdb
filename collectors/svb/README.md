# svb — SVB historical sideload

A one-shot wealthdb collector that reconstructs a static archive of SVB
statements — **Wealth Advisory / NFS-custodied brokerage** (ids
`SV[MRT]-NNNNNN`), **Private Bank deposit**, and **mortgage**. SVB wound down
in 2023, so this is **historical data with no live feed** — there is no `login`
or `download`, only `load`.

It writes one silver DB per statement family in the **fidelity-web silver
schema**, which the shared Fidelity gold adapter projects into gold under one
source id each (`svb`, `svb-deposit`, `svb-mortgage`). See
[DESIGN.md](DESIGN.md) for why the families cannot share an id, why they are
kept out of `fidelity-web`, and how carry-forward and stated zeros are
modelled.

## Layout

```
$XDG_DATA_HOME/wealthdb/svb/        # the data dir (override: --data-dir)
├── bronze/                         # the statement archive, searched recursively
│   ├── <account>/<statement>.pdf   # the statement PDFs, in any folder layout (PII)
│   ├── derived-marks.xlsx          # optional performance workbook of
│   │                               # month-end account values, to fill the
│   │                               # brokerage months no statement covers
│   │                               # (PII; --derived-marks)
│   └── signature.txt               # page-1 guard substrings, one per line (PII)
├── svb.db                          # built silver, brokerage (override: --silver-db)
├── svb-deposit.db                  # built silver, deposit accounts (if any)
└── svb-mortgage.db                 # built silver, mortgage loans (if any)
```

Each PDF is classified from its own text, so each statement family reaches its
own parser; the build summary prints a per-family census of everything it saw.
Only a family the archive holds gets a new silver. A silver that already exists
is still rebuilt after its family leaves the archive, so it empties rather than
going stale.

The brokerage statements have a text layer. The deposit and mortgage statements
have none, so they are rastered and OCRed: Apple's Vision framework on macOS
(nothing to install), RapidOCR everywhere else (a pip wheel carrying its own
models). `requirements.txt` picks the right one per platform. A cold rebuild is
slower because of it; the parse cache replays it after that.

## Build & run

```sh
make build-svb                      # create the host venv + the extraction stack
wealthdb-collect svb load           # rebuild every svb*.db from the PDFs in the data dir
# …or invoke the wrapper directly:
collectors/svb/svb load
```

`load` is a full rebuild, reproducible from the PDFs alone. It caches each
parse under `$XDG_CACHE_HOME/wealthdb/svb/`, so a warm rebuild is fast. Extra
flags pass through to `load.py`, e.g.:

```sh
collectors/svb/svb load --statement-signature "<page-1 substring>"
```

`--statement-signature` repeats: a statement is titled by the registration its
account is held under, an archive can span several, and a brokerage statement
must match at least one (a `signature.txt` with one substring per line does the same
thing).

`login` / `download` are no-ops (there is nothing to fetch), and so is
`prune` (see below).

## Reclaiming disk (prune)

`prune` is a **documented no-op** here. The bronze holds the statement PDFs
and the workbook beside them. They are the `load` inputs and the only copy.
There are no run dirs and no debug artefacts, so `prune` prints an explanation
and exits 0. **The statement PDFs are never deleted.** See
[DESIGN.md](DESIGN.md#bronze-layout-and-prune) for the detail.

## Gold registration

`load` writes one silver DB per statement family, each registered under its
own source id:

```jsonc
{ "id": "svb",          "kind": "fidelity", "path": "$XDG_DATA_HOME/wealthdb/svb/svb.db" }
{ "id": "svb-deposit",  "kind": "fidelity", "path": "$XDG_DATA_HOME/wealthdb/svb/svb-deposit.db" }
{ "id": "svb-mortgage", "kind": "fidelity", "path": "$XDG_DATA_HOME/wealthdb/svb/svb-mortgage.db" }
```

`kind: "fidelity"` reuses the Fidelity gold adapter. The three ids
are what keep each family's carry-forward self-contained — see
[DESIGN.md](DESIGN.md). Per-account taxonomy (`tax_wrapper` /
`management_style`) comes from `account_overrides[<id>]`, and all three need
reloading together after a build. A trade, or an income or withholding row
that names a security, the statements cannot tie to a holding reaches gold
with an `instrument_hint`, which a `transaction_instruments[<id>]` entry
closes.

The ids above name one archive. An archive held under a different registration
is a separate run of the same collector — its own `--data-dir`, its own
`--silver-db`, its own `signature.txt` — and so its own ids, because the
accounts are a different holder's and must not share a carry-forward with
these. A run whose archive holds only some of the three families writes only
their DBs; register just those.

## Tests

```sh
make test-svb
```

All fixtures are synthetic — no real statements are needed (the PDF-parser seam
is monkeypatched).
