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
│   ├── derived-marks.xlsx          # optional advisor workbook of month-end
│   │                               # values, to fill the brokerage months no
│   │                               # statement covers (PII; --derived-marks)
│   └── signature.txt               # page-1 guard substrings, one per line (PII)
├── svb.db                          # built silver, brokerage (override: --silver-db)
├── svb-deposit.db                  # built silver, deposit accounts
└── svb-mortgage.db                 # built silver, mortgage loan
```

Each PDF is classified from its own text, so each statement family reaches its
own parser; the build summary prints a per-family census of everything it saw.
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

`load` is a full rebuild, reproducible from the PDFs alone. Extra flags pass
through to `load.py`, e.g.:

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

`prune` is a **documented no-op** here. The scraper collectors keep timestamped
`<UTC-ts>/` run dirs under bronze and let the shared `collectorkit.prune` engine
reclaim their debug-artefact subdirs and abandoned partial dumps. svb has none of
that: its bronze holds the statement PDFs themselves (plus an optional
`signature.txt`), in a hand-made folder layout rather than run dirs, there is
no download stage that could leave a screenshot or trace, and no run dir ever
exists. Those PDFs *are* the `load` inputs — git-ignored and the only copy — so
there is nothing to reclaim and pruning could only put an irreplaceable input at
risk. `prune` therefore prints an explanation and exits 0 without deleting
anything; no `prune.py` is shipped (a wired engine would match no run dir and be
inert). **The statement PDFs are never deleted.**

## Gold registration

`load` writes one silver DB per statement family, each registered under its
own source id:

```jsonc
{ "id": "svb",          "kind": "fidelity", "path": "$XDG_DATA_HOME/wealthdb/svb/svb.db" }
{ "id": "svb-deposit",  "kind": "fidelity", "path": "$XDG_DATA_HOME/wealthdb/svb/svb-deposit.db" }
{ "id": "svb-mortgage", "kind": "fidelity", "path": "$XDG_DATA_HOME/wealthdb/svb/svb-mortgage.db" }
```

`kind: "fidelity"` reuses the Fidelity gold adapter unchanged. The three ids
are what keep each family's carry-forward self-contained — see
[DESIGN.md](DESIGN.md). Per-account taxonomy (`tax_wrapper` /
`management_style`) comes from `account_overrides[<id>]`, and all three need
reloading together after a build.

## Tests

```sh
make test-svb
```

All fixtures are synthetic — no real statements are needed (the PDF-parser seam
is monkeypatched).
