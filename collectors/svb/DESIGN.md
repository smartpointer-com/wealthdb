# svb — SVB historical sideload

A one-shot sideload of three SVB statement families: Wealth Advisory /
NFS-custodied brokerage, Private Bank deposit, and mortgage. These are **static
historical data** that predate the live collectors — built once from statement
PDFs, not scraped on a schedule.

## Pieces

| File | Role |
|---|---|
| `svb` | Host wrapper. `load` rebuilds the silver; `login`/`download` are no-ops (no live source). |
| `pdf_parsers_svbdep.py` | Parser for the **SVB Private Bank deposit and mortgage** statement families. These carry no text layer at all, so they are rastered and OCRed (`collectorkit.pdf.extract_text_ocr` — Apple's Vision framework on macOS, RapidOCR everywhere else) before parsing. Every section is gated on the statement's own arithmetic: the balance summary must close to the cent, each ledger row's running balance must chain from the beginning balance to the stated ending one, and the rows must sort into the same four buckets the summary states — deposits, withdrawals, interest and charges — each to the cent. |
| `pdf_parsers_svbwa.py` | Parser for the **SVB Wealth Advisory / NFS** statement family (a different statement layout from the supplied statements, whose parser is `fidelity-web/pdf_parsers_supplied.py` in the fidelity-web collector). Equity/ETP/fund, fixed-income (inline CUSIP), and **options** rows — with parens→negative for short legs — plus the no-positions/$0 closing form. |
| `statement_tokens.py` | How this bank writes a number and a date, shared by both parsers: parentheses mean negative, a two-digit year pivots the conventional way, and a separator may come through as a colon or a dot where OCR read a hyphen. The parsers each keep the SHAPE their own layout accepts; this is only what a token MEANS once one is found. |
| `derived_marks.py` | Reads the advisor's performance workbook, which carries a month-end value per BROKERAGE account for the months no statement covers — its sheets are keyed by the `SV[MRT]-NNNNNN` serial, and only the brokerage silver DB is passed to it, so the deposit and mortgage families are out of its reach. Used only to fill an INTERIOR gap, never to extend a series, and every row it produces is marked at row level so a statement that later joins the archive takes its month back. |
| `load.py` | Standalone host builder: discovers the bronze PDFs recursively, routes each to a parser by the document family its own text declares, and writes one silver DB per family (`svb.db`, `svb-deposit.db`, `svb-mortgage.db`) in the **fidelity-web silver schema** — holdings plus the statements' Activity rows as `transactions` — synthesising account/portfolio masters. Host venv, no docker: stdlib `sqlite3`, `collectorkit` (`cli`/`silver`/`srcfp`, and `pdf` for the OCR pass), and the two extraction stacks pinned in `requirements.txt`. |
| `migrations/*.sql` | Copies of the fidelity-web silver schema migrations. fidelity-web is at 0008; svb copies 0001–0004 and deliberately stops there: 0005 (activity-id rehash) and 0006 (529 `management_style`) are data-only UPDATEs against rows svb does not have, 0007 only WIDENS the `portfolios.kind` CHECK to admit a value this build never writes (it writes the neutral `other`), and 0008 creates `parser_generations`, which only `collectorkit.silver` writes — this stdlib loader never imports it — and which the Fidelity gold adapter never reads. These DBs are read by that adapter (`kind: "fidelity"`), so any fidelity-web migration that changes a shape svb WRITES or the adapter READS must be copied here — keep those in lockstep with `collectors/fidelity-web/migrations/`. |

## Why three gold sources, not one

Gold ends an account's series at the first later snapshot of its **source**
that re-covers every account seen alongside it. The three statement families
here run on three calendars that collide — a month-end brokerage statement and
a month-end deposit statement share a snapshot date — so under one source id
each family's next statement repeatedly reads as the others' closure, and the
values flicker in and out at every statement date. The build therefore writes
one silver DB per family and the config registers each under its own id:

```jsonc
{ "id": "svb",          "kind": "fidelity", "path": "…/svb/svb.db" }
{ "id": "svb-deposit",  "kind": "fidelity", "path": "…/svb/svb-deposit.db" }
{ "id": "svb-mortgage", "kind": "fidelity", "path": "…/svb/svb-mortgage.db" }
```

`--silver-db` names the brokerage DB, so that id keeps its original path; the
other two sit beside it. They take the same `kind`, so this costs no new
adapter and no gold migration.

A silver is written only for a family the archive actually holds, so an
archive holding one family alone leaves no empty DBs beside its own; an empty
one is indistinguishable from a source whose statements have all been
withdrawn. A file that already exists is still rebuilt
when its family drops out of bronze, so that source empties in gold instead of
standing at the previous load's values, and a path some config already names
never goes missing.

## Why a separate gold source from `fidelity-web`, not a fold-in

The point-in-time gold reports (`report_accounts` / `report_positions`, migration
0021) carry positions forward **per silver source**: a source contributes only
the accounts present at its single latest snapshot ≤ the as-of day. (The daily
history macros resolve their active snapshot per *account* since gold migration
0051, but a later run that re-covers the accounts a statement was loaded
alongside still ends them, so a feed whose statements straddle another's runs
needs its own id there too.) Folding these staggered-date statements into
`fidelity-web` would let an unrelated fidelity snapshot (a later statement or
scrape) supersede and drop them. So the build keeps them out of `fidelity-web`
entirely, under the three ids registered above.

`kind: "fidelity"` reuses the fidelity gold adapter unchanged — it `COALESCE`s
the empty live tables and projects `historical_position_snapshots`, and the
`silver_sources` whitelist is keyed on the adapter *kind*, so no id here needs
whitelisting of its own. Per-account taxonomy (`tax_wrapper` /
`management_style`) comes from `account_overrides[<id>]` in the config, one
block per source id, since gold keys an account on `(silver_source_id,
account_external_id)` and an entry under one id never reaches another's
accounts; the synthesised masters are deliberately neutral (`kind: "other"`) so
they default cleanly and the overrides set the precise values.

## Modelling decisions

- **Carry-forward over an empty unwind.** A statement whose holdings table is
  absent, and which states no portfolio total either, is **skipped**, so that
  account carries its last real value forward until a later statement supersedes
  it, rather than zeroing mid-stream and leaving a gap. When intermediate
  statements are missing, the change shows as one step where the next real
  statement appears, not a dip-and-recover.
- **Real zeros only, and a zero is not a closure.** A $0 month is recorded where
  the statement STATES $0 — its `TOTAL VALUE OF YOUR PORTFOLIO` / `ENDING VALUE`
  line — and nowhere else. Two rules follow. A zero is never inferred from an
  empty holdings table, because a future parse failure would then read as a real
  unwind. And no closure is stamped at all: an account can state $0 one month and
  carry a residual the next (a late dividend landing), so the series ends where
  the statements end. Nothing is invented either: every row is traceable at row
  level — to its statement's sha256, or, for a gap filled from the advisor
  workbook, to the `derived-advisor-mark` marker in the same `source_sha256`
  column (see the derived-mark bullet under *Data caveats*).
- **Activity is read, projections are not.** The statements' Activity region
  carries the dated cash movements — transfers, wires, dividends, withholding,
  fees, interest, corporate actions — and those become `transactions` rows, with
  the amount signed as the statement prints it (parens → negative). The
  exception is an in-kind row, which prints $0.00 in the Amount column and the
  moved figure on a following `TRAN VALUE:` line: that figure becomes the row's
  amount, normalised to the value-flow convention the section totals are struck
  on — as printed on the 2022+ `MISC. & CORPORATE ACTIONS` template, negated on
  the pre-2022 `MISCELLANEOUS & CORPORATE ACTIONS` one, which prints
  cash-equivalent (a receipt of shares parenthesised). Without that negation an
  in-kind receipt books as an outflow of the same size. The trade blotter books
  too, as buy/sell — what moved the money inside the account, and the other half
  of a round trip that would otherwise read as capital appearing from nowhere
  when the proceeds are reinvested at another custodian. The two projection
  sections (pending distributions, trades pending settlement) are read and
  labelled but not booked: a projection settles into a later statement, which
  books it, so booking one double-counts.
- **The statement checks the parse.** Each Activity section strikes its own
  total, and the build reconciles what it parsed against what the statement
  states, section by section, reporting any statement that does not add up.
- **Activity rows carry no instrument key.** The Activity region prints a
  security's NAME, kerned by the extraction, where the Holdings rows print its
  symbol — so an identity derived from it would never reconcile with the real
  one. The rows are shaped for gold to resolve instead: the narrative gold
  stores is what `wealthdb resolve-symbols` looks a transaction up by, and what
  a `symbol_resolution.overrides` entry (`silver_source_id: "svb"`,
  `lookup_kind: "name"`) pins by exact match. The payload also keeps the name
  unprefixed, and the section that says whether it names a security at all.
- **A data row outranks a boilerplate prefix, but not a label.** Holdings rows
  are separated from the surrounding prose by a list of line prefixes, and two
  of those are short enough to match the opening of a security's description
  rather than the prose they were written for — dropping the holding silently.
  A line that parses as a holdings row therefore wins over those two. It does
  NOT win over the rest, which are structural labels: a label tokenises like a
  data row often enough that admitting one would invent a holding out of a
  total, which is the failure the list exists to prevent.
- **An identifier is not a figure.** A CUSIP of all digits reads as numeric and
  joins the row's trailing run, taking it past the width the layout allows —
  further still when the description itself ends in a numeral. The run is cut
  at the CUSIP wherever it falls, so the key is the key and the tail is the
  tail.
- **PK disambiguation.** `historical_position_snapshots` is keyed on
  `(as_of, account, description)`, but option legs share a description (the strike
  is off-description); colliding descriptions are disambiguated by the unique
  instrument key (OCC symbol) so short legs aren't collapsed (which would inflate
  the signed total).

## Data caveats

- Where intermediate statements are missing and the advisor workbook does not
  cover them either, the gaps are carried (not real marks), so a period bounded
  only by its endpoints shows the change as one step.
- An account's series ends at its last statement. Nothing extends it.
- **Deposit and mortgage statements are read by OCR.** They carry no text
  layer, so the text-layer pass reports them as image-only and the loader sends
  them round again through `extract_text_ocr`. A deposit account contributes one
  cash row per statement valued at the balance it states, and its ledger becomes
  transactions; a loan contributes one row valued at MINUS its outstanding
  principal, because the fidelity adapter passes `market_value` through
  unchanged and a liability that arrives positive reads as an asset.
- **Anything the parser turns away is retried at a finer raster before it is
  believed.** An unrecognised title, a registration that did not match, a
  statement whose account headings vanished, a section whose sums did not
  close: each says the recognition missed something rather than that the
  document is unreadable, and each earns the second read. A document is
  rastered at most twice. The two recognisers do not need the same
  resolution: Vision
  reads this archive at the default raster, RapidOCR needs the finer one for
  the worst of the 1-bit scans. The retry is paid only on a document that
  failed, and replaces the first pass only where the arithmetic accepts more of
  it. That is what lets one parser serve both engines: the escalation absorbs
  the difference between them, so neither has to be the one the patterns were
  written for.
- **A section that does not add up reaches silver as nothing.** OCR cannot tell
  a misread digit from a real one, so the statement's own arithmetic does: the
  balance summary must close, each ledger row's running balance must follow from
  the one above it, the last must land on the stated ending balance, and the
  rows must sort into the same four buckets the summary states. A section
  failing any of those is named in the build output and contributes no rows at
  all, so the account carries its last real value forward rather than taking a
  plausible wrong number.
- **A deposit row's KIND comes from the summary, not from its wording alone.**
  The ledger has no transaction column, so what a row IS would otherwise be a
  guess from its description — and guessing wrong books interest as capital
  arriving, which flatters no number but quietly collapses the account's
  measured return to nothing. The statement already states the split four ways,
  so the rows are classified by their own wording and then required to add up
  to it, bucket by bucket. Wording that drifts fails the sum and refuses the
  section; it never falls back to a bare direction, because that is exactly the
  statement where the guess would be wrong.
- **An account can outlive this archive, and gold is what ends it.** A deposit
  product whose bank is acquired keeps running under the acquirer's collector;
  a holding moves custodian. Where one source stops is a fact about two
  sources, not something this collector can know, so it lives in the config's
  `supersession` block, which ends a source's account at a date (the
  `supersession` field in wealthdb `docs/DESIGN.md` §5.1). This loader reads
  every statement it has and knows nothing of a handover. The cut is by date
  rather than by document, so a statement straddling the handover still
  contributes the days that precede it.
- **A loan is tiled against an earlier `manual` position, not overlapped.**
  Where a `manual` position covers the era before a lender's first statement,
  its `closed_at` is set to that statement's date; `closed_at` is exclusive
  (`acquired_at ≤ date < closed_at`), so the two meet exactly once with no gap
  and no double count.
- **A loan statement's payment rows are not booked against the loan.** Where
  the paying account is itself tracked, its own ledger already books the
  outflow, so booking it again against the loan would either double-count the
  payment or invent an inflow. The principal / interest split the lender states
  — which the paying account's ledger does not know — is kept on the loan's
  position row instead.
- **A gap the archive leaves open is filled from a sourced mark, or not at
  all.** Carry-forward alone is not enough here: gold ends an account's series
  at the first later snapshot of its source that re-covers every account seen
  alongside it, so a month the archive misses for one account but covers for its
  peers reads as that account's CLOSURE and its value drops to zero. The advisor
  workbook's month-end value for exactly that month fills it. That fill reaches
  the brokerage family only — the workbook is keyed by brokerage serial, and
  only the brokerage silver DB is passed to it — so a deposit or mortgage gap
  falls to the next bullet. This does not contradict the real-zero rule — that
  forbids INVENTING a value where the statement states one; this sources a value
  where no statement exists. The rules that keep it honest are in
  `derived_marks.py`: only where the archive has nothing, only strictly inside
  an account's own coverage, and every row marked so it is auditable and
  superseded by a real statement.
- **Where even a sourced mark is unavailable, nothing is written.** An account
  the archive never covers is not created; a month before the first statement
  or after the last is not filled.
- **The annual loan statements are not parsed.** They are a dot-matrix form
  whose digits do not survive OCR. They are recognised by title so they are
  counted rather than mistaken for an unreadable monthly statement; everything
  on them is on the monthly statements in a legible form.
- An Activity row whose Transaction column is blank, or holds a verb the parser
  does not know, is reported by statement name and left unbooked rather than
  booked under a guessed kind.

## Rebuild

Reproducible-from-bronze. Drop the in-scope statement PDFs (+ an optional
`signature.txt` page-1 guard and the optional `derived-marks.xlsx` advisor
workbook that fills an interior gap) into `<data-dir>/bronze/` (default
`$XDG_DATA_HOME/wealthdb/svb/bronze/`), in whatever folder layout suits — the
build searches it recursively, in relative-path order — then:

```sh
wealthdb-collect svb load       # rebuild every svb*.db from <data-dir>/bronze/
wealthdb reload svb             # re-project each into gold — all three, since
wealthdb reload svb-deposit     # one build writes all three
wealthdb reload svb-mortgage
```

A bronze dir with no PDFs fails the load without touching the existing silver —
a mis-pointed dir must never replace a good one with an empty rebuild.

PDF parsing dominates a rebuild and is CPU-bound, so `load.py` fans it out
across a process pool and memoises each parse in a persistent sidecar cache
(`$XDG_CACHE_HOME/wealthdb/svb/parse-cache.json` by default; overridable with
`--parse-cache-dir`). The cache is keyed by `(statement sha256, parser-logic
fingerprint, signature set)` — the fingerprint (`collectorkit.srcfp`) covers
both parsers' import closures and the installed pdfplumber / pdfminer.six /
pypdfium2 / ocrmac / rapidocr versions, so editing either parser (or anything
they import) or upgrading either extraction stack auto-invalidates every entry,
while a comment / formatting / docstring edit does not. A recogniser that is
not installed is recorded as unavailable rather than skipped, so a macOS key
and a Linux key differ and a sidecar moved between them re-parses. The
signature component is the sorted set, so reordering `signature.txt` does not
evict the cache. New/changed statements miss and re-parse. Against this static
archive a warm rebuild replays every parse from the sidecar (sub-second) and
emits byte-identical silver. The sidecar holds parsed statement data, so — like
the silver DBs — it lives outside the repo and never under a secrets dir.

A brokerage statement carrying none of the configured registrations is refused
and counted as a `signature-mismatch` in the build census rather than ingested;
the guard runs only once the family is settled, so the image-only families —
which have no text layer for it to read — are not subject to it. For the
invocation and the `--statement-signature` / `signature.txt` mechanics see
[README.md](README.md); for the `--data-dir` / `--silver-db` precedence see the
wrapper contract in [collectors/README.md](../README.md).

## Bronze layout and `prune`

The bronze is a hand-made archive, not a run tree: the statement PDFs sit in
whatever folder layout the archive uses, with an optional `signature.txt` and
an optional `derived-marks.xlsx` at the root. There are no timestamped
`<UTC-ts>/` run dirs — svb has no `download.py`, the statements are
hand-dropped — and no download stage that could leave a screenshot, trace, or
other debug artefact. `build()` discovers the PDFs with `bronze_dir.rglob`,
ordered by relative path, and reads the guard from `<bronze>/signature.txt`;
the workbook defaults to `<bronze>/derived-marks.xlsx` and `--derived-marks`
overrides it.

Because of that, `prune` is a **documented no-op**. The shared
`collectorkit.prune` engine only reclaims debug subdirs of *complete* run dirs
and whole *non-complete* run dirs; its `iter_run_dirs` matches only direct
children of the bronze root named `<UTC-ts>`, and nothing here ever creates one,
so whatever layout the archive uses it yields nothing — and it never touches a
bronze-root file. More to the point, the PDFs, and the workbook beside them,
*are* the `load` inputs and the only copy, so there is no reclaimable disk
here — only inputs that must be kept.
The `svb prune` wrapper arm prints this and exits 0; no `prune.py` is shipped,
since a wired engine would be inert. The same rule that makes `load` refuse an
empty bronze (a mis-pointed dir must never replace a good silver) makes this
collector refuse to delete: **the statement PDFs, and the workbook beside them,
are never removed by `prune`.**

Do not confuse the silver `dump_runs.run_dir = "svb-sleeves-build"` string — an
internal marker *inside* each silver DB so the Fidelity gold adapter's
change-trigger fires on a historical-only build — with a bronze run dir. There
is no bronze run dir and no bronze manifest, so `prune` has nothing to key off
even in principle.
