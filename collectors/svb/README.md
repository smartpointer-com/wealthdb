# svb — SVB Wealth Advisory historical sideload

A one-shot wealthdb collector that reconstructs a static archive of **SVB
Wealth Advisory / NFS-custodied brokerage** accounts (ids `SV[MRT]-NNNNNN`)
from statement PDFs. SVB wound down in 2023 and these accounts were closed
out, so this is **historical data with no live feed** — there is no `login`
or `download`, only `load`.

It produces an `svb.db` in the **fidelity-web silver schema**, which the shared
Fidelity gold adapter projects into gold under a separate source id (`svb`).
See [DESIGN.md](DESIGN.md) for why a separate source is required and how
carry-forward and closures are modelled.

## Layout

```
$XDG_DATA_HOME/wealthdb/svb/        # the data dir (override: --data-dir)
├── bronze/                         # the statement archive
│   ├── <statement>.pdf  ...        # the in-scope statement PDFs (PII)
│   └── signature.txt               # optional page-1 guard substring (PII)
└── svb.db                          # built silver (override: --silver-db)
```

## Build & run

```sh
make build-svb                      # create the host venv + install pdfplumber
wealthdb-collect svb load           # rebuild svb.db from the PDFs in the data dir
# …or invoke the wrapper directly:
collectors/svb/svb load
```

`load` is a full rebuild, reproducible from the PDFs alone. Extra flags pass
through to `load.py`, e.g.:

```sh
collectors/svb/svb load --closure-date 2023-09-30 --signature "<page-1 substring>"
```

`login` / `download` are no-ops (there is nothing to fetch).

## Gold registration

Register the built DB in your wealthdb config (this is unchanged by where the
collector lives):

```jsonc
{ "id": "svb", "kind": "fidelity", "path": "$XDG_DATA_HOME/wealthdb/svb/svb.db" }
```

`kind: "fidelity"` reuses the Fidelity gold adapter unchanged; `id: "svb"`
gives these accounts their own silver source so carry-forward is
self-contained. Per-account taxonomy (`tax_wrapper` / `management_style`) comes
from `account_overrides["svb"]`.

## Tests

```sh
make test-svb
```

All fixtures are synthetic — no real statements are needed (the PDF-parser seam
is monkeypatched).
