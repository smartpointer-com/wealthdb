# manual

Part of the **wealthdb** suite — see [the architecture overview](../../DESIGN.md)
for the bronze → silver → gold model and [collectors/README.md](../README.md)
for shared collector conventions.

A catch-all collector for **private holdings that have no bank or portal
behind them** — directly-held real estate, convertible loan agreements
(CLAs) into private companies, and direct equity in a private LLC (a German
GmbH / Swiss AG). Every other collector scrapes or calls a source; this one
has **no source**. A few hand-maintained CSVs are the input; `load` validates
them and projects them into a SQLite silver.

> `load` is exercised against the synthetic [examples/](examples/), and the
> gold adapter
> ([`wealthdb/internal/silver/manual/`](../../wealthdb/internal/silver/manual/))
> loads the manual source into gold, where it appears in
> `wealthdb holdings positions`. The collector tracks **positions + valuations only**
> (no transactions; see [DESIGN.md](DESIGN.md) §6).

## Tools

| Script | Purpose |
| --- | --- |
| [`load.py`](load.py) | Validate `accounts.csv` / `positions.csv` / `valuations.csv` / `cost_basis.csv` and rebuild the SQLite silver from them. Aggressive validation; a bad row fails the whole load with `file:row:column` context. |
| `login.py` | **N/A.** No source, no session. `./manual login` is a no-op that prints this. |
| `download.py` | **N/A.** No source to fetch; the CSVs are hand-maintained. `./manual download` is a no-op. |

There is no Docker image and no `~/.secrets/manual.env` — there is nothing
to authenticate to.

## Layout

```
$XDG_DATA_HOME/wealthdb/manual/            <- hand-maintained data dir (outside the repo)
├── accounts.csv             OPTIONAL: one row per pseudo-account (tax sleeve)
├── positions.csv            one row per held asset
├── valuations.csv           periodic mark-to-market, one row per (asset, date)
├── cost_basis.csv           OPTIONAL: capital paid in so far, one row per (asset, date)
└── manual.db                silver SQLite (written by load; safe to delete + rebuild)
```

## Quick start

Build the `.venv` with `make build-manual` (the host-venv pattern — see
[collectors/README.md](../README.md#build-scaffolding)). Then:

```bash
# 1. Create $XDG_DATA_HOME/wealthdb/manual/positions.csv + valuations.csv
#    (and accounts.csv if the book spans more than one tax sleeve,
#    cost_basis.csv if a commitment is paid in over time).
#    Copying examples/ (a synthetic sample covering every asset kind, incl.
#    a note→equity conversion and a second sleeve) makes a good skeleton;
#    replace the placeholder holdings with your real ones.

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

**accounts.csv** — one row per pseudo-account. **Optional**: leave the file
out and every position lands in one account (`manual`, kind `other`,
`taxable_personal` / `self_directed`), which is right for a book with one
owner and one tax treatment.

Add it when two holdings sit in different **tax sleeves** — a stake held
through a trust or a company is not your own taxable property, and rolling
both into one account makes every wrapper-grained report wrong. An account
here is a *declaration*, not something fetched: it says "these positions are
held under this wrapper, managed this way".

| column | notes |
| --- | --- |
| `id` | a hand-assigned stable id, e.g. `manual`, `sleeve-b` (unique). Referenced by `positions.account_id`. |
| `display_name` | a label (synthetic in any committed file) |
| `account_kind` | canonical gold `account_kind` — `other` for a directly-held asset with no institutional container, which is the usual answer here. Full vocabulary in `load.py`'s `ACCOUNT_KINDS`. |
| `tax_wrapper` | optional — canonical gold `tax_wrapper` (`trust_non_grantor`, `custodial_utma`, `pillar_3a`, …). Blank means `taxable_personal`. `load.py`'s `TAX_WRAPPERS`. |
| `management_style` | optional — `self_directed` \| `advisory` \| `discretionary` \| `automated`. Blank means `self_directed`. |
| `notes` | free text (optional) |
| `payload` | JSON object (optional) |

All three taxonomy columns are checked against the canonical gold
vocabularies at load time, so a near-miss (`trust` for `trust_non_grantor`)
fails with the CSV row number in hand rather than landing unnoticed in gold.

> A per-account entry in `wealthdb.cfg`'s `account_overrides` still wins on
> overlap — the loader applies config after the adapter stamps. Declaring the
> sleeve here is the better place for a hand-maintained source, because it
> sits next to the positions it describes.

**positions.csv** — one row per held asset.

| column | notes |
| --- | --- |
| `id` | a hand-assigned stable id, e.g. `re-001`, `pe-001`, `cn-001`, `pf-001`, `spv-001` (unique) |
| `account_id` | optional — which `accounts.csv` row holds it. Blank means the default account above. Naming an account the file does not declare is an error, not a silently created sleeve. |
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
basis (gold's book value), unless `cost_basis.csv` covers the position.

**cost_basis.csv** — optional. The capital paid into a position as of a
date, for a holding whose cost is not its first valuation: a fund
commitment paid in over several capital calls, say. Columns:
`position_id`, `as_of_date`, `amount`, `currency`, `notes`. Each row is
the total paid in so far, gross of any capital paid back. A position with
rows here takes its book value from them: the latest row on or before the
date, and none before the first row. Positions without rows keep the
valuation at `acquired_at`.

> **No transactions.** The collector tracks positions + valuations only. The
> wires that fund a purchase, pay a fee, or return a distribution are real
> movements in the bank accounts — already captured by the bank collectors —
> so a transactions ledger here would only duplicate them. The acquisition
> date lives on the position (`acquired_at`). See [DESIGN.md](DESIGN.md) §6. A
> note→equity **conversion** is recorded position-side: close the note
> (`closed_at`) and open the equity with `payload.converted_from_position_id`.
> The one exception is value arriving from **another tracked vehicle** with no
> bank in between — sale proceeds an agent holds back — whose cash half lives
> in the equity-transfer ledger, not here (DESIGN.md §6).

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

## Maintaining the files

Every edit is just a line in a CSV followed by `./manual load`. The load
rebuilds silver from scratch each time, so there is no state to reconcile and
no way to get a half-applied change: fix the line, run it again.

**Add a holding.** One row in `positions.csv`, and one row in
`valuations.csv` dated *exactly* its `acquired_at` — that first valuation is
the cost basis, and without it the holding has none. Then add marks as they
arrive.

**Record a capital call.** For a commitment paid in over time, add a row
to `cost_basis.csv` on each call date with the total paid in so far. The
book value then follows the capital called, not the commitment.

**Re-mark it.** One row in `valuations.csv` per (asset, date). The value on
any date is the latest row on or before it, so marks carry forward: a
property appraised every few years needs a row only when the appraisal
lands, not one a year. Marks may be irregular and far apart.

**Close it.** Set `closed_at` on the position. It drops out of the portfolio
from that date — no row is deleted, so history before it stays intact. Pick
the date the value actually leaves: for a sale, the day the cash arrives in
whichever account wealthdb already tracks, so the holding hands over to that
deposit with no gap and no overlap.

**Convert a note to equity.** Close the note (`closed_at`) and open the
equity with `payload.converted_from_position_id` naming the note. The load
checks that back-reference resolves. There is no transaction either side —
the conversion is recorded position-side, because no cash moved.

**Move a holding into a sleeve.** Add the sleeve to `accounts.csv`, set the
position's `account_id`. Nothing else changes; existing positions without an
`account_id` stay where they were.

**Record a holding with derived marks.** When no statement reports a
holding's value, enter it as a position and mark it from whatever evidence
exists. Say in `notes` and `payload` that the marks are derived, so a later
reader does not mistake them for reported figures.

**A receivable left by a sale.** Proceeds an agent holds back after a sale
are a position of their own (`kind=other`, `vehicle=escrow`). It opens the
day the sale closes, is carried at principal, and is marked down as claims
are paid. The sold holding closes at the cash it produced, and the
receivable carries the rest. Together they should account for the gross,
less any fee; that check catches a wrong share count. The *cash* half of a
release with no bank in between belongs in the equity-transfer ledger
rather than here: [DESIGN.md](DESIGN.md) §6.

### What does NOT belong here

Cash. A wire that funds a purchase, pays a fee or returns a distribution is a
real movement in a bank account another collector already captures, so
recording it here would double it. The acquisition date lives on the
position; the money lives with the bank. See [DESIGN.md](DESIGN.md) §6.

Anything a source can be scraped or exported from. A brokerage, a bank, a
crypto exchange — those get their own collector, and one that fetches beats
one that is typed.

### Before you run it

```bash
./manual load --bronze-dir <your dir> --silver-db /tmp/check.db -v
```

Validates against a throwaway DB and leaves the real silver untouched. Worth
it after a bulk edit: the load is all-or-nothing, but seeing the failure
before touching the real file is cheaper than reasoning about it afterwards.

## Validation

`load` rejects the whole load, with a `file:row:column` message and a
non-zero exit, on any of these:

- a duplicate id;
- an unknown `kind`;
- an `account_kind`, `tax_wrapper` or `management_style` outside the
  canonical gold vocabulary;
- a bad date, currency or number;
- a `positions` `account_id` not present in `accounts.csv`;
- a `valuations` or `cost_basis` `position_id` not present in
  `positions.csv`;
- a `valuations` or `cost_basis` currency that disagrees with the
  position's;
- two rows for one (asset, date) in `valuations.csv`, or in
  `cost_basis.csv`;
- a `converted_from_position_id` that names a position not in
  `positions.csv`;
- a malformed JSON `payload`;
- an unexpected (typo'd) column.

It warns (but loads) when a valuation predates the position's `acquired_at`,
and when an account holds no position — that one is usually a typo in an
`account_id` or a sleeve left behind after its last holding closed.

## Reclaiming disk (`prune`)

`prune` is a **documented no-op** for `manual`. On the scraping collectors the
fleet-wide verb deletes debug captures and crashed run dirs from the bronze
tree; here it has nothing to act on. `manual` is load-only — no timestamped
bronze run-dirs, and no debug or diagnostic artefacts (there is no download, no
browser, no capture surface to leave anything behind). Bronze is just the
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
