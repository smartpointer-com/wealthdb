# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The svb-specific surface below applies on top of
those shared rules.

## What makes this collector different

`svb` is a **one-shot historical sideload**, not a recurring collector. It has
**no source to fetch from**: SVB Wealth Advisory wound down in 2023 and its
statements are a fixed archive of PDFs. There is **no `login`, no `download`,
no auth, no MFA, no Docker, no browser** — only `load`, which parses the
statement PDFs in the data dir into a silver SQLite. Most of root
[CLAUDE.md](../../CLAUDE.md) §1–§3 (read-only sessions, never weaken auth,
protect the cookie jar) does not apply here: there is no session and no
credential.

It deliberately **reuses the fidelity-web silver schema and gold adapter**.
`load.py` builds an `svb.db` in the fidelity silver shape, registered in gold
as `{ "id": "svb", "kind": "fidelity" }`, so the existing Fidelity adapter
projects it — under a *separate* source id, which is load-bearing for the
per-source carry-forward (see [DESIGN.md](DESIGN.md)). The
`migrations/*.sql` here are copies of the fidelity-web silver schema
migrations (schema-bearing ones only — data-only fidelity migrations have no
svb copy) and MUST stay schema-compatible with that gold adapter — keep
schema changes in lockstep, don't let them drift.

## 1. The statement PDFs are pure PII — never copy them into the repo

Root [CLAUDE.md](../../CLAUDE.md) §4 (no private information in source) applies
in **full force** and is the single most important rule for this collector.
The real statement PDFs name a real holder, account ids (`SV[MRT]-NNNNNN`),
holdings, and exact balances. The `signature.txt` page-1 guard contains a real
registration string. None of that may ever reach a tracked file — not source,
not comments, not commit messages, not test fixtures. The real PDFs live
**only** under `$XDG_DATA_HOME/wealthdb/svb/` (outside the repo); both the PDFs
and the derived `svb.db` are git-ignored as a backstop, but the primary rule
is **don't author repo content from real statements**.

**Synthetic placeholders only**, exactly as in the tests: all-zero account
serials in the `SV[MRT]-NNNNNN` shape, example tickers (`AAAA` / `BBBB` /
`VOO`), synthetic 9-char CUSIPs and OCC option codes, round example amounts,
and a fabricated registration signature. Pre-commit: grep the staged diff for
any real account id, holder name, or amount **before** the first `git add`.

## 2. `load` is a full, reproducible-from-bronze rebuild

`load.py` deletes and rebuilds `svb.db` from the PDFs on every run — it is
idempotent and reproducible from the bronze archive alone. It only ever reads
the PDFs under `--bronze-dir` and writes the SQLite at `--silver-db`; there is
no network call and no credential, and it never writes back into the input
PDFs.

## 3. Modelling invariants — don't silently change

The carry-forward (an empty-holdings statement is skipped so the account
carries its last real value forward), the synthetic `$0` closure injected at
`--closure-date`, and the option-leg PK disambiguation are all load-bearing —
see [DESIGN.md](DESIGN.md). Changing any of them shifts the gold history; do it
deliberately, with the tests updated.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication — N/A here,
there is no credential) and §4 (no private information in source — binding,
see §1 above).
