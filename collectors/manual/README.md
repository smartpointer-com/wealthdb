# manual

Part of the **wealthdb** suite — see [the architecture overview](../../DESIGN.md)
for the bronze → silver → gold model and [collectors/README.md](../README.md)
for shared collector conventions.

A catch-all collector for **private holdings that have no bank or portal
behind them** — directly-held real estate, convertible loan agreements
(CLAs) into private companies, and direct equity in a private LLC (a German
GmbH / Swiss AG). Every other collector scrapes or calls a source; this one
has **no source**. There is no source to fetch; two hand-maintained CSVs are the input
by hand; `load` validates them and projects them into a SQLite silver.

> **Status:** implemented end-to-end. `load` is verified against the synthetic
> [examples/](examples/), and the gold adapter
> ([`wealthdb/internal/silver/manual/`](../../wealthdb/internal/silver/manual/))
> is built + registered — the manual source loads into gold and appears in
> `wealthdb holdings positions`. The collector tracks **positions + valuations only**
> (no transactions; see [DESIGN.md](DESIGN.md) §6).

## Tools

| Script | Status | Purpose |
| --- | --- | --- |
| [`load.py`](load.py) | implemented | Validate `positions.csv` / `valuations.csv` and rebuild the SQLite silver from them. Aggressive validation; a bad row fails the whole load with `file:row:column` context. |
| `login.py` | — | **N/A.** No source, no session. `./manual login` is a no-op that prints this. |
| `download.py` | — | **N/A.** No source to fetch. You maintain the CSVs by hand. `./manual download` is a no-op. |

There is no Docker image and no `~/.secrets/manual.env` — there is nothing
to authenticate to.

## Layout

```
$XDG_DATA_HOME/wealthdb/manual/            <- you own this directory (outside the repo)
├── positions.csv            one row per held asset
├── valuations.csv           periodic mark-to-market, one row per (asset, date)
└── manual.db                silver SQLite (written by load; safe to delete + rebuild)
```

## Quick start

Build the `.venv` with `make build-manual` (the host-venv pattern — see
[collectors/README.md](../README.md#build-scaffolding)). Then:

```bash
# 1. $XDG_DATA_HOME/wealthdb/manual/ already holds two fictional starter CSVs
#    (positions.csv / valuations.csv). Edit them in place, replacing the
#    placeholder holdings with your real ones. (examples/ in this repo is a
#    second synthetic sample covering every asset kind, incl. a note→equity
#    conversion — for reference, not for editing.)

# 2. Load — validates the CSVs and (re)builds $XDG_DATA_HOME/wealthdb/manual/manual.db
./manual load
```

`./manual load` resolves its two paths with precedence **CLI flag > env var
> default**:

| path | flag | env var | default |
| --- | --- | --- | --- |
| CSV dir | `--bronze-dir` | `MANUAL_DATA_DIR` (via the wrapper) | `$XDG_DATA_HOME/wealthdb/manual` |
| silver DB | `--silver-db` | `MANUAL_SILVER_DB` | `<bronze-dir>/manual.db` |

(The silver DB follows the resolved CSV dir unless overridden on its own.)
`-v` turns on DEBUG logging. The load is **idempotent** — it fully rebuilds
silver from the current CSVs every run, so editing a CSV and re-running just
reflects the new state (there are no incremental dumps to reconcile).

To try it against the committed synthetic data without touching your own:

```bash
./manual load --bronze-dir examples --silver-db /tmp/manual.db -v
```

## The CSV schema (bronze)

One stable column set per file; everything kind-specific rides in a JSON
`payload` column, so a new asset kind or per-kind field never needs a new
CSV column. Full details + the gold mapping are in [DESIGN.md](DESIGN.md).

**positions.csv** — one row per held asset.

| column | notes |
| --- | --- |
| `id` | your stable id, e.g. `re-001`, `pe-001`, `cn-001`, `pf-001`, `spv-001` (unique) |
| `kind` | `real_estate` \| `private_equity` \| `convertible_note` \| `private_fund` \| `spv` \| `mortgage` \| `other` — the coarse 1-D classification; the gold adapter maps (`kind`, `vehicle`) to the (`asset_class`, `vehicle`) pair. `other` is the catch-all (e.g. a receivable). Add a kind in `load.py`'s `POSITION_KINDS` (one line, no migration). |
| `vehicle` | optional — the wrapper dimension of the 2-D taxonomy (wealthdb docs/TAXONOMY.md): `physical` \| `stock` \| `fund` \| `spv` \| `convertible_note` \| `loan` \| `escrow` \| `mortgage` \| … When blank it defaults from `kind` (real_estate→physical, private_equity→stock, spv→spv, private_fund→fund, convertible_note→convertible_note, mortgage→mortgage, other→other). Set it explicitly for a kind=other row to carry the right wrapper into gold — an escrow receivable is `escrow`, a private loan is `loan`. Full vocabulary in `load.py`'s `POSITION_VEHICLES`. |
| `display_name` | a label (synthetic in any committed file) |
| `currency` | ISO 4217 |
| `acquired_at` | `YYYY-MM-DD` |
| `closed_at` | `YYYY-MM-DD`, blank while still held (set on full disposal / note conversion) |
| `notes` | free text (optional) |
| `payload` | JSON object of kind-specific fields (optional; see below) |

**valuations.csv** — the periodic mark-to-market series (one row per asset
per as-of date). Columns: `position_id`, `as_of_date`, `value`, `currency`,
`notes`, `payload`. The asset's value as of a date is the latest row on or
before it; the valuation dated at the position's `acquired_at` is its cost
basis (gold's book value).

> **No transactions.** The collector tracks positions + valuations only. The
> wires that fund a purchase, pay a fee, or return a distribution are real
> movements in your bank accounts — already captured by the bank collectors —
> so a transactions ledger here would only duplicate them. The acquisition
> date lives on the position (`acquired_at`). See [DESIGN.md](DESIGN.md) §6. A
> note→equity **conversion** is recorded position-side: close the note
> (`closed_at`) and open the equity with `payload.converted_from_position_id`.

### `payload` cheat-sheet

```jsonc
// real_estate
{"property_type": "residential", "ownership_pct": 100, "city": "...", "country": "CH"}
// convertible_note  (0% venture note)
{"principal": 25000, "interest_rate": 0, "cap": 5000000,
 "maturity_date": "YYYY-MM-DD", "conversion_terms": "...", "counterparty": "..."}
// private_equity
{"ownership_pct": 10, "share_cnt": 1000, "fiduciary": "...",
 "converted_from_position_id": "..."}   // last key present only if it came from a note
// private_fund  (LP interest)
{"role": "limited_partner", "commitment": 500000}
// spv  (single-deal vehicle)
{"spv_name": "...", "company": "...", "round": "Series X", "post_money_valuation": 250000000,
 "carry": 0.20, "deal_lead": "...", "platform": "...", "funding_account": "..."}
```

## Validation

`load` rejects (with a `file:row:column` message and non-zero exit) any:
duplicate id; unknown `kind`; bad date / currency / number; a `value`
currency that disagrees with the position's currency; a `valuations`
`position_id` not present in `positions.csv`; a `converted_from_position_id`
that references a position not in `positions.csv`; malformed JSON `payload`;
an unexpected/typo'd column. It warns (but loads) when a valuation
predates the position's `acquired_at`.

## Reclaiming disk (`prune`)

`prune` is a **documented no-op** for `manual`. On the scraping collectors the
fleet-wide verb deletes debug captures and crashed run dirs from the bronze
tree; here it has nothing to act on. `manual` is load-only — no timestamped
bronze run-dirs, and no debug or diagnostic artefacts (there is no download, no
browser, no capture surface to leave anything behind). Bronze is just the two
hand-maintained CSVs sitting flat in the data dir; they are `load`'s only input
and the collector's source of truth, so they are never a prune target. The
shared prune engine walks only timestamped run-dirs, so wiring it here would
leave it permanently empty-handed, and a bespoke root-file sweeper is ruled out
because deleting those CSVs would be unrecoverable — hence the no-op rather than
a real `prune.py`. `./manual prune` prints that explanation and exits 0 (the
`wealthdb-collect` dispatcher forwards the verb, so it must not fail). The
silver `manual.db` is rebuilt from the CSVs by `load` and can be deleted by
hand to reclaim its space.

## Tests

```bash
make test-manual
```

Runs the loader against the synthetic examples and exercises the validation
paths. All fixtures are synthetic placeholders.
