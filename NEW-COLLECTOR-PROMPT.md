# Building a collector with a coding agent

[collectors/README.md](collectors/README.md) defines **what** a
collector is — the anatomy, verbs, CLI conventions, and shared
libraries. This file defines **how one gets built**: the process that
produced the existing fleet, distilled from nearly twenty
agent-assisted builds. Almost every collector in this repo was written
by an AI coding agent working from a kickoff prompt, with the account
user driving the live banking sessions. The process converged on a
phased playbook whose latest runs take a new source from empty
directory to a validated gold adapter in one or two days.

The file has four parts: how to run a build, a copy-paste **kickoff
prompt template**, the **user protocol** (what the human does), and
the **fleet lessons** — the recurring traps every earlier build paid
for. The template references the other sections; an agent given
the template should read this whole file plus the docs it names.

> [!WARNING]
> A collector build ends with real, fully privileged banking sessions.
> Read the security & liability disclaimer in the
> [repo README](README.md) first. The process below exists to keep the
> agent away from credentials and live sessions — do not relax that
> separation for convenience.

## How to run a build

1. **Prerequisites.** A working checkout (`make` runs, Docker works,
   `make base-images` has been run for browser collectors) and a VNC
   viewer for explore sessions (macOS Screen Sharing works; any VNC
   client on Linux). Commands below invoke the wrapper directly
   (`collectors/<name>/<name> <verb>`); after `make install`,
   `wealthdb-collect <name> <verb>` is equivalent. Credentials live
   in a file the user creates by hand — `~/.secrets/<name>.env`,
   chmod `0600` — when the agent states the exact variable names at
   the end of scaffolding (explore occasionally adds one later, such
   as a region or contract code). The agent never sees credential
   values, only the file's variable *names*.
2. **Have an agent draft the kickoff — don't hand-fill the
   template.** Start a throwaway *authoring* session in the repo root
   and say, roughly:

   > Draft a kickoff prompt for a new `<bank>` collector from the
   > template in NEW-COLLECTOR-PROMPT.md. Interview me for the scope
   > and the "What is already known" section, do the repo recon
   > yourself (archetype, template siblings, whether anything needed
   > already exists), and give me the finished prompt to copy.

   The agent fills the mechanical placeholders from the repo and
   turns the knowns section into a short interview — answering
   questions beats writing prose, and repo recon has caught features
   that already existed. This is how the fleet's own kickoffs were
   written.
3. **Review the draft before dispatch.** The scope and the forbid
   list deserve the user's eyes; iterate in the authoring session until
   the prompt reads right (see
   [Writing the knowns](#writing-the-knowns-section)).
4. **Paste the finished prompt into a fresh session** in the repo
   root and build there — never in the authoring session. One
   collector per session: the build gets a clean context with the
   brief as its very first message, which also survives context
   compaction intact, unlike interview chatter. (A lighter one-session
   variant — point the build agent at this file plus a short brief
   and let it interview then build — works, but forfeits the review
   checkpoint and buries the brief mid-conversation.)
5. **Expect** the agent to work offline most of the time and to stop
   at explicit gates: it scaffolds, then asks for an explore session;
   builds, then asks for live validation. The user's hands-on time
   is a handful of short live sessions — each explicitly requested,
   each with the 2FA device at hand. Recent builds needed roughly
   half a dozen short logins end to end.
6. **Phases are user-triggered.** The agent finishes a phase, commits,
   and lists what it needs next; the user says go. Nothing touches
   the live bank unprompted, ever.

### Choosing a template collector

Every build copies the newest validated sibling of its archetype and
diverges only where a capture proves the new source differs — including
the explore harness, which is deliberately copy-adapted per collector,
never extracted into a shared library. Pick the archetype from the
Auth/Runtime columns of the roster table in
[collectors/README.md](collectors/README.md), newest sibling first
(date candidates with `git log --diff-filter=A -- collectors/<name>`
and prefer rows the roster notes as validated):

| Archetype | Marker |
| --- | --- |
| Browser scrape, 2FA every login | scraped session; login folds into `download` (one browser lifetime per run) |
| Browser login + REST data | bot-defense sensors on the login endpoints only; durable device trust; separate `login` verb, data via `page.request` |
| Pure REST / API key / SFTP | no browser at all; `login --check` is the cheapest authenticated read |
| Load-only sideload | no source to fetch; hand-dropped files, documented no-op `prune` |

Which archetype a new source is cannot be known up front — the explore
phase measures it. Scaffold on the closest guess (a Camoufox browser
scraper is the safe default for an unknown bank) and let the captures
decide; de-escalating to plain REST after discovery is cheap and has
happened before.

## The kickoff prompt template

The block below is what the authoring agent instantiates: every
`<...>` placeholder replaced (by repo recon for the mechanical slots,
by user interview for the knowledge slots), inapplicable parts
deleted. Keep the structure: knowns → scope → phases → rules →
stop-gate. The hard rules restate repo CLAUDE.md deliberately — a
kickoff that binds the agent explicitly outperforms one that assumes
the rules will be discovered.

```markdown
Build a new wealthdb collector for <BANK / PLATFORM> (entry point
<URL>) — <account kinds in scope, e.g. "retail deposit accounts
(checking + savings)">, read-only. Repo: <path to the wealthdb
checkout>. Collector name: `<name>` (kebab-case per
collectors/README.md; add a country suffix when the brand operates
distinct banking systems per country). Env prefix: `<NAME>_`.

Read before writing anything, in this order: repo-root CLAUDE.md
(non-negotiable ground rules), DESIGN.md (bronze → silver → gold),
collectors/README.md (the collector contract: anatomy, verbs, CLI
conventions, collectorkit), NEW-COLLECTOR-PROMPT.md (the build
playbook — its "Fleet lessons" bind this build), and the docs + source
of the template collectors: <newest validated sibling(s) of the same
archetype — read their CLAUDE.md and DESIGN.md "Observed" sections>.
Copy the newest sibling's shape, including its explore harness
(copy-and-adapt is the tracked convention — do not extract a shared
library), and diverge only where a capture proves this source differs.

## What is already known (user-provided — do not re-derive, do verify)

Treat every item as a hypothesis to confirm during explore; record
confirmations and refutations in DESIGN.md "Observed" sections. Wrong
entries are cheap when framed this way — correct them and move on.

- 2FA: <push approval in the bank's app / SMS or email OTP / TOTP /
  QR scan / unknown — observe>.
- Session & device trust: <e.g. "no trusted-browser bypass observed;
  still measure whether any trust survives browser restarts — this
  decides the verb split">.
- Exports: <formats seen in the UI, or unknown>.
- Statements: <monthly archive / generated on demand / unknown;
  how far back, if known>.
- Anti-bot: <known wall, or unknown — assume hostile until measured>.
- UI language: <if not English: never key selectors on display text;
  prefer ids/test-ids/ARIA; record load-bearing labels in DESIGN.md.
  Repo prose stays English.>
- API: <known public/partner API status, or unknown — do not assume a
  clean JSON API; capture all XHR traffic and decide scrape-vs-REST
  from what it shows>.

## Scope

- <Account kinds> only: overview, transaction history, exports,
  statement/document downloads. Everything else is forbidden — money
  movement in every form (transfers / payments / standing orders /
  bill pay), cards <unless in scope>, account lifecycle, product
  offers, profile/settings/2FA-method mutations, the message center,
  and any other product surface the same login may expose. Triggering
  a read-only export (generating a statement) is allowed.
- Write collectors/<name>/CLAUDE.md (the allow/forbid surface) before
  any live session, erring wide on forbid, and a DESIGN.md skeleton
  listing the open questions the first explore must answer.

## Phases

1. **Scaffold** — adapt the template sibling's harness: explore.py
   with DOM-snapshot-per-distinct-screen capture AND response-body
   capture (network + click logs alone pin no selectors and leave
   every JSON shape a guess) — every DOM serialisation goes through
   `debugcap.scrub_dom(..., redact)`, which the collectorkit suite
   enforces on any module that serialises a page, and every capture path
   through a `debugcap.secret_redactor(username, password)` masker, which
   that suite only enforces in `explore.py`, so a capture written from a
   `login.py` or a `download.py` is the author's to route. Neither
   masker reaches a file the browser writes itself, so a recorded HAR is
   cleaned after the context close with `debugcap.redact_har()`, and the
   collectorkit suite fails a harness that records one without it. A
   Playwright trace cannot be rewritten at all and holds the credential
   verbatim. Dockerfile on the matching
   shared base image (Camoufox for unknown or hostile bot defense — it
   costs nothing if the site turns out lenient); wrapper via
   shared/wrappers/wrapper-lib.sh; collectorkit for CLI, bronze, and
   silver plumbing. Credentials from ~/.secrets/<name>.env
   (<NAME>_USERNAME / <NAME>_PASSWORD — map onto whatever the login
   form actually asks for once explore shows it); env only, never
   argv; ask the user to create the file, stating the exact variable
   names. Verify make build-<name> and make test-<name> resolve via
   the root Makefile's auto-discovery.
2. **Explore** (live, user-triggered) — verify the knowns and answer
   the open questions, recorded as dated DESIGN.md "Observed"
   sections: bot-defense stack, and which endpoints carry the sensor
   headers (that one observation decides browser-for-login-only vs
   browser-everywhere); web technology; login form shape; the 2FA
   challenge's network/DOM shape and its completion signal; session
   persistence and device trust across browser restarts (script a
   second short run without --fresh, the flag that forces the cold
   first-device path by setting the saved profile aside — whether 2FA
   fires again is the single measurement that decides the verb
   architecture); transaction history depth, pagination, and
   export formats with their columns (stable ids? running balance?
   booking vs value dates?); statement listing reach. Prefer two or
   three short scripted sessions — plus targeted one-document
   micro-captures for wire-format questions — over one long tour.
3. **login / download** — from the captures, propose to the user the
   verb split (2FA fires every login → login folds into download;
   durable device trust → a separate login verb) and the runtime tier
   (browser everywhere / browser-for-login + REST via page.request /
   no browser), then build offline. Non-negotiables: 2FA is driven
   from the terminal — announce the challenge, print any comparison
   code (read from the DOM, never from a network event), auto-detect
   approval by polling; a vnc-login fallback verb may exist, but CLI
   2FA is an acceptance criterion, not a preference — even a QR
   factor can be terminal-driven, since an ANSI-rendered QR scans
   fine from banking apps (the QR-based sibling's precedent).
   Authentication is proven only by possession of a working
   credential — a cheap
   read-only authenticated probe returning success; NEVER by URL or
   SPA route (pre- and post-auth pages share URLs; this false
   positive has bitten three collectors). Never fire any
   authenticated-surface request mid-challenge — it can invalidate
   the challenge server-side. Give every browser-event dependency a
   poll-based twin and pump the event loop in every wait. Bronze runs
   follow the fleet layout (run.json status lifecycle, accounts
   roster, per-account exports/JSON/statements; diagnostics only
   behind --debug). Wire the standard flags via collectorkit.cli
   (add_standard_args / resolve_standard) — the conventions table in
   collectors/README.md is binding; a standard flag the source cannot
   honour parses cleanly and warns loudly, never silently no-ops.
4. **load** — bronze → SQLite silver (JSON1 payloads), migrations,
   idempotent (re-load is a clean no-op; --force = delete + rebuild),
   unit tests on synthetic fixtures only (synthesize values from
   scratch — never transform real ones; placeholder patterns per root
   CLAUDE.md §4). The ledger source is whatever the capture proves
   richest — the SPA's own JSON usually supersedes CSV/HTML exports;
   check before writing a parser. Key rows on the source's most
   stable identifier (immutable id, legal name — display labels
   churn); synthesize deterministic ids where the source offers none;
   snapshot timestamps carry the source's as-of time, never fetch
   time; amounts are signed per the fleet convention from day one.
5. **Statement backfill** (conditional — build only if the measured
   history floors say the transaction export is date-capped while
   statements reach further back). If built: parse the
   `pdftotext -layout` text `collectorkit.pdftotext` extracts; gate
   every import on beginning + Σ == ending reconciliation (skip and log
   what doesn't reconcile — never import a mis-parse); import only rows
   strictly older than the export seam (the oldest row the transaction
   export reaches — the chase collector's DESIGN.md documents the
   reference implementation); attribute multi-product statements per
   segment by balance chaining, stopping on ambiguity, never guessing.
6. **Gold adapter** — wealthdb/internal/silver/<name>/ modeled on the
   newest comparable adapter (contract: wealthdb/docs/DESIGN.md §6):
   the Adapter/Connection interface, a silver_sources whitelist
   migration, the blank import in wealthdb/cmd/wealthdb/main.go, a
   co-located ReturnsPolicy (policy.go beside adapter.go, plus the
   test blank-import lists in internal/returns/policy_registered_test.go
   and internal/gold/returns_policy_import_test.go — a gold guard test
   fails when a whitelisted kind registers no policy), fixture tests.
   Map accounts and transactions into the canonical
   enums; unrecognised values fall through to `other` with the raw
   value in payload — never fail the load. Validation gate: the user
   registers the new source in their private wealthdb.cfg
   (per-deployment config, never committed — schema in
   wealthdb/docs/DESIGN.md §5), rebuilds the gold image, runs the
   gold load plus a holdings/transactions report against the real
   silver, and pastes the output; the build is done when that output
   reconciles against the source's own UI and shows no unexpected
   `other` fallthroughs.

## Working rules (non-negotiable)

- NEVER start a live session unprompted. Logins fire real 2FA at the
  user's device, and repeated attempts trip fraud lockouts (a
  sibling collector was soft-blocked after ~6 rapid logins in a day).
  Every live run is explicitly requested with the user present.
  Treat each failed login as a burned experiment: diagnose from
  captures, respect multi-hour cooldowns, never fire logins in quick
  succession. Use long timeouts (1h+) whenever waiting on the user.
- Structure every live handoff as: the exact command for the user's
  own terminal + what to have ready (phone) + what success looks like
  + "paste the output". Escalate cheap-to-expensive: login --check
  and download --dry-run before a real run, the full backfill last,
  and a --fresh run to validate the untrusted-device path before
  sign-off.
- Interactive prompts read stdin and need a real TTY — the docker
  wrapper passes stdin only when stdin and stdout are both TTYs.
  Verify early, or hand the command to the user. (A pure
  push-approval flow may need no stdin at all — confirm it.)
- Iterate cheap: when the user lends a session, hold it
  (session-holder pattern — watch a trigger file, importlib.reload
  bind-mounted code against the live page) instead of paying a 2FA
  per probe. The holder must exercise the production code paths, not
  re-implementations.
- When a control won't click or a wait hangs, capture ground truth
  (frame DOM + shadow-skeleton dumps) before the second guess.
- No PII in tracked files — including the account roster/composition
  prose trap (root CLAUDE.md §4): scope is stated as account kinds,
  never as what the login was observed to hold. Synthetic fixtures
  and placeholders only, format-valid and obviously fake. Sweep
  source, fixtures, docstrings, docs AND the drafted commit message
  before git add — and mind grep's exit codes in && chains.
- Tests green (make test-<name>) before every commit; a refactor pass
  (dead code, stale comments, comments encoding disproven theories)
  before milestone commits; squash to milestone commits with tight
  WHAT-level messages carrying no user data; never git push.
- Keep DESIGN.md and CLAUDE.md current in the same commit as each
  change: record each live-run root cause as a fleet lesson, and
  correct the allow/forbid surface when discovery reclassifies a
  surface.
- End every phase with: commit + PII sweep + docs updated + a
  numbered list of exactly what is needed from the user next. Leave
  genuine judgment calls to the user, stated with a default.

Start with phase 1 (scaffold + CLAUDE.md + DESIGN.md skeleton), then
stop and ask for the first explore session to be run.
```

## Writing the "knowns" section

The single highest-leverage part of the template. Spend thirty minutes
in the bank's UI before the authoring interview and note what the
login asks for, which 2FA factor fires, what export buttons exist, how
far back statements go, and anything idiosyncratic about the login
screen — the answers land here whether typed directly or elicited by
the authoring agent's questions. Frame every item as *verify, don't re-derive*: the agent
treats each as a hypothesis, and explore confirms or refutes it. Both
outcomes are wins — a confirmed known skips discovery; a refuted one
gets corrected for free, and whole phases have been dropped this way
when their premise turned out false. What no prompt can know (an
unusual login control, an undocumented endpoint) surfaces during
explore; the user's eyes on the live UI routinely catch what captures
miss. Narrating observations back in prose — and wandering
off-script — has repeatedly surfaced load-bearing endpoints that no
scripted walk would have visited.

## The user protocol

The division of labour is strict: the agent writes and tests code
offline; the user runs everything that touches the live bank. Live
sessions are the scarce resource the whole process is shaped around.

**Explore sessions.** `explore` is a wrapper verb on the collectors
that ship a discovery harness (`collectors/<name>/<name> explore`);
the wrapper forwards a VNC port (5900–6000, announced at start).
Either side may start the container — it carries no credentials until
the user types them — and the agent hands over the VNC address plus
a scripted walk (which screens to visit, which controls never to
click). The user signs in and drives —
credentials and 2FA are always submitted by the human; the harness
only pre-fills and records. Closing the browser ends the capture. Run
the short second pass without `--fresh` when asked: whether 2FA fires
again is the single measurement that decides the collector's verb
architecture.

**Live validation.** Run the exact command the agent hands over, in a
real terminal, with the 2FA device at hand, and paste the full output
— tracebacks included — back into the chat. Pasted terminal output is
the primary debugging channel; `--debug` captures (DOM snapshots,
screenshots) land under the collector's debug directory (default
`~/.cache/wealthdb/debug/<name>`) for the agent to read afterwards —
those captures carry real response bodies, so the directory is
sensitive and nothing derived from it lands in a tracked file
unstripped. Approve pushes when the script announces them;
relay OTP codes promptly (they expire in minutes). After a rate-limit
stall, wait a few hours and time the single confirming retry
yourself — the agent verifies success from artefacts rather than
burning another login.

**Decisions the user keeps.** Scope (which account kinds), the verb
split and runtime tier the agent proposes from explore evidence, the
backfill go/no-go, anything with destructive semantics, and every
change to the private `wealthdb.cfg` or the real gold store. The agent
proposes with a stated default and waits. Conversely, the agent may —
without asking — read code and captures, run `login --check`,
`download --dry-run`, and unit tests (root [CLAUDE.md](CLAUDE.md) §2).

**Lending a session.** The user may explicitly lend an authenticated
session to the agent for bounded iteration ("iterate on download
until the exports work"). The lend is a per-task grant, not a
default — absent it, the no-unprompted-live-sessions rule stands. The
session-holder pattern makes lending cheap: one login, then the agent
hot-reloads code against the held page. Anything needing fresh
authentication queues for the user's return.

**Concurrent builds.** One collector per agent session. When several
collector sessions run at once, the user is the lock manager for the
shared gold-layer code: explicit freeze/unfreeze messages, each agent
staging only its own collector's files.

## Fleet lessons

The distilled scar tissue of the existing fleet. The kickoff template
compresses these into rules; this section records them with their
reasons, grouped by theme. Several were hit repeatedly *after* being
written down as prose warnings — which is why the template states them
as hard rules rather than advice.

### Authentication and sessions

- **Authenticate on proof of a working credential, never on the URL.**
  SPAs serve the same URL logged-out and logged-in; three collectors
  in a row declared victory on a pre-login shell and scraped nothing.
  The only trustworthy signal is a cheap read-only authenticated
  request succeeding. Harvest tokens from two independent places
  (request headers and the token response body) so one dropped browser
  event cannot lose them.
- **Never touch an authenticated-only endpoint mid-challenge.** One
  collector's login loop probed its roster endpoint every few seconds
  for progress; during the login→2FA transition that probe invalidated
  the challenge server-side — SMS sent, UI frozen, and an evening of
  misdirected debugging before the probe was gated onto post-login
  routes only.
- **Session persistence and device trust are independent axes.**
  Measure both (close the browser, rerun without `--fresh`). Trust
  that persists while the session cookie dies means a separate `login`
  verb; 2FA-on-every-login means login folds into `download` with one
  browser lifetime per run. A profile directory on disk is not a live
  session — several banks invalidate server-side the moment the
  browser exits, so `login --check` can honestly report dead right
  after a successful run.
- **First logins on a fresh profile may offer different 2FA factors**
  than later ones (unrecognised-device paths). Validate the untrusted
  path deliberately (`--fresh`), not by waiting for natural expiry;
  move a profile aside rather than deleting it.
- **No cleverness in auth flows.** Submit exactly what the human
  entered, surface the provider's own error text verbatim, fail fast.
  Guessed provider policy (code lifetimes, reuse, prefixes) is
  unverifiable and every "clever" retry design has been rejected.

### Browser mechanics

- **The pinned browser stack drops Playwright events across login
  navigations.** A watcher built on `.on(...)` callbacks alone will
  silently hang out the full MFA timeout. Every event dependency needs
  a poll-based twin (route markers, DOM reads), and every wait must
  pump the event loop (`page.wait_for_timeout`, never a bare
  `time.sleep` — sync-Playwright callbacks only fire inside Playwright
  calls). Anything *displayed* to the user — a 2FA comparison code —
  is read from the DOM, never from a network event.
- **A response watcher must match the METHOD, not just the URL.** Any
  endpoint an SPA calls cross-origin is preceded by a CORS **preflight** to
  the same URL, and that `OPTIONS` answers `200` with an EMPTY BODY about a
  second before the real response. A watcher keyed on the URL alone reads
  the preflight as the outcome, and what that costs depends only on where
  it sits: `amex` reported a sign-in refusal the provider never made, the
  same shape in two siblings would have declared a *successful* login
  instead, and in a fourth it sat on a document download, where the empty
  body decoded to no file and failed every row. This is the response-side twin of
  "authenticate on a read, never on a URL" — an OPTIONS response is never an
  outcome.
- **Web components need the real inner control.** Zero-box hosts with
  open shadow roots ignore native `.click()`, dispatched events,
  keyboard, and coordinate clicks; the working click is a real
  Playwright role locator on the rendered inner element (Playwright
  pierces open shadow). When a click mysteriously no-ops, dump the
  frame DOM and shadow skeleton *first* — one ground-truth dump has
  repeatedly settled what ten blind live attempts could not.
- **Login forms hide in iframes, next to decoys** (password-manager
  autofill dictionaries, hidden template duplicates). Fill helpers
  must be frame-aware and origin-gated so credentials never touch a
  third-party frame; pick the candidate that actually renders.
- **Disable the browser password manager before pre-filling** (the
  shared launch helpers in `collectorkit.launch` do this) — a saved
  credential autofills on top of the programmatic fill and doubles the
  field. Fill once, read back to verify; clear OTP fields before any
  retry, or the new code appends to the rejected one.
- **Selectors: stable ids, test-ids, ARIA roles, or class-name
  *prefixes* — never display text (mandatory for non-English UIs) and
  never a full CSS-modules hash** (build hashes rotate on redeploy;
  match the stable prefix). Centralize selectors and routes in one
  module and date observed rot in DESIGN.md.

### Bot defense

- **Escalate a known ladder, and record each rung's result empirically
  — even when the answer is "none".** Vanilla Chromium → Camoufox →
  Camoufox + humanize/geoip + one VNC-seeded human login to plant the
  vendor trust cookie → BYO cookie (the human signs into a stock,
  un-instrumented browser and the tool lifts the session). Akamai-class
  defenses block vanilla Chromium outright at credential submit;
  score-based invisible challenges can flag an automation fingerprint
  with no puzzle to solve, making BYO cookie the only escape.
- **Check which endpoints carry the sensor headers.** Bot defense
  often rides only the login endpoints; data calls need just
  cookie/CSRF — which licenses the browser-for-login + REST
  architecture and much simpler downloads.
- **Logins are budgeted.** Banks rate-limit: a handful of rapid
  attempts has triggered multi-hour fraud holds (pushes silently stop
  arriving; logins stall before the challenge), and about seven sign-ins
  in twenty minutes provoked a captcha at `amex`. Recognize the tell,
  stop unprompted, and let the user time the single retry. Captcha
  reputation typically accrues per account, not per session —
  parallelism does not scale past the gate, and it decays only with
  idle time. **Count the sign-ins a routine run costs**: a `login` →
  `download` pair is two, and a fleet orchestrator runs that pair for
  every source, so the budget goes twice as fast as it looks.
- **A step-up is not one thing.** A logon response saying "more is
  required" may mean an OTP, a push — or a captcha, which no terminal
  can answer. A CLI 2FA drive must recognise the ones it cannot drive
  and stop with the verb that can, instead of timing out against UI
  that will never render.

### Bronze, silver, and data contracts

- **Prefer the SPA's own JSON/GraphQL over CSV/HTML exports** — the
  export is often a lossy client-side rendering of the JSON. When
  several export formats exist, compare them before choosing; formats
  split the useful fields (one carries the stable row id, another the
  running balance — capture both and join in silver). Where the SPA
  signs its requests, drive the real UI and read the SPA's own
  responses instead of replaying endpoints.
- **A tiled overview is not an enumeration.** Where a source offers both
  a rendered overview and a roster endpoint, take the roster: an
  overview tiles what its layout chose to show, which can be a strict
  subset of what the login holds, and a collector keyed on those anchors
  under-collects **silently** — no error, no gap, just fewer accounts
  than exist. One build discovered this only because a hand-driven
  capture recorded the roster response beside the page that had listed
  a fraction of it.
- **Cap captured response bodies generously.** A body truncated at a
  round number loses its tail, and the tail of a ledger response is
  exactly what says how far the history reaches — the one measurement
  that decides whether a backfill phase exists.
- **Key on the most stable identifier the source offers** (immutable
  id, legal name) — display names churn over a multi-year horizon and
  mutable-label keys have caused double-counts. Never derive ids from
  bytes the source regenerates per download (re-rendered PDFs defeat
  byte-level dedup; dedupe on logical identity). Synthesize
  deterministic ids where the source has none.
- **Temporal semantics: snapshot timestamps carry the source's as-of
  time, never fetch time.** Never let a sparse event stream (monthly
  documents, mortgages) mint its own snapshot instants in a
  latest-snapshot-wins world — anchor to existing snapshot dates.
- **Reconciliation is the import gate** for statement parsing:
  beginning + Σ == ending, per segment for multi-product statements,
  with balance-chained attribution that stops on ambiguity. Parse
  every page of every PDF — never sample. Validate extraction with
  non-NULL counts; re-derive every regex after changing text
  normalization.
- **Measure each channel's reach early** (export floor vs statement
  archive floor) — the comparison decides whether a backfill phase
  exists at all, in either direction.
- **Idempotency and self-healing:** re-load is a clean no-op; derived
  silver tables are rebuilt from bronze each load rather than patched
  incrementally (an incremental build can never retire stale rows
  after an identity shift); destructive loaders validate their input
  before touching existing output.
- **Private-market portals differ from banks:** no transaction feed
  (synthesize a balanced double-entry ledger on the custody account —
  each event nets to zero, and the deposit/withdrawal boundary legs are
  the flows the returns policy counts; a separate sentinel account is
  invisible to the value spine), sparse event-dated valuations
  (changes-only, at-most-daily;
  forward-fill in gold; never extrapolate a valuation backward), and
  documents that vanish (portals purge after exits — archive early,
  content-addressed). Expect permanent per-holding authorization
  boundaries; gate on the portal's own access field rather than
  warning on every 403.

### Process hygiene

- **PII discipline is mechanical, not aspirational.** Synthetic values
  from the first draft in every tracked artefact — source, fixtures,
  docstrings, docs, and commit messages; sweep before `git add`, and
  remember `grep` exits 0 on a *match* in `&&` chains. Fixtures are
  synthesized from scratch (transforming real numbers leaks and
  corrupts), format-valid, and obviously fake. Scope prose states
  account *kinds*, never what a login was observed to hold. Tests
  never read real data directories. The agent never pushes.
- **The CLI conventions table is binding** (collectors/README.md).
  Fifteen independently built collectors once drifted into 51 verified
  CLI discrepancies — four names for one timeout, three meanings of
  `--force`, flags that parsed and did nothing — and unifying them
  retroactively cost a 139-file sweep. Wire
  `collectorkit.cli.add_standard_args`/`resolve_standard`, take every
  spelling from the table, and wire every advertised flag or warn
  loudly. Help never starts a container or touches the network; zero
  arguments prints help, never runs the action.
- **Give every stage an exit-early path** (`login --check`,
  `--dry-run`, narrow `--lookback` windows) so iterating on one stage
  never re-pays the stages before it — and make `--dry-run` cover
  every scripted surface, or it cannot reproduce real failures.
- **Wrappers must be re-invocation-safe**: never evict a running
  container that may be hosting a live authenticated session.
- **Commit at milestones and squash.** Work messy, commit clean: one
  or two commits per collector milestone, tests green first, a
  refactor pass before each (dead code, stale comments — especially
  comments that still describe a disproven hypothesis), tight
  WHAT-level messages with no user data. Names are expensive to change
  after the first commit — settle the collector name, env prefix, and
  flag spellings before it.
- **Plan for site drift.** Expect a breaking portal change within
  weeks or months of the build (login form redesigns, minimum-browser
  gates, selector rot). The explore harness is the standing repair
  tool — keep it working forever, and make login failure paths
  self-diagnosing (print the page URL, title, and visible controls) so
  the next drift debugs itself from a pasted log. Print that
  UNCONDITIONALLY, not behind `--debug`: the run that hits the drift is
  rarely the one that thought to ask for a capture. Identifiers only,
  never element text — a challenge screen's labels carry the masked
  destination and the log gets pasted around. Fix a bug in every
  sibling collector that shares the pattern, in the same commit.
