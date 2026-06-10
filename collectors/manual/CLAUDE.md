# Notes for Claude / coding agents

Shared, repo-wide ground rules (authentication discipline, no PII in
source, git/commit conventions) live in the repo-root
[CLAUDE.md](../../CLAUDE.md). The manual-specific surface below applies on
top of those shared rules.

## What makes this collector different

`manual` is the **odd one out**: it has **no source to fetch from**. The
input is hand-maintained. There is **no `login`, no `download`, no auth,
no MFA, no Docker, no browser** — none of the read-only-session machinery
the other collectors live by. Most of root [CLAUDE.md](../../CLAUDE.md) §1–§3
(read-only sessions, never weaken auth, protect the cookie jar) simply does
not apply here: there is no session and no credential.

The only step is `load`: it reads three hand-maintained CSVs from
`~/wealthdb/manual/`, validates them, and rebuilds a SQLite silver. See
[DESIGN.md](DESIGN.md).

## 1. The bronze CSVs are pure PII — never copy them into the repo

Root [CLAUDE.md](../../CLAUDE.md) §4 (no private information in source)
applies in **full force** and is the single most important rule for this
collector. The user's real CSVs in `~/wealthdb/manual/` name:

- **Real properties** — addresses, cities, the fact of ownership.
- **Private companies, funds & vehicles** — the names of held companies (equity and lending), the venture/PE funds
  and SPVs in the book, and the deals behind them. **A company / fund / SPV name alone is
  identifying.**
- **Counterparties & agents** — lenders, co-owners, sellers, deal leads,
  escrow agents, fund admins, and the external bank accounts that funded a
  deal.
- **Exact figures** — purchase prices, valuations, principal/commitment
  amounts, ownership percentages, rent, dividends, distributions.

None of that may ever reach a tracked file — not source, not comments, not
commit messages, not test fixtures, not "sample" CSVs. The real CSVs live
**only** under `~/wealthdb/manual/` (outside the repo) and the silver
`manual.db` SQLite derived from them stays there too; both are git-ignored as a
backstop, but the primary rule is **don't author repo content from real
holdings**.

**Synthetic placeholders only**, exactly as in [examples/](examples/):
`"Property A"`, `"Swiss CLA #1"`, `"GmbH X"`, `"Swiss Startup AG #2"`,
round example amounts, `"Example City"`. Any new fixture or doc example
must use the same obviously-fake vocabulary. Pre-commit: grep the staged
diff for any real property/company/counterparty name or amount **before**
the first `git add`.

## 2. `load` never writes outside the silver DB

`load.py` only ever: reads the CSVs under `--bronze-dir`, and writes the
SQLite DB at `--silver-db`. It must never write back into the input CSVs
(they are hand-maintained input, not the collector's to mutate) and must
never default any output under `~/.secrets/`. There is no network call and
no credential of any kind.

## 3. Validate loudly; never silently drop a row

The whole value of this collector is that the hand-entered CSVs are
**checked**. A bad row (unknown kind, dangling `position_id`, currency
mismatch, malformed JSON payload, a `conversion` with no target) must fail
the load with `file:row:column` context and a non-zero exit — never be
skipped, coerced, or partially loaded. The load is one transaction: a
single bad row rolls the whole rebuild back, so silver is never left in a
half-updated state. Don't add a "lenient" / "skip-bad-rows" mode.

## Authentication & private data

See the repo-root [CLAUDE.md](../../CLAUDE.md) §3 (authentication — N/A here,
there is no credential) and §4 (no private information in source — binding,
see §1 above).
