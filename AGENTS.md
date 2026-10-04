# Notes for Claude / coding agents — repo-wide

This is the **wealthdb** monorepo: a Go gold engine under
`wealthdb/` plus a set of bronze+silver collectors under
`collectors/<source>/`. See [DESIGN.md](DESIGN.md)
for the bronze → silver → gold model.

The ground rules below apply across **every** component. Each
collector's own `collectors/<source>/AGENTS.md` carries the
source-specific specifics (which UI surfaces are allowed, which
MFA factor fires, which env vars hold the credentials); this file
is the shared policy they all inherit. All of it is
non-negotiable.

A **new** collector is built by the phased playbook in
[NEW-COLLECTOR-PROMPT.md](NEW-COLLECTOR-PROMPT.md) — a kickoff-prompt
template plus the fleet's accumulated build lessons. The rules below
bind every phase of it.

## 1. Read-only access to every financial source

Each collector drives a fully privileged session — the same login
a human uses to move money, change a strategy, or alter a
beneficiary. There is no read-only sub-scope. Every collector's
contract is that it *only reads*: navigate, filter, export. Never
submit a form, place an order, change a setting, or issue any
non-idempotent `POST` beyond login and read-only export triggers.
The per-source AGENTS.md spells out the exact allow/forbid surface;
when in doubt, treat it as forbidden until the user opts in in
writing. No CLI flag may ever trigger a write action.

## 2. Do not run real sessions unless the user asks

A fresh login fires an MFA challenge to the user's device and may
trip source-side fraud heuristics or lock-out. Allowed without
asking: read code/config/docs, `login --check` (probe the stored
session, no new MFA — amex has no session to probe and reports its
device-trust cookie instead, see
[collectors/README.md](collectors/README.md)), `download --dry-run`
**only where that dry run walks with a session that already exists and
exports nothing**, and unit/fixture tests. That qualifier is the rule,
not a footnote about one collector: a dry run that can sign in or raise
a challenge is a real session whatever the flag is called, and it
belongs with the verbs below. Which of the two a collector's dry run is
belongs in its own AGENTS.md; where nothing says, assume it signs in.
Not allowed unless explicitly asked: any verb that can sign in or fire a
challenge — full `login` (mints a session, pushes MFA), a real
`download`, a `--dry-run` that signs in — plus any live navigation from
a REPL/one-off, and adding scheduling (cron/launchd) that would fire
logins automatically. These toolkits are human-triggered by design.

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
- An env file is a bash script. Read it by sourcing it in a bash
  subprocess (`bash -n FILE` first, then
  `set -a; source FILE; set +a; env -0`), never with a hand-rolled
  `KEY=VALUE` parser — quoting, escapes and `$`-expansion are the
  shell's job.

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
- **The account roster / composition** — which accounts or products a
  real login holds, how many, their types, or the *absence* of one, and
  how many contact points (phone/email) are on file. This is
  source-returned data **even in aggregate** and **even when written as
  scope rationale or a capture note** — the trap is that it reads like
  design context, not data. State a collector's scope by the account
  *kinds* it handles ("deposit accounts: checking and savings; cards out
  of scope"), never by what a real login was observed to contain. (Do not
  reproduce a real roster even as a "forbidden example" — describe the
  shape, not the specifics.)
- **Narrative about a real person's finances** — what someone holds,
  bought, sold, closed or moved, and which institutions they have a
  relationship with. This is private even when no number appears, and
  it is bad documentation besides. Describe the data shape and the
  parsing mechanism impersonally instead.
- **Deployment specifics** — absolute personal paths
  (`/Users/<name>/…`), hostnames, schedules, the real data root. The
  repo stays generic and pluggable; that wiring lives in config
  outside it.

Fine to use: bank names, widely-held example tickers (SPX / QQQ /
VTI), IBAN-spec placeholder letters (`CH<chk><BBBB><RRRR>…`), and a
collector's scope stated as the account *kinds* it handles (a
capability, not a roster). Synthetic examples only — never copy a
real account ID into an example, even in a comment.

**Pre-commit:** grep the staged diff for known real values
*before* the first `git add`, not after — **and** re-read added
comments/docs for the roster/composition prose above, which no
value-grep catches (any statement of account counts, product types
present or absent, or contact points on file). When the user pastes a
captured response or log fragment in chat, strip identifiers before
committing anything derived from it. When in doubt, ask.

A brief for a sub-agent gets the same care. Give the *shape* of a
value (`####XXXXXXXX####`), never a real value, not even masked or
labelled "synthetic" — the agent reuses it verbatim as a fixture, and
it lands in a tracked file. After any agent commit that touches
fixtures, grep the tree for every real token handled in that session,
not just the one that was noticed.

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
  green, and `make lint` clean. Ad-hoc verification is additive,
  never a substitute.
- **Comments and docs describe behaviour, not an operator.**
  Narrate what the code does and why; describe a choice's *effect*,
  not the person making it. Don't lean on a stand-in like "the
  user" / "the owner" — rephrase to remove it. First/second person
  ("you") only in install and license docs. Match the density and
  idiom of the surrounding code.
- **Examples stay synthetic** (§4) — never a real identifier, even
  in a comment, fixture, or doc.
- **A temp file pairs with a trap.** In shell, `mktemp` is followed
  by `trap … EXIT` (or `RETURN` inside a function) so the file is
  removed on an early exit too.
- **Config lives under `$XDG_CONFIG_HOME`** (default
  `$HOME/.config/<tool>.cfg`, or `$HOME/.config/<tool>/` for several
  files), not inside the tool's data tree.

## 7. Build & test entry points

The repo-root `Makefile` is the top-level entry — run it from the
root, no `cd`-ing into subdirectories:

- `make all` / `make test` — build or test everything.
- `make lint` — gofmt and `go vet` over the gold engine, ruff over
  every Python module (rules in the root `ruff.toml`). The pre-commit
  hook (`.githooks/`, installed by `make hooks` or `make install`)
  runs it on the staged tree and refuses a commit it fails on or
  rewrites.
- `make build-<name>` / `make test-<name>` — one component (the
  gold engine is `wealthdb`; e.g. `make test-wealthdb`).
- `make update` — bring deps forward (host venvs, Go modules, base
  images); commit any resulting `go.mod` / `go.sum` bump, never
  revert it.

Underneath, each component also builds directly:

- **Gold engine** (`wealthdb/`): `./wealthdb build` then
  `./wealthdb <subcommand>`; tests via `./wealthdb-test ./...`, any
  other Go command via `./wealthdb-go <subcommand>`. Both run the
  image's toolchain — no host Go, and `make update` resolves the
  module graph there too, so the Dockerfile's base-image tag is the
  only place the Go version is declared.
- **Host-venv collectors** (a `requirements.txt`, no Dockerfile):
  `.venv/bin/python {download,load}.py` (some are load-only — just
  `load.py`).
- **Docker collectors** (a Dockerfile + `entrypoint.sh`): the per-tool
  wrapper (`./<tool> <login|download|load>`) drives `docker run`.

## 8. Documentation

- **Simple English.** One idea per sentence, mostly under 20 words.
  Lists instead of colon-chains. No nested clauses and no dash-asides
  inside a clause. Plain verbs ("reads", "shows", "computes"). The
  audience is international and skims.
- **A README says what the software does, in plain words, with a
  few real examples.** Big picture first, then example commands
  (each one verified to run), then where the output goes. Exact
  syntax, flag inventories, internal mechanism names and config or
  ledger catalogues belong in `help` output and topic docs.
- **Docs describe the present.** No origin story — no "began as",
  "used to", "now also". A feature reads as if it had always been
  there. Version history belongs in release notes and git.
- **No plan files or "deviations from the plan" notes in the
  repo.** Design plans and stage notes live outside the tree; a
  deviation is reported, not committed. This holds even when a task
  brief asks for such a section.
- When a process doc has to name the human, it is "the user", never
  "the owner".

## 9. Login and challenge flows

- **Mirror the provider's own form.** Submit input as entered, show
  the provider's error text verbatim, re-prompt a bounded number of
  times the way the form does, then exit fast with a clear message.
  No guessed code lifetimes, staleness thresholds, carry-forward or
  auto-restart logic: the provider's error is the only reliable
  signal, and re-running is the recovery path.
- **A step that waits for a human sets no tight deadline.** An MFA
  prompt, a browser the human drives, a VNC session: default to an
  hour or more, or wait for an idle/close signal instead of a wall
  clock. A short default assumes someone is waiting at the screen.
