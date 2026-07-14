# svb — SVB Wealth Advisory historical sideload

A one-shot sideload of a family of SVB Wealth Advisory / NFS-custodied brokerage
statements (account ids of the form `SV[MRT]-NNNNNN`). These are **static
historical data** that predate the live collectors — built once from statement
PDFs, not scraped on a schedule.

## Pieces

| File | Role |
|---|---|
| `svb` | Host wrapper. `load` rebuilds the silver; `login`/`download` are no-ops (no live source). |
| `pdf_parsers_svbwa.py` | Parser for the **SVB Wealth Advisory / NFS** statement family (a different statement layout from the supplied statements, whose parser lives in the `fidelity-web` collector). Equity/ETP/fund, fixed-income (inline CUSIP), and **options** rows — with parens→negative for short legs — plus the no-positions/$0 closing form. |
| `load.py` | Standalone host builder: parses the bronze PDFs into a `svb.db` that uses the **fidelity-web silver schema**, injects the closures, and synthesises account/portfolio masters. Stdlib `sqlite3` + `pdfplumber`; no `collectorkit`/docker. |
| `migrations/*.sql` | Copies of the four fidelity-web silver migrations. `svb.db` is read by the Fidelity gold adapter (`kind: "fidelity"`), so these MUST stay schema-compatible with it — keep them in lockstep with `collectors/fidelity-web/migrations/`. |

## Why a separate gold source (`svb`), not a fold-in

The gold history macros (`report_*_history`, migration 0022) and the point-in-time
`report_accounts` (0021) carry positions forward **per silver source**: a source
contributes only the accounts present at its single latest snapshot ≤ the as-of
day. Folding these staggered-date statements into `fidelity-web` would let an
unrelated fidelity snapshot (a later statement or scrape) supersede and drop
them. So the build emits a separate `svb.db` and the config registers it as

```jsonc
{ "id": "svb", "kind": "fidelity", "path": "$XDG_DATA_HOME/wealthdb/svb/svb.db" }
```

`kind: "fidelity"` reuses the fidelity gold adapter unchanged (it `COALESCE`s the
empty live tables and projects `historical_position_snapshots`); `id: "svb"` gives
these accounts their own `silver_source_id`, so carry-forward is self-contained. No
new Go adapter and no gold migration — the `silver_sources` whitelist is keyed on
the adapter *kind*, which is already allowed. Per-account taxonomy
(`tax_wrapper` / `management_style`) comes from `account_overrides["svb"]` in the
config; the synthesised masters are deliberately neutral (`kind: "other"`) so they
default cleanly and the overrides set the precise values.

## Modelling decisions

- **Carry-forward over an empty unwind.** A statement with no holdings is
  **skipped**, so that account carries its last real value forward until a later
  statement supersedes it, rather than zeroing mid-stream and leaving a gap. When
  intermediate statements are missing, the change shows as one step where the next
  real statement appears, not a dip-and-recover.
- **Synthetic $0 closures.** When the real closing statements aren't
  machine-readable, the builder injects a $0 row for every still-held account at
  `--closure-date`, so a closed account zeroes out at its closing date.
- **PK disambiguation.** `historical_position_snapshots` is keyed on
  `(as_of, account, description)`, but option legs share a description (the strike
  is off-description); colliding descriptions are disambiguated by the unique
  instrument key (OCC symbol) so short legs aren't collapsed (which would inflate
  the signed total).

## Data caveats

- Where intermediate statements are missing, the gaps are carried (not real
  marks), so a period bounded only by its endpoints shows the change as one step.
- Where closing statements aren't machine-readable, only the $0 terminal state is
  modelled.
- Cash accounts and tax forms in the same source folders are **out of scope**
  and excluded from the bronze.

## Rebuild

Reproducible-from-bronze. Drop the in-scope statement PDFs (+ an optional
`signature.txt` page-1 guard) into `<data-dir>/bronze/` (default
`$XDG_DATA_HOME/wealthdb/svb/bronze/`), then:

```sh
wealthdb-collect svb load   # rebuild svb.db from <data-dir>/bronze/
wealthdb reload svb         # re-project into gold
```

A bronze dir with no PDFs fails the load without touching the existing
`svb.db` — a mis-pointed dir must never replace a good silver with an empty
rebuild.

PDF parsing dominates a rebuild and is CPU-bound, so `load.py` fans it out
across a process pool and memoises each parse in a persistent sidecar cache
(`$XDG_CACHE_HOME/wealthdb/svb/parse-cache.json` by default; overridable with
`--parse-cache-dir`). The cache is keyed by `(statement sha256, parser-logic
fingerprint, signature)` — the fingerprint (`collectorkit.srcfp`) covers the
parser's import closure and the pdfplumber / pdfminer.six versions, so editing
`pdf_parsers_svbwa.py` (or anything it imports) or upgrading the extraction
stack auto-invalidates every entry, while a comment / formatting / docstring
edit does not. New/changed statements miss and re-parse. Against this static archive a warm
rebuild replays every parse from the sidecar (sub-second) and emits
byte-identical silver. The sidecar holds parsed statement data, so — like
`svb.db` — it lives outside the repo and never under a secrets dir.

The wrapper resolves `--data-dir` / `--silver-db` (CLI flag > `SVB_*` env >
`WEALTHDB_*` env > default `<data-dir>/svb.db`) and forwards extra flags to
`load.py`, e.g. `wealthdb-collect svb load --closure-date 2023-09-30`. Running
`load.py` directly works too: `collectors/svb/svb load`.

## Bronze layout and `prune`

The bronze is a **flat archive**, not a run tree: the statement PDFs (plus an
optional `signature.txt`) sit directly at the bronze root. There are no
timestamped `<UTC-ts>/` run dirs — svb has no `download.py`, the statements are
hand-dropped — and no download stage that could leave a screenshot, trace, or
other debug artefact. `build()` reads the PDFs with `bronze_dir.iterdir()` (files
whose suffix is `.pdf`) and the guard from `<bronze>/signature.txt`.

Because of that, `prune` is a **documented no-op**. The shared
`collectorkit.prune` engine only reclaims debug subdirs of *complete* run dirs
and whole *non-complete* run dirs; its `iter_run_dirs` matches only `<UTC-ts>/`
slugs, so on this flat layout it yields nothing and never touches a bronze-root
file. More to the point, the PDFs *are* the `load` inputs and the only copy, so
there is no reclaimable disk here — only inputs that must be kept. The `svb
prune` wrapper arm prints this and exits 0; no `prune.py` is shipped, since a
wired engine would be inert. The same rule that makes `load` refuse an empty
bronze (a mis-pointed dir must never replace a good silver) makes this collector
refuse to delete: **the statement PDFs are never removed by `prune`.**

Do not confuse the silver `dump_runs.run_dir = "svb-sleeves-build"` string — an
internal marker *inside* `svb.db` so the Fidelity gold adapter's change-trigger
fires on a historical-only build — with a bronze run dir. There is no bronze run
dir and no bronze manifest, so `prune` has nothing to key off even in principle.
