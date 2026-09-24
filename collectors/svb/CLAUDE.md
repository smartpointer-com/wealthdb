# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The svb-specific surface below applies on top of
those shared rules.

## What makes this collector different

`svb` is a **one-shot historical sideload**, not a recurring collector. It has
**no source to fetch from**: SVB wound down in 2023 and its statements are a
fixed archive of PDFs covering brokerage, deposit and mortgage accounts. There
is **no `login`, no `download`, no auth, no MFA, no Docker, no browser** — only
`load`, which parses the statement PDFs in the data dir into the silver
SQLites. Most of root [CLAUDE.md](../../CLAUDE.md) §1–§3 (read-only sessions,
never weaken auth, protect the cookie jar) does not apply here: there is no
session and no credential.

It deliberately **reuses the fidelity-web silver schema and gold adapter**.
`load.py` builds one silver DB per statement family in the fidelity silver
shape — `svb.db`, `svb-deposit.db`, `svb-mortgage.db` — each registered in gold
under its own id (`{ "id": "svb" | "svb-deposit" | "svb-mortgage", "kind":
"fidelity" }`), so the existing Fidelity adapter projects all three. The ids are
separate from `fidelity-web`'s and from each other, which is load-bearing for
the per-source carry-forward (see [DESIGN.md](DESIGN.md)). The
`migrations/*.sql` here are copies of the fidelity-web silver schema
migrations, and MUST stay schema-compatible with that gold adapter: a
fidelity-web migration that changes a shape svb writes or the adapter reads has
to be copied here, while one that touches neither needs no copy (DESIGN.md
§Pieces lists the current exceptions). Keep those in lockstep, don't let them
drift.

## Two extraction stacks, one of them OCR

The brokerage statements have a text layer; the deposit and mortgage statements
have none at all, and are rastered and read by OCR — Apple's Vision framework on
macOS, RapidOCR elsewhere. **Neither is privileged: this suite is not macOS-only,
and a change here must keep both working.** The two do not read a page
identically, so nothing may depend on the exact characters one of them returns.

What makes that safe is that **every OCRed section is gated on the statement's
own arithmetic**, and a section that does not close contributes nothing. Never
relax a gate to recover coverage: a plausible wrong balance is worse than a
missing one. The gates are also what allow the patterns to tolerate the
characters OCR loses, and what drives the retry at a finer raster — the one
engine-specific difference, handled once rather than per pattern.

## 1. The statement PDFs are pure PII — never copy them into the repo

Root [CLAUDE.md](../../CLAUDE.md) §4 (no private information in source) applies
in **full force** and is the single most important rule for this collector.
The real statement PDFs name a real holder, account ids — the brokerage
`SV[MRT]-NNNNNN`, a 10-digit deposit account number, a 10-digit loan number
(printed under both `Account Number:` and `MORTGAGE LOAN NO.`) — holdings,
counterparty names in the deposit ledger rows, and exact balances. The deposit
account number is also re-stamped *inside* ledger rows as an `ID: NNNNNNNNNN`
continuation line, so even a single pasted transaction row carries it
mid-description. The `signature.txt` page-1 guard contains a real
registration string. The advisor workbook is PII on the same footing, and is
easy to overlook because it is a spreadsheet rather than a statement: one
sheet per account, named by that account's own serial, every row a real
month-end value. None of that may ever reach a tracked file — not source, not
comments, not commit messages, not test fixtures. All three inputs live
**only** under `$XDG_DATA_HOME/wealthdb/svb/` (outside the repo); the PDFs, the
workbook and the derived `*.db` files are git-ignored as a backstop, but the
primary rule is **don't author repo content from real statements**.

**Synthetic placeholders only**, exactly as in the tests: all-zero account
serials — the `SV[MRT]-NNNNNN` shape for brokerage, all-zero 10-digit serials
for the deposit accounts and the loan, distinguished only by the last digit —
example tickers (`AAAA` / `BBBB` / `VOO`), synthetic 9-char CUSIPs and OCC
option codes, round example amounts, and a fabricated registration signature.
Pre-commit: grep the staged diff for any real account id, holder name, or
amount **before** the first `git add`.

## 2. `load` is a full, reproducible-from-bronze rebuild

`load.py` deletes and rebuilds the silver DBs from the PDFs on every run —
`svb.db` plus the `-deposit` and `-mortgage` DBs that `silver_paths()` derives
beside `--silver-db` — so it is idempotent and reproducible from the bronze
archive alone. A family the archive does not hold gets no DB, so a
single-family archive leaves none empty beside its own; one already built is
still rebuilt after its family leaves the archive, so it empties rather than
going stale. Every input sits under `--bronze-dir`: the statement
PDFs, the `signature.txt` page-1 guard, and the optional `derived-marks.xlsx`
advisor workbook. It writes those SQLite DBs and a parse-cache sidecar
under `--parse-cache-dir`; there is no network call and no credential, and it
never writes back into the input PDFs.

## 3. Modelling invariants — don't silently change

The three-way split into one silver DB and one gold source id per statement
family (a single id makes the families close each other — see
[DESIGN.md](DESIGN.md)), the rules that bound a derived mark to an interior gap
inside an account's own coverage, the carry-forward (a statement with neither a
holdings table nor a stated total is skipped so the account carries its last
real value forward), the real-zero rule (a `$0` month comes only from a stated
`$0`, and a zero is never treated as a closure), the option-leg PK
disambiguation, and the Activity sign conventions are all load-bearing — see
[DESIGN.md](DESIGN.md). Changing any of them shifts the gold history; do it
deliberately, with the tests updated.

So is the instrument-link proof. A row is linked to a holding only when the
statements' quantities force it; a name only proposes the candidates. Never
relax that to a name match to recover coverage: a plausible wrong link moves
money between asset classes in every report, and an unlinked row is at least
visibly untracked. Nor may a link change what a row already is — its activity
id, description and amount are what gold's pins and categorisation key on.

The signs deserve their own warning. Gold reads a wire's direction off the
amount's sign alone, so an inverted parse reverses a large transfer instead of
failing. Two verb families print against intuition — a withholding reversal is a
credit, a dividend clawback a debit — and the corporate-actions section's
transaction-value convention flips between the archive's two statement
templates. Each is covered by a sign fixture; keep it that way.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication — N/A here,
there is no credential) and §4 (no private information in source — binding,
see §1 above).
