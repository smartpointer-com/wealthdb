# Notes for Claude / coding agents — repo-wide

This is the **wealthdb** monorepo: a Go gold engine under
`wealthdb/` plus fifteen bronze+silver collectors under
`collectors/<source>/`. See [DESIGN.md](DESIGN.md)
for the bronze → silver → gold model.

The ground rules below apply across **every** component. Each
collector's own `collectors/<source>/CLAUDE.md` carries the
source-specific specifics (which UI surfaces are allowed, which
MFA factor fires, which env vars hold the credentials); this file
is the shared policy they all inherit. All of it is
non-negotiable.

## 1. Read-only access to every financial source

Each collector drives a fully privileged session — the same login
a human uses to move money, change a strategy, or alter a
beneficiary. There is no read-only sub-scope. Every collector's
contract is that it *only reads*: navigate, filter, export. Never
submit a form, place an order, change a setting, or issue any
non-idempotent `POST` beyond login and read-only export triggers.
The per-source CLAUDE.md spells out the exact allow/forbid surface;
when in doubt, treat it as forbidden until the user opts in in
writing. No CLI flag may ever trigger a write action.

## 2. Do not run real sessions unless the user asks

A fresh login fires an MFA challenge to the user's device and may
trip source-side fraud heuristics or lock-out. Allowed without
asking: read code/config/docs, `login --check` (probe the stored
session, no new MFA), `download --dry-run` (walk with the existing
session, export nothing), and unit/fixture tests. Not allowed
unless explicitly asked: full `login` (mints a session, pushes
MFA), real `download`, any live navigation from a REPL/one-off,
and adding scheduling (cron/launchd) that would fire logins
automatically. These toolkits are human-triggered by design.

## 3. Do not weaken authentication

The session token / cookie jar is the keys to the kingdom.

- Never disable, bypass, or downgrade MFA — not even "temporarily
  for testing."
- Credentials reach a tool only via env vars the user manages
  outside the repo (sourced from `~/.secrets/<source>.env`).
  Never add a `--password VALUE` flag (it leaks into `ps` /
  shell history); use `--client-id VALUE` with an env-var
  fallback, never the `--client-id-env NAME` indirection.
- Never persist a password to disk or cache it across runs.
- Never lower a secrets/state file below `chmod 0600`, or store
  it wider than `~/.secrets/`.
- Never default a debug/transient artefact (trace, screenshot,
  log) under `~/.secrets/` — debug paths must be user-provided
  with no secrets-dir fallback.

## 4. Do not leak private information into source

This repo is intended to be publishable. Never write into tracked
files (source, configs, comments, commit messages, test fixtures,
sample drops shared for debugging):

- Login usernames, customer/contract IDs, account numbers, IBANs,
  or any of the many ID formats the sources use.
- Personal data: names, addresses, phone/email, birth dates,
  AHV/SSN fragments, beneficiary identifiers.
- Real session cookies / bearer tokens / MFA codes.
- Any source-returned data: balances, allocations, transactions,
  fees, holdings, document IDs, fund weights.

Fine to use: bank names, widely-held example tickers (SPX / QQQ /
VTI), and IBAN-spec placeholder letters (`CH<chk><BBBB><RRRR>…`).
Synthetic examples only — never copy a real account ID into an
example, even in a comment.

**Pre-commit:** grep the staged diff for known real values
*before* the first `git add`, not after. When the user pastes a
captured response or log fragment in chat, strip identifiers
before committing anything derived from it. When in doubt, ask.

## 5. Git & commit conventions

- **Never `git push`.** Only the user pushes to remotes. After
  `git commit`, stop.
- Commit at meaningful milestones, not on every fix during a
  debug loop.
- Commit messages must not reference user data even indirectly —
  no balances, holdings, account IDs, or "coverage before/after"
  framing. Bank names and example tickers are fine.
- **Messages say WHAT changed and WHY, not HOW — and stay short.**
  A tight subject line that carries the change; add at most a
  sentence or two of context, and only when the subject can't stand
  alone. No per-file blow-by-blow. Match the existing log's voice.
- **No AI-attribution trailer** — no `Co-Authored-By: Claude`, no
  "generated with" line. Use the repo's normal git identity.
- Stage specific files by name; avoid `git add -A` so stray
  secrets/artefacts don't slip in.

## 6. Code quality & style

Hold every change to the bar a senior engineer would.

- **Refactor before you commit.** Make a pass for dead code,
  outdated comments, and obvious simplification / optimization the
  change opens up. No comment may survive that no longer matches
  the code it describes.
- **Comprehensive unit tests, and they must pass.** New behaviour
  ships with tests that cover it; run the relevant `make test-<x>`
  (and the full `make test` before a milestone commit) and keep it
  green. Ad-hoc verification is additive, never a substitute.
- **Comments and docs describe behaviour, not an operator.**
  Narrate what the code does and why; describe a choice's *effect*,
  not the person making it. Don't lean on a stand-in like "the
  user" / "the owner" — rephrase to remove it. First/second person
  ("you") only in install and license docs. Match the density and
  idiom of the surrounding code.
- **Examples stay synthetic** (§4) — never a real identifier, even
  in a comment, fixture, or doc.

## 7. Build & test entry points

The repo-root `Makefile` is the top-level entry — run it from the
root, no `cd`-ing into subdirectories:

- `make all` / `make test` — build or test everything.
- `make build-<name>` / `make test-<name>` — one component (the
  gold engine is `wealthdb`; e.g. `make test-wealthdb`).
- `make update` — bring deps forward (host venvs, Go modules, base
  images); commit any resulting `go.mod` / `go.sum` bump, never
  revert it.

Underneath, each component also builds directly:

- **Gold engine** (`wealthdb/`): `./wealthdb build` then
  `./wealthdb <subcommand>`; tests via `./wealthdb-test ./...`
  (or `go test ./...` with a host toolchain).
- **Host-venv collectors** (`schwab-api`, `ubs-psn`, `fred`,
  `manual`, `svb`): `.venv/bin/python {download,load}.py`
  (`manual` / `svb` are load-only — just `load.py`).
- **Docker collectors** (the other ten): the per-tool wrapper
  (`./<tool> <login|download|load>`) drives `docker run`.
